package fleet

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"nixos-fleet-management/api"
)

func TestNixOSGeneration(t *testing.T) {
	const running, other = "/nix/store/aaa-nixos-system-host-26.05", "/nix/store/bbb-nixos-system-host-26.05"
	root := t.TempDir()
	link := func(target, name string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}

	if g, err := nixosGeneration(root); g != nil || err != nil {
		t.Fatalf("without /run/current-system: %v, %v", g, err)
	}
	link(running, "run/current-system")
	if g, err := nixosGeneration(root); g != nil || err != nil {
		t.Fatalf("without nixos-version, as on nix-darwin: %v, %v", g, err)
	}

	if err := os.MkdirAll(filepath.Join(root, running), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, running, "nixos-version"), []byte("26.05.20261001.abcdef0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := nixosGeneration(root)
	if err != nil || g.Number != 0 {
		t.Fatalf("running system without generation link: %v, %v", g, err)
	}

	link(running, "nix/var/nix/profiles/system-1-link")
	link(other, "nix/var/nix/profiles/system-2-link")
	link(running, "nix/var/nix/profiles/system-3-link")
	link(other, "nix/var/nix/profiles/system-4-link")
	link("system-4-link", "nix/var/nix/profiles/system")
	link(running, "run/booted-system")
	g, err = nixosGeneration(root)
	if err != nil {
		t.Fatal(err)
	}
	want := &api.NixOSGeneration{Number: 3, StorePath: running, NixosVersion: "26.05.20261001.abcdef0", BootedStorePath: running, ProfileStorePath: other}
	if !proto.Equal(g, want) {
		t.Errorf("generation = %v, want %v", g, want)
	}

	booted := "/nix/store/ccc-nixos-system-host-26.05"
	if err := os.Remove(filepath.Join(root, "run/booted-system")); err != nil {
		t.Fatal(err)
	}
	link(booted, "run/booted-system")
	for _, system := range []string{running, booted} {
		link("/nix/store/kernel-1", filepath.Join(system, "kernel"))
		link("/nix/store/initrd-1", filepath.Join(system, "initrd"))
		link("/nix/store/modules-1", filepath.Join(system, "kernel-modules"))
	}
	if g, err := nixosGeneration(root); err != nil || g.RebootRequired {
		t.Errorf("booted system with the same kernel, initrd and modules: reboot required = %v, %v", g.GetRebootRequired(), err)
	}
	if err := os.Remove(filepath.Join(root, running, "kernel")); err != nil {
		t.Fatal(err)
	}
	link("/nix/store/kernel-2", filepath.Join(running, "kernel"))
	if g, err := nixosGeneration(root); err != nil || !g.RebootRequired {
		t.Errorf("booted system with another kernel: reboot required = %v, %v", g.GetRebootRequired(), err)
	}
}

func TestCollectFactsIncludesUname(t *testing.T) {
	for _, fact := range collectFacts().Facts {
		var uname api.Uname
		if fact.MessageIs(&uname) {
			if err := fact.UnmarshalTo(&uname); err != nil {
				t.Fatal(err)
			}
			if uname.Sysname == "" || uname.Release == "" || uname.Machine == "" {
				t.Errorf("incomplete uname: %v", &uname)
			}
			return
		}
	}
	t.Fatal("no uname fact")
}
