package fleet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	deploy := func(root, path string, scripts map[string]string) result {
		var r result
		d := newDeployer(func(p *api.DeployProgress) {
			r.phases = append(r.phases, p.Phase.String())
			r.last = p
		})
		d.root = root
		d.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			r.commands = append(r.commands, strings.Join(append([]string{name}, args...), " "))
			return exec.CommandContext(ctx, "sh", "-c", scripts[name])
		}
		d.deploy(context.Background(), path)
		return r
	}

	r := deploy(root, next, map[string]string{"nix-store": "echo copying path 1; echo copying path 2"})
	wantCommands := []string{
		"nix-store --realise " + next,
		"nix-env --profile " + filepath.Join(root, "nix/var/nix/profiles/system") + " --set " + next,
		"systemd-run -E LOCALE_ARCHIVE -E NIXOS_INSTALL_BOOTLOADER=0 -E NIXOS_NO_CHECK --collect --no-ask-password --quiet" +
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

	r = deploy(root, next, map[string]string{"nix-store": "echo copying path 1; echo 'error: cannot substitute' >&2; exit 1"})
	if want := []string{"PHASE_REALISING", "PHASE_FAILED"}; !slices.Equal(r.phases, want) || !strings.Contains(r.last.Message, "error: cannot substitute") {
		t.Errorf("failed realise: phases %v, message %q", r.phases, r.last.Message)
	}
	if len(r.commands) != 1 {
		t.Errorf("ran %v after the failed realise", r.commands[1:])
	}

	for _, tc := range []struct {
		name, root, path, message string
	}{
		{"already active", root, running, "already active"},
		{"invalid store path", root, "/nix/store/x; reboot", "invalid store path"},
		{"not NixOS", t.TempDir(), next, "not a NixOS host"},
	} {
		r := deploy(tc.root, tc.path, nil)
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
