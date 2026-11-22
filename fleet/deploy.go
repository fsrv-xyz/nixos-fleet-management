package fleet

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"nixos-fleet-management/api"
)

const (
	progressInterval = time.Second
	switchUnit       = "fleet-agent-switch"
	// legacySwitchUnit ran switches of earlier agents and of nixos-rebuild; it is
	// collected, so the result of a switch in it is unknown.
	legacySwitchUnit = "nixos-rebuild-switch-to-configuration"
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
	desired latest[*api.Deploy]
}

func newDeployer(report func(*api.DeployProgress)) *deployer {
	return &deployer{root: "/", command: exec.CommandContext, report: report, desired: make(latest[*api.Deploy], 1)}
}

func (d *deployer) offer(target *api.Deploy) {
	d.desired.offer(target)
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
		case target := <-d.desired:
			d.deploy(ctx, target)
		}
	}
}

func (d *deployer) deploy(ctx context.Context, target *api.Deploy) {
	storePath := target.StorePath
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
	// A running switch may still change the active store path, so wait for it first.
	legacy, err := d.awaitUnit(ctx, legacySwitchUnit)
	if err != nil {
		fail(err)
		return
	}
	last, err := d.awaitUnit(ctx, switchUnit)
	if err != nil {
		fail(err)
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
	}
	recordedFailure := last.storePath == storePath && last.failed()
	switch {
	case recordedFailure && last.revision != 0 && last.revision == target.Revision:
		// Resent after a reconnect rather than assigned again.
		progress.phase(api.DeployProgress_PHASE_FAILED, last.failure())
		return
	// Activation sets /run/current-system before the switch restarts this agent
	// and finishes, and the legacy unit keeps no result.
	case generation.StorePath == storePath && !recordedFailure && !legacy.waited:
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
	if last.failed() {
		log.Printf("previous switch to %s failed (%s)", last.storePath, last.result)
		if err := d.exec(ctx, progress, "systemctl", "reset-failed", switchUnit); err != nil {
			fail(err)
			return
		}
	}
	// A separate unit, as in nixos-rebuild: the switch may restart this agent and
	// must finish anyway. Its output goes to the journal of that unit.
	err = d.exec(ctx, progress, "systemd-run",
		"-E", "LOCALE_ARCHIVE", "-E", "NIXOS_INSTALL_BOOTLOADER=0", "-E", "NIXOS_NO_CHECK",
		"--no-ask-password", "--quiet", "--service-type=exec",
		"--unit="+switchUnit, fmt.Sprintf("--description=%s %d", storePath, target.Revision), "--wait",
		storePath+"/bin/switch-to-configuration", "switch")
	if err != nil {
		fail(fmt.Errorf("%w, see journalctl -u %s", err, switchUnit))
		return
	}
	progress.phase(api.DeployProgress_PHASE_DONE, "")
	log.Printf("deployed %s", storePath)
}

// unitState is how the last switch in a unit ended. Without --collect, the unit
// of a failed switch stays loaded, its description naming store path and
// revision, until the next switch resets it; a successful one is unloaded.
type unitState struct {
	storePath, result string
	revision          int64
	waited            bool
}

func (s unitState) failed() bool {
	return s.result != "success"
}

func (s unitState) failure() string {
	return fmt.Sprintf("switch failed (%s), see journalctl -u %s", s.result, switchUnit)
}

// awaitUnit waits for a running switch in unit and returns how it ended.
func (d *deployer) awaitUnit(ctx context.Context, unit string) (unitState, error) {
	for waited := false; ; waited = true {
		output, err := d.command(ctx, "systemctl", "show", "--property=ActiveState,Result,Description", unit).Output()
		if err != nil {
			return unitState{}, fmt.Errorf("systemctl show %s: %w", unit, err)
		}
		state := unitState{waited: waited}
		var active string
		for line := range strings.Lines(string(output)) {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch key {
			case "ActiveState":
				active = value
			case "Result":
				state.result = value
			case "Description":
				var revision string
				state.storePath, revision, _ = strings.Cut(value, " ")
				state.revision, _ = strconv.ParseInt(revision, 10, 64)
			}
		}
		if active == "inactive" || active == "failed" {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return unitState{}, ctx.Err()
		case <-time.After(switchPollInterval):
		}
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
