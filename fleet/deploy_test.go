package fleet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	deploy := func(ctx context.Context, root, path string, scripts map[string]string) result {
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
			return exec.CommandContext(ctx, "sh", append([]string{"-c", scripts[name], name}, args...)...)
		}
		d.deploy(ctx, path)
		return r
	}
	ctx := context.Background()
	switchPollInterval = time.Millisecond
	t.Cleanup(func() { switchPollInterval = time.Second })
	const (
		show        = "systemctl show --property=ActiveState,Result nixos-rebuild-switch-to-configuration"
		resetFailed = "systemctl reset-failed nixos-rebuild-switch-to-configuration"
	)

	r := deploy(ctx, root, next, map[string]string{"nix-store": "echo copying path 1; echo copying path 2"})
	wantCommands := []string{
		"nix-store --realise " + next,
		"nix-env --profile " + filepath.Join(root, "nix/var/nix/profiles/system") + " --set " + next,
		"systemd-run -E LOCALE_ARCHIVE -E NIXOS_INSTALL_BOOTLOADER=0 -E NIXOS_NO_CHECK --no-ask-password --quiet" +
			" --service-type=exec --unit=nixos-rebuild-switch-to-configuration --wait " + next + "/bin/switch-to-configuration switch",
	}
	if !slices.Equal(r.commands, wantCommands) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(r.commands, "\n"), strings.Join(wantCommands, "\n"))
	}
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_DONE"}; !slices.Equal(r.phases, want) {
		t.Errorf("phases %v, want %v", r.phases, want)
	}
	if r.last.StorePath != next {
		t.Errorf("progress for %s, want %s", r.last.StorePath, next)
	}

	r = deploy(ctx, root, next, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"})
	if want := []string{"PHASE_REALISING", "PHASE_FAILED"}; !slices.Equal(r.phases, want) || !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: phases %v, message %q", r.phases, r.last.Message)
	}
	if len(r.commands) != 1 {
		t.Errorf("ran %v after the failed realise", r.commands[1:])
	}

	r = deploy(ctx, root, next, map[string]string{"systemd-run": "exit 4"})
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING", "PHASE_FAILED"}; !slices.Equal(r.phases, want) || r.commands[len(r.commands)-1] != resetFailed {
		t.Errorf("failed switch: phases %v, commands %v", r.phases, r.commands)
	}

	stopped, stop := context.WithCancel(ctx)
	defer stop()
	stopAt, stopAgent = "systemd-run", stop
	r = deploy(stopped, root, next, nil)
	stopAt = ""
	if want := []string{"PHASE_REALISING", "PHASE_ACTIVATING"}; !slices.Equal(r.phases, want) {
		t.Errorf("stopped agent: phases %v", r.phases)
	}

	r = deploy(ctx, root, running, map[string]string{"systemctl": `printf 'ActiveState=inactive\nResult=success\n'`})
	if !slices.Equal(r.commands, []string{show}) || !slices.Equal(r.phases, []string{"PHASE_DONE"}) || r.last.Message != "already active" {
		t.Errorf("already active: commands %v, phases %v, message %q", r.commands, r.phases, r.last.Message)
	}

	// The agent restarted by the switch it started finds the switch still running, which then fails.
	polled := filepath.Join(t.TempDir(), "polled")
	r = deploy(ctx, root, running, map[string]string{"systemctl": `
		[ "$1" = show ] || exit 0
		if [ -e ` + polled + ` ]; then printf 'ActiveState=failed\nResult=exit-code\n'
		else touch ` + polled + `; printf 'ActiveState=active\nResult=success\n'; fi`})
	if !slices.Equal(r.commands, []string{show, show, resetFailed}) || !slices.Equal(r.phases, []string{"PHASE_FAILED"}) || !strings.Contains(r.last.Message, "exit-code") {
		t.Errorf("switch failing after agent restart: commands %v, phases %v, message %q", r.commands, r.phases, r.last.Message)
	}

	for _, tc := range []struct {
		name, root, path, message string
	}{
		{"invalid store path", root, "/nix/store/x; reboot", "invalid store path"},
		{"not NixOS", t.TempDir(), next, "not a NixOS host"},
	} {
		r := deploy(ctx, tc.root, tc.path, nil)
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
