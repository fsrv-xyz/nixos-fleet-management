package fleet

import (
	"context"
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
	// unit is the systemctl show output for the switch unit.
	unit := func(state, result, description string) string {
		return `ActiveState=` + state + `\nResult=` + result + `\nDescription=` + description + `\n`
	}
	notLoaded := `printf '` + unit("inactive", "success", "fleet-agent-switch.service") + `'`
	// deploy offers paths one after another to the same deployer.
	deploy := func(ctx context.Context, root string, scripts map[string]string, paths ...string) result {
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
			script, ok := scripts[name]
			if !ok && name == "systemctl" {
				script = notLoaded
			}
			return exec.CommandContext(ctx, "sh", append([]string{"-c", script, name}, args...)...)
		}
		for _, path := range paths {
			d.deploy(ctx, path)
		}
		return r
	}
	ctx := context.Background()
	switchPollInterval = time.Millisecond
	t.Cleanup(func() { switchPollInterval = time.Second })
	const (
		show        = "systemctl show --property=ActiveState,Result,Description fleet-agent-switch"
		resetFailed = "systemctl reset-failed fleet-agent-switch"
	)
	realise := func(path string) string { return "nix-store --realise " + path }
	setProfile := func(path string) string {
		return "nix-env --profile " + filepath.Join(root, "nix/var/nix/profiles/system") + " --set " + path
	}
	switchTo := func(path string) string {
		return "systemd-run -E LOCALE_ARCHIVE -E NIXOS_INSTALL_BOOTLOADER=0 -E NIXOS_NO_CHECK --no-ask-password --quiet" +
			" --service-type=exec --unit=fleet-agent-switch --description=" + path + " --wait " + path + "/bin/switch-to-configuration switch"
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
	runningThenFailed := func() string {
		return polls(unit("active", "success", running), unit("failed", "exit-code", running))
	}

	r := deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo copying path 2"}, next)
	if want := []string{show, realise(next), setProfile(next), switchTo(next)}; !slices.Equal(r.commands, want) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(r.commands, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}; !slices.Equal(r.phases, want) {
		t.Errorf("phases %v, want %v", r.phases, want)
	}
	if r.last.StorePath != next {
		t.Errorf("progress for %s, want %s", r.last.StorePath, next)
	}

	r = deploy(ctx, root, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"}, next)
	if want := []string{"PHASE_REALISING", "PHASE_FAILED"}; !slices.Equal(r.phases, want) || !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: phases %v, message %q", r.phases, r.last.Message)
	}
	if want := []string{show, realise(next)}; !slices.Equal(r.commands, want) {
		t.Errorf("failed realise: commands %v", r.commands)
	}

	// The failed unit stays loaded as the record of the failure.
	r = deploy(ctx, root, map[string]string{"systemd-run": "exit 4"}, next)
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_FAILED"}; !slices.Equal(r.phases, want) ||
		r.commands[len(r.commands)-1] != switchTo(next) || !strings.Contains(r.last.Message, "journalctl -u fleet-agent-switch") {
		t.Errorf("failed switch: phases %v, commands %v, message %q", r.phases, r.commands, r.last.Message)
	}

	// A switch the agent did not see end is awaited and reset before the next one, without blaming it.
	r = deploy(ctx, root, map[string]string{"systemctl": runningThenFailed()}, next)
	if want := []string{show, show, realise(next), setProfile(next), resetFailed, switchTo(next)}; !slices.Equal(r.commands, want) {
		t.Errorf("previous switch: commands:\n%s\nwant:\n%s", strings.Join(r.commands, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}; !slices.Equal(r.phases, want) {
		t.Errorf("previous switch: phases %v", r.phases)
	}

	stopped, stop := context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = "systemd-run", stop
	r = deploy(stopped, root, nil, next)
	stopAt = ""
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING"}; !slices.Equal(r.phases, want) {
		t.Errorf("stopped agent: phases %v", r.phases)
	}

	for name, systemctl := range map[string]string{
		"no switch recorded":            notLoaded,
		"failed switch to another path": polls(unit("failed", "exit-code", next)),
	} {
		r = deploy(ctx, root, map[string]string{"systemctl": systemctl}, running)
		if !slices.Equal(r.commands, []string{show}) || !slices.Equal(r.phases, []string{"PHASE_DONE"}) || r.last.Message != "already active" {
			t.Errorf("already active, %s: commands %v, phases %v, message %q", name, r.commands, r.phases, r.last.Message)
		}
	}

	// The agent restarted by its switch waits for it and switches again, as the
	// failed switch left the store path active.
	r = deploy(ctx, root, map[string]string{"systemctl": runningThenFailed()}, running)
	if want := []string{show, show, realise(running), setProfile(running), resetFailed, switchTo(running)}; !slices.Equal(r.commands, want) {
		t.Errorf("switch failing after agent restart: commands:\n%s\nwant:\n%s", strings.Join(r.commands, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}; !slices.Equal(r.phases, want) {
		t.Errorf("switch failing after agent restart: phases %v", r.phases)
	}

	for _, tc := range []struct {
		name, root, path, message string
	}{
		{"invalid store path", root, "/nix/store/x; reboot", "invalid store path"},
		{"not NixOS", t.TempDir(), next, "not a NixOS host"},
	} {
		r := deploy(ctx, tc.root, nil, tc.path)
		if len(r.commands) != 0 || len(r.phases) != 1 || !strings.Contains(r.last.Message, tc.message) {
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
