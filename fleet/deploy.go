package fleet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
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
	// switchRecordFile holds the last switch while its unit does not: before it
	// starts, when it fails to start, and once the next switch removed the unit.
	// In /run, it lasts until reboot like the unit.
	switchRecordFile = "run/fleet-agent-switch"
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
	progress := newProgress(target, d.report)
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
	_, legacyWaited, err := d.awaitUnit(ctx, legacySwitchUnit)
	if err != nil {
		fail(err)
		return
	}
	unit, _, err := d.awaitUnit(ctx, switchUnit)
	if err != nil {
		fail(err)
		return
	}
	last := unit
	if !unit.loaded {
		if last, err = d.readRecord(); err != nil {
			fail(err)
			return
		}
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
	// Activation sets /run/current-system before the switch finishes, so the active
	// store path counts only once its switch succeeded, or as booted with no switch since.
	switchedTo := last.storePath == storePath && last.result == "success" ||
		last == (switchRecord{}) && generation.BootedStorePath == storePath
	switch {
	case last.storePath == storePath && last.failed() && last.revision != 0 && last.revision == target.Revision:
		// Resent after a reconnect rather than assigned again.
		progress.phase(api.DeployProgress_PHASE_FAILED, last.failure())
		return
	case generation.StorePath == storePath && switchedTo && !legacyWaited:
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
	if unit.loaded {
		// The record keeps the result that removing the unit erases.
		if err := d.writeRecord(unit); err != nil {
			fail(err)
			return
		}
		remove := "stop"
		if unit.failed() {
			remove = "reset-failed"
		}
		if err := d.exec(ctx, progress, "systemctl", remove, switchUnit); err != nil {
			fail(err)
			return
		}
	}
	if err := d.writeRecord(switchRecord{storePath: storePath, revision: target.Revision, result: "starting"}); err != nil {
		fail(err)
		return
	}
	// A separate unit, as in nixos-rebuild: the switch may restart this agent and
	// must finish anyway. Its output goes to the journal of that unit.
	err = d.exec(ctx, progress, "systemd-run",
		"-E", "LOCALE_ARCHIVE", "-E", "NIXOS_INSTALL_BOOTLOADER=0", "-E", "NIXOS_NO_CHECK",
		"--no-ask-password", "--quiet", "--service-type=exec", "--property=RemainAfterExit=yes",
		"--unit="+switchUnit, fmt.Sprintf("--description=%s %d", storePath, target.Revision),
		storePath+"/bin/switch-to-configuration", "switch")
	if err != nil {
		if ctx.Err() == nil {
			if err := d.writeRecord(switchRecord{storePath: storePath, revision: target.Revision, result: "start-failed"}); err != nil {
				log.Print(err)
			}
		}
		fail(err)
		return
	}
	unit, _, err = d.awaitUnit(ctx, switchUnit)
	if err != nil {
		fail(err)
		return
	}
	if unit != (switchRecord{storePath: storePath, revision: target.Revision, result: "success", loaded: true}) {
		if !unit.failed() {
			unit.result = "stopped"
		}
		fail(errors.New(unit.failure()))
		return
	}
	progress.phase(api.DeployProgress_PHASE_DONE, "")
	log.Printf("deployed %s", storePath)
}

// switchRecord is a switch and how it ended. With RemainAfterExit and without
// --collect, its unit stays loaded with store path and revision as description
// until the next switch removes it.
type switchRecord struct {
	storePath string
	revision  int64
	// result is starting, success, start-failed or the result of a failed unit,
	// such as exit-code.
	result string
	loaded bool
}

func (r switchRecord) failed() bool {
	return r.result != "" && r.result != "starting" && r.result != "success"
}

func (r switchRecord) failure() string {
	return fmt.Sprintf("switch failed (%s), see journalctl -u %s", r.result, switchUnit)
}

func (d *deployer) readRecord() (switchRecord, error) {
	path := filepath.Join(d.root, switchRecordFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return switchRecord{}, nil
	}
	if err != nil {
		return switchRecord{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return switchRecord{}, fmt.Errorf("%s: malformed switch record %q", path, data)
	}
	revision, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return switchRecord{}, fmt.Errorf("%s: %w", path, err)
	}
	return switchRecord{storePath: fields[0], revision: revision, result: fields[2]}, nil
}

func (d *deployer) writeRecord(record switchRecord) error {
	path := filepath.Join(d.root, switchRecordFile)
	data := fmt.Sprintf("%s %d %s\n", record.storePath, record.revision, record.result)
	if err := os.WriteFile(path+".tmp", []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// awaitUnit waits for a running switch in unit and returns how it ended, and
// whether it had to wait.
func (d *deployer) awaitUnit(ctx context.Context, unit string) (switchRecord, bool, error) {
	for waited := false; ; waited = true {
		output, err := d.command(ctx, "systemctl", "show", "--property=LoadState,ActiveState,SubState,Result,Description", unit).Output()
		if err != nil {
			return switchRecord{}, waited, fmt.Errorf("systemctl show %s: %w", unit, err)
		}
		properties := map[string]string{}
		for line := range strings.Lines(string(output)) {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			properties[key] = value
		}
		record := switchRecord{loaded: properties["LoadState"] == "loaded"}
		var revision string
		record.storePath, revision, _ = strings.Cut(properties["Description"], " ")
		record.revision, _ = strconv.ParseInt(revision, 10, 64)
		switch state := properties["ActiveState"]; {
		case state == "failed":
			record.result = properties["Result"]
			return record, waited, nil
		case state == "active" && properties["SubState"] == "exited":
			record.result = "success"
			return record, waited, nil
		case state == "inactive":
			return switchRecord{}, waited, nil
		}
		select {
		case <-ctx.Done():
			return switchRecord{}, waited, ctx.Err()
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
	target  *api.Deploy
	current api.DeployProgress_Phase
	sentAt  time.Time
	report  func(*api.DeployProgress)
}

func newProgress(target *api.Deploy, report func(*api.DeployProgress)) *progress {
	return &progress{target: target, report: report}
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
	p.report(&api.DeployProgress{StorePath: p.target.StorePath, Revision: p.target.Revision, Phase: p.current, Message: message})
}
