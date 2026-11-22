package fleet

import (
	"bufio"
	"cmp"
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
	// switchRecordFile holds the last switch; in /run, it lasts until reboot like a failed unit.
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
	last, err := d.lastSwitch(unit)
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
	failedBefore := last.storePath == storePath && last.failed()
	switch {
	case failedBefore && last.revision != 0 && last.revision == target.Revision:
		// Resent after a reconnect rather than assigned again.
		progress.phase(api.DeployProgress_PHASE_FAILED, last.failure())
		return
	// Activation sets /run/current-system before the switch restarts this agent
	// and finishes, and the legacy unit keeps no result.
	case generation.StorePath == storePath && !failedBefore && !legacyWaited:
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
	if unit.failed() {
		log.Printf("previous switch to %s failed (%s)", unit.storePath, unit.result)
		// The record keeps the failure that the reset erases from the unit.
		if err := d.writeRecord(unit); err != nil {
			fail(err)
			return
		}
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
		"--unit="+switchUnit, fmt.Sprintf("--description=%s %d", storePath, target.Revision),
		storePath+"/bin/switch-to-configuration", "switch")
	if err != nil {
		if ctx.Err() == nil {
			if err := d.writeRecord(switchRecord{storePath, target.Revision, "start-failed"}); err != nil {
				log.Print(err)
			}
		}
		fail(err)
		return
	}
	// Written only once the unit runs, so a running record without a loaded unit
	// means the switch succeeded.
	if err := d.writeRecord(switchRecord{storePath, target.Revision, "running"}); err != nil {
		fail(err)
		return
	}
	unit, _, err = d.awaitUnit(ctx, switchUnit)
	if err != nil {
		fail(err)
		return
	}
	done := switchRecord{storePath, target.Revision, cmp.Or(unit.result, "success")}
	if err := d.writeRecord(done); err != nil {
		fail(err)
		return
	}
	if done.failed() {
		fail(errors.New(done.failure()))
		return
	}
	progress.phase(api.DeployProgress_PHASE_DONE, "")
	log.Printf("deployed %s", storePath)
}

// switchRecord is a switch and how it ended. The agent records each switch it
// starts. Without --collect, the unit of a failed switch stays loaded with store
// path and revision as description until the next switch resets it, while a
// successful one is unloaded.
type switchRecord struct {
	storePath string
	revision  int64
	// result is running, success or why the switch failed, such as the unit result exit-code.
	result string
}

func (r switchRecord) failed() bool {
	return r.result != "" && r.result != "running" && r.result != "success"
}

func (r switchRecord) failure() string {
	return fmt.Sprintf("switch failed (%s), see journalctl -u %s", r.result, switchUnit)
}

// lastSwitch returns the last switch, given the ended state of the switch unit.
func (d *deployer) lastSwitch(unit switchRecord) (switchRecord, error) {
	if unit.failed() {
		return unit, nil
	}
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
	return switchRecord{fields[0], revision, fields[2]}, nil
}

func (d *deployer) writeRecord(record switchRecord) error {
	path := filepath.Join(d.root, switchRecordFile)
	data := fmt.Sprintf("%s %d %s\n", record.storePath, record.revision, record.result)
	if err := os.WriteFile(path+".tmp", []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// awaitUnit waits for a running switch in unit and returns it with a result
// only if it failed, and whether it had to wait.
func (d *deployer) awaitUnit(ctx context.Context, unit string) (switchRecord, bool, error) {
	for waited := false; ; waited = true {
		output, err := d.command(ctx, "systemctl", "show", "--property=ActiveState,Result,Description", unit).Output()
		if err != nil {
			return switchRecord{}, waited, fmt.Errorf("systemctl show %s: %w", unit, err)
		}
		var state, result string
		var record switchRecord
		for line := range strings.Lines(string(output)) {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch key {
			case "ActiveState":
				state = value
			case "Result":
				result = value
			case "Description":
				var revision string
				record.storePath, revision, _ = strings.Cut(value, " ")
				record.revision, _ = strconv.ParseInt(revision, 10, 64)
			}
		}
		switch state {
		case "failed":
			record.result = result
			return record, waited, nil
		case "inactive":
			return record, waited, nil
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
