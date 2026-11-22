package fleet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"nixos-fleet-management/api"
)

func storePath(hash byte, name string) string {
	return "/nix/store/" + strings.Repeat(string(hash), 32) + "-" + name
}

func TestDeployer(t *testing.T) {
	running, next := storePath('a', "nixos-system-host"), storePath('b', "nixos-system-host")
	// newRoot has running active and booted, and next built.
	newRoot := func() string {
		root := t.TempDir()
		for _, path := range []string{running, next} {
			if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, path, "nixos-version"), []byte("26.05"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, link := range []string{"run/current-system", "run/booted-system"} {
			if err := os.Symlink(running, filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	root := newRoot()

	type result struct {
		commands []string
		phases   []string
		last     *api.DeployProgress
	}
	// stopAt cancels the agent's context at this command, counting from 1.
	var stopAt int
	var stopAgent context.CancelFunc
	// unit is systemctl show output for the switch unit.
	unit := func(active, sub, result, description string) string {
		return `LoadState=loaded\nActiveState=` + active + `\nSubState=` + sub + `\nResult=` + result + `\nDescription=` + description + `\n`
	}
	notLoaded := `LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nDescription=fleet-agent-switch.service\n`
	runningUnit := func(path string, revision int) string {
		return unit("active", "running", "success", fmt.Sprint(path, " ", revision))
	}
	succeeded := func(path string, revision int) string {
		return unit("active", "exited", "success", fmt.Sprint(path, " ", revision))
	}
	failed := func(path string, revision int) string {
		return unit("failed", "failed", "exit-code", fmt.Sprint(path, " ", revision))
	}
	// deploy offers targets one after another to the same deployer. scripts["systemctl"]
	// answers for fleet-agent-switch, scripts["legacy"] for the legacy unit.
	deploy := func(ctx context.Context, root string, scripts map[string]string, targets ...*api.Deploy) result {
		var r result
		d := newDeployer(func(p *api.DeployProgress) {
			r.phases = append(r.phases, p.Phase.String())
			r.last = p
		})
		d.root = root
		d.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			r.commands = append(r.commands, strings.Join(append([]string{name}, args...), " "))
			if len(r.commands) == stopAt {
				stopAgent()
			}
			key := name
			if name == "systemctl" && args[len(args)-1] == legacySwitchUnit {
				key = "legacy"
			}
			script, ok := scripts[key]
			if !ok && name == "systemctl" {
				script = `printf '` + notLoaded + `'`
			}
			return exec.CommandContext(ctx, "sh", append([]string{"-c", script, name}, args...)...)
		}
		for _, target := range targets {
			d.deploy(ctx, target)
		}
		return r
	}
	at := func(storePath string, revision int64) *api.Deploy {
		return &api.Deploy{StorePath: storePath, Revision: revision}
	}
	ctx := context.Background()
	switchPollInterval = time.Millisecond
	t.Cleanup(func() { switchPollInterval = time.Second })
	const (
		showLegacy  = "systemctl show --property=LoadState,ActiveState,SubState,Result,Description nixos-rebuild-switch-to-configuration"
		show        = "systemctl show --property=LoadState,ActiveState,SubState,Result,Description fleet-agent-switch"
		stopUnit    = "systemctl stop fleet-agent-switch"
		resetFailed = "systemctl reset-failed fleet-agent-switch"
	)
	realise := func(path string) string { return "nix-store --realise " + path }
	setProfile := func(path string) string {
		return "nix-env --profile " + filepath.Join(root, "nix/var/nix/profiles/system") + " --set " + path
	}
	switchTo := func(path string, revision int) string {
		return "systemd-run -E LOCALE_ARCHIVE -E NIXOS_INSTALL_BOOTLOADER=0 -E NIXOS_NO_CHECK --no-ask-password --quiet" +
			" --service-type=exec --property=RemainAfterExit=yes --unit=fleet-agent-switch --description=" + path + " " + strconv.Itoa(revision) +
			" " + path + "/bin/switch-to-configuration switch"
	}
	awaited := []string{showLegacy, show}
	// switching lists the commands of a switch, removing the previous unit with remove.
	switching := func(path string, revision int, remove ...string) []string {
		return slices.Concat(awaited, []string{realise(path), setProfile(path)}, remove, []string{switchTo(path, revision), show})
	}
	// polls answers the n-th systemctl show with the n-th of units, then with the last.
	polls := func(units ...string) map[string]string {
		counter := filepath.Join(t.TempDir(), "polls")
		script := `[ "$1" = show ] || exit 0; echo >> ` + counter + `; case $(wc -l < ` + counter + `) in`
		for i, u := range units {
			pattern := strconv.Itoa(i + 1)
			if i == len(units)-1 {
				pattern = "*"
			}
			script += ` ` + pattern + `) printf '` + u + `';;`
		}
		return map[string]string{"systemctl": script + ` esac`}
	}
	recordFile := filepath.Join(root, "run/fleet-agent-switch")
	setRecord := func(record string) {
		t.Helper()
		if err := os.Remove(recordFile); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if record != "" {
			if err := os.WriteFile(recordFile, []byte(record+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	record := func(path string, revision int, result string) string {
		return fmt.Sprint(path, " ", revision, " ", result)
	}
	check := func(name string, r result, commands, phases []string, wantRecord string) {
		t.Helper()
		if !slices.Equal(r.commands, commands) {
			t.Errorf("%s: commands:\n%s\nwant:\n%s", name, strings.Join(r.commands, "\n"), strings.Join(commands, "\n"))
		}
		if !slices.Equal(r.phases, phases) {
			t.Errorf("%s: phases %v, want %v", name, r.phases, phases)
		}
		if data, _ := os.ReadFile(recordFile); strings.TrimSpace(string(data)) != wantRecord {
			t.Errorf("%s: record %q, want %q", name, data, wantRecord)
		}
	}
	switched := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}
	switchFailed := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_FAILED"}
	interrupted := []string{"PHASE_REALISING", "PHASE_ACTIVATING"}
	done := []string{"PHASE_DONE"}
	failure := []string{"PHASE_FAILED"}

	setRecord("")
	scripts := polls(notLoaded, succeeded(next, 1))
	scripts["nix-store"] = "echo copying path 1; echo copying path 2"
	r := deploy(ctx, root, scripts, at(next, 1))
	check("switch", r, switching(next, 1), switched, record(next, 1, "starting"))
	if r.last.StorePath != next || r.last.Revision != 1 {
		t.Errorf("progress for %s at %d, want %s at 1", r.last.StorePath, r.last.Revision, next)
	}

	setRecord("")
	r = deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"}, at(next, 1))
	check("failed realise", r, append(awaited, realise(next)), []string{"PHASE_REALISING", "PHASE_FAILED"}, "")
	if !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: message %q", r.last.Message)
	}

	setRecord("")
	r = deploy(ctx, root, polls(notLoaded, failed(next, 1)), at(next, 1))
	check("failed switch", r, switching(next, 1), switchFailed, record(next, 1, "starting"))
	if !strings.Contains(r.last.Message, "journalctl -u fleet-agent-switch") {
		t.Errorf("failed switch: message %q", r.last.Message)
	}

	setRecord("")
	scripts = polls(notLoaded)
	scripts["systemd-run"] = "echo 'Failed to start transient service unit' >&2; exit 1"
	r = deploy(ctx, root, scripts, at(next, 7))
	check("switch failing to start", r, slices.Concat(awaited, []string{realise(next), setProfile(next), switchTo(next, 7)}), switchFailed, record(next, 7, "start-failed"))
	if !strings.Contains(r.last.Message, "Failed to start transient service unit") {
		t.Errorf("switch failing to start: message %q", r.last.Message)
	}

	// The unit of the previous switch is awaited and removed, its result kept in the record.
	setRecord("")
	stopped, stop := context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = 6, stop
	r = deploy(stopped, root, polls(runningUnit(running, 1), failed(running, 1)), at(next, 2))
	stopAt = 0
	check("previous failed switch", r, []string{showLegacy, show, show, realise(next), setProfile(next), resetFailed}, interrupted, record(running, 1, "exit-code"))
	setRecord("")
	r = deploy(ctx, root, polls(succeeded(running, 1), succeeded(next, 2)), at(next, 2))
	check("previous successful switch", r, switching(next, 2, stopUnit), switched, record(next, 2, "starting"))

	// Stopped while its switch runs, the agent reports nothing; restarted, it
	// reports how the switch ended without switching again.
	setRecord("")
	stopped, stop = context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = 6, stop
	r = deploy(stopped, root, polls(notLoaded, runningUnit(next, 6)), at(next, 6))
	stopAt = 0
	check("stopped agent", r, switching(next, 6), interrupted, record(next, 6, "starting"))
	activeRoot := newRoot()
	if err := os.Remove(filepath.Join(activeRoot, "run/current-system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(next, filepath.Join(activeRoot, "run/current-system")); err != nil {
		t.Fatal(err)
	}
	r = deploy(ctx, activeRoot, polls(succeeded(next, 6)), at(next, 6))
	if !slices.Equal(r.commands, awaited) || !slices.Equal(r.phases, done) {
		t.Errorf("succeeded while restarting: commands %v, phases %v", r.commands, r.phases)
	}
	r = deploy(ctx, activeRoot, polls(failed(next, 6)), at(next, 6))
	if !slices.Equal(r.commands, awaited) || !slices.Equal(r.phases, failure) || !strings.Contains(r.last.Message, "exit-code") {
		t.Errorf("failed while restarting, resent: commands %v, phases %v, message %q", r.commands, r.phases, r.last.Message)
	}
	// Restarted before the unit started, it switches.
	r = deploy(ctx, root, polls(notLoaded, succeeded(next, 6)), at(next, 6))
	check("restarted before the switch started", r, switching(next, 6), switched, record(next, 6, "starting"))

	// The agent restarted by its switch waits for it and reports its failure.
	setRecord(record(running, 5, "starting"))
	r = deploy(ctx, root, polls(runningUnit(running, 5), failed(running, 5)), at(running, 5))
	check("switch failing after agent restart", r, []string{showLegacy, show, show}, failure, record(running, 5, "starting"))

	// Resent, a failed switch is reported, whether from its unit or the record;
	// assigned again, or by a server not sending revisions, it is retried.
	for name, tc := range map[string]struct {
		record, unit string
		revision     int
	}{
		"failed unit":              {"", failed(running, 5), 5},
		"recorded failure":         {record(running, 5, "exit-code"), notLoaded, 5},
		"recorded start failure":   {record(running, 5, "start-failed"), notLoaded, 5},
		"server without revisions": {"", failed(running, 0), 0},
	} {
		if tc.revision != 0 {
			setRecord(tc.record)
			r = deploy(ctx, root, polls(tc.unit), at(running, int64(tc.revision)))
			check(name+", resent", r, awaited, failure, tc.record)
		}
		var remove []string
		if tc.unit != notLoaded {
			remove = []string{resetFailed}
		}
		assigned := tc.revision + 1
		if tc.revision == 0 {
			assigned = 0
		}
		setRecord(tc.record)
		r = deploy(ctx, root, polls(tc.unit, succeeded(running, assigned)), at(running, int64(assigned)))
		check(name+", assigned again", r, switching(running, assigned, remove...), switched, record(running, assigned, "starting"))
	}

	// The active store path counts as switched to only after a successful switch
	// to it, or booted with none since.
	for name, tc := range map[string]struct {
		record, unit string
		switches     bool
	}{
		"booted, nothing switched":        {"", notLoaded, false},
		"switched to":                     {"", succeeded(running, 1), false},
		"switched to, unit removed":       {record(running, 1, "success"), notLoaded, false},
		"another switch failed after it":  {"", failed(next, 2), true},
		"another switch started after it": {record(next, 2, "starting"), notLoaded, true},
	} {
		setRecord(tc.record)
		r = deploy(ctx, root, polls(tc.unit, succeeded(running, 3)), at(running, 3))
		if tc.switches {
			if !slices.Equal(r.phases, switched) {
				t.Errorf("active, %s: phases %v, want a switch", name, r.phases)
			}
		} else if !slices.Equal(r.commands, awaited) || !slices.Equal(r.phases, done) || r.last.Message != "already active" {
			t.Errorf("active, %s: commands %v, phases %v, message %q", name, r.commands, r.phases, r.last.Message)
		}
	}

	// Switched to since boot without the agent, the active store path is switched again.
	bootedRoot := newRoot()
	if err := os.Remove(filepath.Join(bootedRoot, "run/booted-system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(next, filepath.Join(bootedRoot, "run/booted-system")); err != nil {
		t.Fatal(err)
	}
	if r = deploy(ctx, bootedRoot, polls(notLoaded, succeeded(running, 3)), at(running, 3)); !slices.Equal(r.phases, switched) {
		t.Errorf("active, switched to since boot: phases %v, want a switch", r.phases)
	}

	// The result of a switch in the legacy unit is lost, so the agent switches again.
	setRecord("")
	scripts = polls(notLoaded, succeeded(running, 1))
	scripts["legacy"] = polls(runningUnit("x", 0), notLoaded)["systemctl"]
	r = deploy(ctx, root, scripts, at(running, 1))
	check("legacy switch", r, append([]string{showLegacy}, switching(running, 1)...), switched, record(running, 1, "starting"))

	// The agent restarted by a switch waits for it before reading the active store path.
	switchingRoot := newRoot()
	activated := filepath.Join(t.TempDir(), "activated")
	r = deploy(ctx, switchingRoot, map[string]string{"systemctl": `[ "$1" = show ] || exit 0
		if [ -e ` + activated + ` ]; then printf '` + succeeded(next, 1) + `'
		else touch ` + activated + `; ln -sfn ` + next + ` ` + filepath.Join(switchingRoot, "run/current-system") + `
			printf '` + runningUnit(next, 1) + `'; fi`}, at(next, 1))
	if !slices.Equal(r.commands, []string{showLegacy, show, show}) || !slices.Equal(r.phases, done) {
		t.Errorf("switch activating while waiting: commands %v, phases %v", r.commands, r.phases)
	}

	for _, tc := range []struct {
		name, root, path, message string
		commands                  int
	}{
		{"invalid store path", root, "/nix/store/x; reboot", "invalid store path", 0},
		{"not NixOS", t.TempDir(), next, "not a NixOS host", 2},
	} {
		r := deploy(ctx, tc.root, nil, at(tc.path, 1))
		if len(r.commands) != tc.commands || len(r.phases) != 1 || !strings.Contains(r.last.Message, tc.message) {
			t.Errorf("%s: commands %v, phases %v, message %q", tc.name, r.commands, r.phases, r.last.Message)
		}
	}
}

func TestLatestKeepsNewestValue(t *testing.T) {
	l := make(latest[int], 1)
	l.offer(1)
	l.offer(2)
	if v := <-l; v != 2 {
		t.Fatalf("got %d, want 2", v)
	}
}
