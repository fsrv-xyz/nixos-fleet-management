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
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, running), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, running, "nixos-version"), []byte("26.05"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, filepath.Join(root, "run/current-system")); err != nil {
		t.Fatal(err)
	}

	type result struct {
		commands []string
		phases   []string
		last     *api.DeployProgress
	}
	// stopAt cancels the agent's context at this command, counting from 1.
	var stopAt int
	var stopAgent context.CancelFunc
	// unit is systemctl show output for a switch unit.
	unit := func(state, result, description string) string {
		return `ActiveState=` + state + `\nResult=` + result + `\nDescription=` + description + `\n`
	}
	notLoaded := unit("inactive", "success", "fleet-agent-switch.service")
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
		showLegacy  = "systemctl show --property=ActiveState,Result,Description nixos-rebuild-switch-to-configuration"
		show        = "systemctl show --property=ActiveState,Result,Description fleet-agent-switch"
		resetFailed = "systemctl reset-failed fleet-agent-switch"
	)
	realise := func(path string) string { return "nix-store --realise " + path }
	setProfile := func(path string) string {
		return "nix-env --profile " + filepath.Join(root, "nix/var/nix/profiles/system") + " --set " + path
	}
	switchTo := func(path string, revision int) string {
		return "systemd-run -E LOCALE_ARCHIVE -E NIXOS_INSTALL_BOOTLOADER=0 -E NIXOS_NO_CHECK --no-ask-password --quiet" +
			" --service-type=exec --unit=fleet-agent-switch --description=" + path + " " + strconv.Itoa(revision) +
			" " + path + "/bin/switch-to-configuration switch"
	}
	switching := func(path string, revision int) []string {
		return []string{realise(path), setProfile(path), switchTo(path, revision), show}
	}
	// polls answers the n-th systemctl show with the n-th of units, then with the last.
	polls := func(units ...string) string {
		counter := filepath.Join(t.TempDir(), "polls")
		script := `[ "$1" = show ] || exit 0; echo >> ` + counter + `; case $(wc -l < ` + counter + `) in`
		for i, u := range units {
			pattern := strconv.Itoa(i + 1)
			if i == len(units)-1 {
				pattern = "*"
			}
			script += ` ` + pattern + `) printf '` + u + `';;`
		}
		return script + ` esac`
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
	check := func(name string, r result, commands, phases []string, record string) {
		t.Helper()
		if !slices.Equal(r.commands, commands) {
			t.Errorf("%s: commands:\n%s\nwant:\n%s", name, strings.Join(r.commands, "\n"), strings.Join(commands, "\n"))
		}
		if !slices.Equal(r.phases, phases) {
			t.Errorf("%s: phases %v, want %v", name, r.phases, phases)
		}
		if data, _ := os.ReadFile(recordFile); strings.TrimSpace(string(data)) != record {
			t.Errorf("%s: record %q, want %q", name, data, record)
		}
	}
	switched := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}
	switchFailed := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_FAILED"}
	with := func(commands ...[]string) []string { return slices.Concat(commands...) }
	awaited := []string{showLegacy, show}

	setRecord("")
	r := deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo copying path 2"}, at(next, 1))
	check("switch", r, with(awaited, switching(next, 1)), switched, next+" 1 success")
	if r.last.StorePath != next || r.last.Revision != 1 {
		t.Errorf("progress for %s at %d, want %s at 1", r.last.StorePath, r.last.Revision, next)
	}

	setRecord("")
	r = deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"}, at(next, 1))
	check("failed realise", r, with(awaited, []string{realise(next)}), []string{"PHASE_REALISING", "PHASE_FAILED"}, "")
	if !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: message %q", r.last.Message)
	}

	setRecord("")
	r = deploy(ctx, root, map[string]string{"systemctl": polls(notLoaded, unit("failed", "exit-code", next+" 1"))}, at(next, 1))
	check("failed switch", r, with(awaited, switching(next, 1)), switchFailed, next+" 1 exit-code")
	if !strings.Contains(r.last.Message, "journalctl -u fleet-agent-switch") {
		t.Errorf("failed switch: message %q", r.last.Message)
	}

	// A switch the agent did not see end is awaited and reset before the next one, without blaming it.
	setRecord("")
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("active", "success", running+" 1"), unit("failed", "exit-code", running+" 1"), notLoaded)}, at(next, 2))
	check("previous switch", r, []string{showLegacy, show, show, realise(next), setProfile(next), resetFailed, switchTo(next, 2), show}, switched, next+" 2 success")

	// Stopped while a retry runs, the agent leaves it running and reports nothing.
	setRecord(running + " 5 exit-code")
	stopped, stop := context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = 6, stop
	r = deploy(stopped, root, map[string]string{"systemctl": polls(notLoaded, unit("active", "success", running+" 6"))}, at(running, 6))
	stopAt = 0
	check("stopped agent", r, with(awaited, switching(running, 6)), []string{"PHASE_REALISING", "PHASE_ACTIVATING"}, running+" 6 running")
	// Restarted, it finds the switch it started ended: successful as its unit is gone, or failed.
	r = deploy(ctx, root, nil, at(running, 6))
	check("succeeded while restarting", r, awaited, []string{"PHASE_DONE"}, running+" 6 running")
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", running+" 6"))}, at(running, 6))
	check("failed while restarting, resent", r, awaited, []string{"PHASE_FAILED"}, running+" 6 running")
	if !strings.Contains(r.last.Message, "exit-code") {
		t.Errorf("failed while restarting: message %q", r.last.Message)
	}

	// The agent restarted by its switch waits for it and reports its failure.
	setRecord(running + " 5 running")
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("active", "success", running+" 5"), unit("failed", "exit-code", running+" 5"))}, at(running, 5))
	check("switch failing after agent restart", r, []string{showLegacy, show, show}, []string{"PHASE_FAILED"}, running+" 5 running")

	// Resent, a switch that failed before activation is reported, not retried.
	setRecord("")
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", next+" 5"))}, at(next, 5))
	check("failed before activation, resent", r, awaited, []string{"PHASE_FAILED"}, "")

	// Assigned again, or by a server not sending revisions, a failed switch is retried.
	for _, revisions := range [][2]int{{5, 6}, {0, 0}} {
		recorded, assigned := revisions[0], revisions[1]
		setRecord("")
		r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", running+" "+strconv.Itoa(recorded)), notLoaded)}, at(running, int64(assigned)))
		check(fmt.Sprintf("retry of revision %d as %d", recorded, assigned), r,
			[]string{showLegacy, show, realise(running), setProfile(running), resetFailed, switchTo(running, assigned), show},
			switched, fmt.Sprintf("%s %d success", running, assigned))
	}

	// The record keeps a failure whose unit the retry reset.
	setRecord("")
	stopped, stop = context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = 6, stop
	r = deploy(stopped, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", running+" 5"))}, at(running, 6))
	stopAt = 0
	check("stopped before the retry started", r, []string{showLegacy, show, realise(running), setProfile(running), resetFailed, switchTo(running, 6)},
		[]string{"PHASE_REALISING", "PHASE_ACTIVATING"}, running+" 5 exit-code")
	for name, record := range map[string]string{
		"stopped before the retry started": running + " 5 exit-code",
		"retry failed to start":            running + " 5 start-failed",
	} {
		setRecord(record)
		r = deploy(ctx, root, nil, at(running, 5))
		check(name+", resent", r, awaited, []string{"PHASE_FAILED"}, record)
		r = deploy(ctx, root, nil, at(running, 6))
		check(name+", assigned again", r, with(awaited, switching(running, 6)), switched, running+" 6 success")
	}
	setRecord(running + " 6 exit-code")
	r = deploy(ctx, root, map[string]string{"systemd-run": "echo 'Failed to start transient service unit' >&2; exit 1"}, at(running, 7))
	check("retry failing to start", r, with(awaited, []string{realise(running), setProfile(running), switchTo(running, 7)}), switchFailed, running+" 7 start-failed")
	if !strings.Contains(r.last.Message, "Failed to start transient service unit") {
		t.Errorf("retry failing to start: message %q", r.last.Message)
	}

	for name, systemctl := range map[string]string{
		"no switch recorded":            notLoaded,
		"failed switch to another path": unit("failed", "exit-code", next+" 1"),
	} {
		setRecord("")
		r = deploy(ctx, root, map[string]string{"systemctl": polls(systemctl)}, at(running, 1))
		check("already active, "+name, r, awaited, []string{"PHASE_DONE"}, "")
		if r.last.Message != "already active" {
			t.Errorf("already active, %s: message %q", name, r.last.Message)
		}
	}

	// The result of a switch in the legacy unit is lost, so the agent switches again.
	setRecord("")
	r = deploy(ctx, root, map[string]string{"legacy": polls(unit("active", "success", "x"), unit("inactive", "success", "x"))}, at(running, 1))
	check("legacy switch", r, with([]string{showLegacy, showLegacy, show}, switching(running, 1)), switched, running+" 1 success")

	// The agent restarted by a switch waits for it before reading the active store path.
	switchingRoot := t.TempDir()
	for _, path := range []string{running, next} {
		if err := os.MkdirAll(filepath.Join(switchingRoot, path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(switchingRoot, path, "nixos-version"), []byte("26.05"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(switchingRoot, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, filepath.Join(switchingRoot, "run/current-system")); err != nil {
		t.Fatal(err)
	}
	activated := filepath.Join(t.TempDir(), "activated")
	r = deploy(ctx, switchingRoot, map[string]string{"systemctl": `[ "$1" = show ] || exit 0
		if [ -e ` + activated + ` ]; then printf '` + notLoaded + `'
		else touch ` + activated + `; ln -sfn ` + next + ` ` + filepath.Join(switchingRoot, "run/current-system") + `
			printf '` + unit("active", "success", next+" 1") + `'; fi`}, at(next, 1))
	if !slices.Equal(r.commands, []string{showLegacy, show, show}) || !slices.Equal(r.phases, []string{"PHASE_DONE"}) {
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
