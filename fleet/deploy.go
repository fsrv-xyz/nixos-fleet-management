package fleet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"nixos-fleet-management/api"
)

const (
	progressInterval = time.Second
	switchUnit       = "nixos-rebuild-switch-to-configuration"
)

var switchPollInterval = time.Second

// Nix base32 hash (no e, o, t, u) and the characters Nix allows in store path names.
var storePathPattern = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[0-9a-zA-Z+\-._?=]+$`)

func validateDeploy(certname, storePath string) error {
	if !certnamePattern.MatchString(certname) {
		return fmt.Errorf("invalid certname %q", certname)
	}
	if !storePathPattern.MatchString(storePath) {
		return fmt.Errorf("invalid store path %q", storePath)
	}
	return nil
}

// SetDesired makes the agent with certname switch to storePath and returns the
// etcd revision of that change.
func SetDesired(ctx context.Context, etcd *clientv3.Client, certname, storePath string) (int64, error) {
	if err := validateDeploy(certname, storePath); err != nil {
		return 0, err
	}
	resp, err := etcd.Put(ctx, agentsPrefix+certname+"/desired", storePath)
	if err != nil {
		return 0, err
	}
	return resp.Header.Revision, nil
}

// deployer switches the host to the latest offered store path, one deployment
// at a time, the way nixos-rebuild switch does.
type deployer struct {
	root    string
	command func(ctx context.Context, name string, args ...string) *exec.Cmd
	report  func(*api.DeployProgress)
	desired latest[string]
}

func newDeployer(report func(*api.DeployProgress)) *deployer {
	return &deployer{root: "/", command: exec.CommandContext, report: report, desired: make(latest[string], 1)}
}

func (d *deployer) offer(storePath string) {
	d.desired.offer(storePath)
}

// latest is a channel of capacity 1 whose pending value a newer offer replaces,
// so offering never blocks and the receiver only gets the most recent value.
type latest[T any] chan T

func (l latest[T]) offer(value T) {
	for {
		select {
		case l <- value:
			return
		default:
		}
		select {
		case <-l:
		default:
		}
	}
}

func (d *deployer) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case storePath := <-d.desired:
			d.deploy(ctx, storePath)
		}
	}
}

func (d *deployer) deploy(ctx context.Context, storePath string) {
	progress := newProgress(storePath, d.report)
	// Stopping the agent kills its commands; the restarted agent reports the outcome.
	fail := func(err error) {
		if ctx.Err() == nil {
			progress.phase(api.DeployProgress_PHASE_FAILED, err.Error())
		}
	}
	if !storePathPattern.MatchString(storePath) {
		progress.phase(api.DeployProgress_PHASE_FAILED, fmt.Sprintf("invalid store path %q", storePath))
		return
	}
	generation, err := nixosGeneration(d.root)
	switch {
	case err != nil:
		progress.phase(api.DeployProgress_PHASE_FAILED, err.Error())
		return
	case generation == nil:
		progress.phase(api.DeployProgress_PHASE_FAILED, "not a NixOS host")
		return
	case generation.StorePath == storePath:
		// Activation sets /run/current-system before the switch restarts this
		// agent and finishes, so only the switch unit knows whether it succeeded.
		if err := d.switchResult(ctx); err != nil {
			fail(err)
			return
		}
		progress.phase(api.DeployProgress_PHASE_DONE, "already active")
		return
	}

	log.Printf("deploying %s", storePath)
	progress.phase(api.DeployProgress_PHASE_REALISING, "")
	if err := d.exec(ctx, progress, "nix-store", "--realise", storePath); err != nil {
		fail(err)
		return
	}
	progress.phase(api.DeployProgress_PHASE_ACTIVATING, "")
	if err := d.exec(ctx, progress, "nix-env", "--profile", filepath.Join(d.root, systemProfiles, "system"), "--set", storePath); err != nil {
		fail(err)
		return
	}
	// A separate unit, as in nixos-rebuild: the switch may restart this agent and
	// must finish anyway. Its output goes to the journal of that unit. Without
	// --collect, a failed unit stays loaded, so a restarted agent can read its result.
	err = d.exec(ctx, progress, "systemd-run",
		"-E", "LOCALE_ARCHIVE", "-E", "NIXOS_INSTALL_BOOTLOADER=0", "-E", "NIXOS_NO_CHECK",
		"--no-ask-password", "--quiet", "--service-type=exec",
		"--unit="+switchUnit, "--wait",
		storePath+"/bin/switch-to-configuration", "switch")
	if err != nil {
		fail(err)
		d.resetSwitch(ctx)
		return
	}
	progress.phase(api.DeployProgress_PHASE_DONE, "")
	log.Printf("deployed %s", storePath)
}

// switchResult waits for a running switch and returns its failure, resetting the
// failed unit so the next switch can reuse its name.
func (d *deployer) switchResult(ctx context.Context) error {
	for {
		output, err := d.command(ctx, "systemctl", "show", "--property=ActiveState,Result", switchUnit).Output()
		if err != nil {
			return fmt.Errorf("systemctl show %s: %w", switchUnit, err)
		}
		var state, result string
		for line := range strings.Lines(string(output)) {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch key {
			case "ActiveState":
				state = value
			case "Result":
				result = value
			}
		}
		if state == "inactive" || state == "failed" {
			if result == "success" {
				return nil
			}
			d.resetSwitch(ctx)
			return fmt.Errorf("switch failed (%s), see journalctl -u %s", result, switchUnit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(switchPollInterval):
		}
	}
}

func (d *deployer) resetSwitch(ctx context.Context) {
	output, err := d.command(ctx, "systemctl", "reset-failed", switchUnit).CombinedOutput()
	if err != nil && ctx.Err() == nil {
		log.Printf("systemctl reset-failed %s: %v: %s", switchUnit, err, bytes.TrimSpace(output))
	}
}

// exec reports each output line of the command as progress and returns an error
// naming its last line if it fails.
func (d *deployer) exec(ctx context.Context, progress *progress, name string, args ...string) error {
	cmd := d.command(ctx, name, args...)
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	var last string
	lines := bufio.NewScanner(output)
	for lines.Scan() {
		last = lines.Text()
		progress.line(last)
	}
	if err := cmd.Wait(); err != nil {
		if last != "" {
			return fmt.Errorf("%s: %w: %s", name, err, last)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// progress sends phase changes at once and output lines at most once per
// progressInterval, so a chatty command does not turn into a write per line.
type progress struct {
	storePath string
	current   api.DeployProgress_Phase
	sentAt    time.Time
	report    func(*api.DeployProgress)
}

func newProgress(storePath string, report func(*api.DeployProgress)) *progress {
	return &progress{storePath: storePath, report: report}
}

func (p *progress) phase(phase api.DeployProgress_Phase, message string) {
	p.current = phase
	p.send(message)
}

func (p *progress) line(message string) {
	if time.Since(p.sentAt) >= progressInterval {
		p.send(message)
	}
}

func (p *progress) send(message string) {
	p.sentAt = time.Now()
	p.report(&api.DeployProgress{StorePath: p.storePath, Phase: p.current, Message: message})
}
