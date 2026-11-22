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
	var stopAt string
	var stopAgent context.CancelFunc
	// unit is systemctl show output for a switch unit.
	unit := func(state, result, description string) string {
		return `ActiveState=` + state + `\nResult=` + result + `\nDescription=` + description + `\n`
	}
	notLoaded := `printf '` + unit("inactive", "success", "fleet-agent-switch.service") + `'`
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
			if name == stopAt {
				stopAgent()
			}
			key := name
			if name == "systemctl" && args[len(args)-1] == legacySwitchUnit {
				key = "legacy"
			}
			script, ok := scripts[key]
			if !ok && name == "systemctl" {
				script = notLoaded
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
			" --wait " + path + "/bin/switch-to-configuration switch"
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
	check := func(name string, r result, commands, phases []string) {
		t.Helper()
		if !slices.Equal(r.commands, commands) {
			t.Errorf("%s: commands:\n%s\nwant:\n%s", name, strings.Join(r.commands, "\n"), strings.Join(commands, "\n"))
		}
		if !slices.Equal(r.phases, phases) {
			t.Errorf("%s: phases %v, want %v", name, r.phases, phases)
		}
	}
	switched := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}

	r := deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo copying path 2"}, at(next, 1))
	check("switch", r, []string{showLegacy, show, realise(next), setProfile(next), switchTo(next, 1)}, switched)
	if r.last.StorePath != next {
		t.Errorf("progress for %s, want %s", r.last.StorePath, next)
	}

	r = deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"}, at(next, 1))
	check("failed realise", r, []string{showLegacy, show, realise(next)}, []string{"PHASE_REALISING", "PHASE_FAILED"})
	if !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: message %q", r.last.Message)
	}

	// The failed unit stays loaded as the record of the failure.
	r = deploy(ctx, root, map[string]string{"systemd-run": "exit 4"}, at(next, 1))
	check("failed switch", r, []string{showLegacy, show, realise(next), setProfile(next), switchTo(next, 1)},
		[]string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_FAILED"})
	if !strings.Contains(r.last.Message, "journalctl -u fleet-agent-switch") {
		t.Errorf("failed switch: message %q", r.last.Message)
	}

	// A switch the agent did not see end is awaited and reset before the next one, without blaming it.
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("active", "success", running+" 1"), unit("failed", "exit-code", running+" 1"))}, at(next, 1))
	check("previous switch", r, []string{showLegacy, show, show, realise(next), setProfile(next), resetFailed, switchTo(next, 1)}, switched)

	stopped, stop := context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = "systemd-run", stop
	r = deploy(stopped, root, nil, at(next, 1))
	stopAt = ""
	check("stopped agent", r, []string{showLegacy, show, realise(next), setProfile(next), switchTo(next, 1)}, []string{"PHASE_REALISING", "PHASE_ACTIVATING"})

	for name, systemctl := range map[string]string{
		"no switch recorded":            notLoaded,
		"failed switch to another path": polls(unit("failed", "exit-code", next+" 1")),
	} {
		r = deploy(ctx, root, map[string]string{"systemctl": systemctl}, at(running, 1))
		check("already active, "+name, r, []string{showLegacy, show}, []string{"PHASE_DONE"})
		if r.last.Message != "already active" {
			t.Errorf("already active, %s: message %q", name, r.last.Message)
		}
	}

	// The agent restarted by its switch waits for it and reports its failure when
	// the assignment is resent after the reconnect.
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("active", "success", running+" 5"), unit("failed", "exit-code", running+" 5"))}, at(running, 5))
	check("switch failing after agent restart", r, []string{showLegacy, show, show}, []string{"PHASE_FAILED"})
	if !strings.Contains(r.last.Message, "exit-code") {
		t.Errorf("switch failing after agent restart: message %q", r.last.Message)
	}

	// Resent, a switch that failed before activation is reported, not retried.
	r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", next+" 5"))}, at(next, 5))
	check("switch failed before activation, resent", r, []string{showLegacy, show}, []string{"PHASE_FAILED"})

	// Assigned again, or by a server not sending revisions, a failed switch is retried.
	for _, revisions := range [][2]int{{5, 6}, {0, 0}} {
		recorded, assigned := revisions[0], revisions[1]
		r = deploy(ctx, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", running+" "+strconv.Itoa(recorded)))}, at(running, int64(assigned)))
		check(fmt.Sprintf("retry of revision %d as %d", recorded, assigned), r,
			[]string{showLegacy, show, realise(running), setProfile(running), resetFailed, switchTo(running, assigned)}, switched)
	}
	record := filepath.Join(root, "run/fleet-agent-switch.failed")
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("failure record left after the retry: %v", err)
	}

	// Stopped once the retry reset the unit, the agent keeps the failure in a record.
	stopped, stop = context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = "systemd-run", stop
	r = deploy(stopped, root, map[string]string{"systemctl": polls(unit("failed", "exit-code", running+" 5"))}, at(running, 6))
	stopAt = ""
	if data, err := os.ReadFile(record); err != nil || string(data) != running+" 5 exit-code" {
		t.Fatalf("failure record %q: %v", data, err)
	}
	// Restarted, it reports the recorded failure when resent and retries when assigned again.
	r = deploy(ctx, root, nil, at(running, 5))
	check("recorded failure resent", r, []string{showLegacy, show}, []string{"PHASE_FAILED"})
	r = deploy(ctx, root, nil, at(running, 6))
	check("recorded failure assigned again", r, []string{showLegacy, show, realise(running), setProfile(running), switchTo(running, 6)}, switched)
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("failure record left after the retry: %v", err)
	}

	// The result of a switch in the legacy unit is lost, so the agent switches again.
	r = deploy(ctx, root, map[string]string{"legacy": polls(unit("active", "success", "x"), unit("inactive", "success", "x"))}, at(running, 1))
	check("legacy switch", r, []string{showLegacy, showLegacy, show, realise(running), setProfile(running), switchTo(running, 1)}, switched)

	// The agent restarted by a switch waits for it before reading the active store path.
	switching := t.TempDir()
	for _, path := range []string{running, next} {
		if err := os.MkdirAll(filepath.Join(switching, path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(switching, path, "nixos-version"), []byte("26.05"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(switching, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(running, filepath.Join(switching, "run/current-system")); err != nil {
		t.Fatal(err)
	}
	activated := filepath.Join(t.TempDir(), "activated")
	r = deploy(ctx, switching, map[string]string{"systemctl": `[ "$1" = show ] || exit 0
		if [ -e ` + activated + ` ]; then printf '` + unit("inactive", "success", "fleet-agent-switch.service") + `'
		else touch ` + activated + `; ln -sfn ` + next + ` ` + filepath.Join(switching, "run/current-system") + `
			printf '` + unit("active", "success", next+" 1") + `'; fi`}, at(next, 1))
	check("switch activating while waiting", r, []string{showLegacy, show, show}, []string{"PHASE_DONE"})

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
