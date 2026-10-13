package fleet

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"nixos-fleet-management/api"
)

// collectFacts skips and logs facts it cannot read, so one broken source does
// not keep the agent from reporting the others.
func collectFacts() *api.Facts {
	facts := []proto.Message{&api.Platform{Goos: runtime.GOOS, Goarch: runtime.GOARCH}}
	if uname, err := unameFact(); err != nil {
		log.Printf("uname fact: %v", err)
	} else {
		facts = append(facts, uname)
	}
	if generation, err := nixosGeneration("/"); err != nil {
		log.Printf("NixOS generation fact: %v", err)
	} else if generation != nil {
		facts = append(facts, generation)
	}

	result := &api.Facts{}
	for _, fact := range facts {
		var packed anypb.Any
		// Deterministic, so unchanged facts encode to the bytes the server already stored.
		if err := anypb.MarshalFrom(&packed, fact, proto.MarshalOptions{Deterministic: true}); err != nil {
			log.Printf("%T fact: %v", fact, err)
			continue
		}
		result.Facts = append(result.Facts, &packed)
	}
	return result
}

func unameFact() (*api.Uname, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return nil, err
	}
	return &api.Uname{
		Sysname:  unix.ByteSliceToString(u.Sysname[:]),
		Nodename: unix.ByteSliceToString(u.Nodename[:]),
		Release:  unix.ByteSliceToString(u.Release[:]),
		Version:  unix.ByteSliceToString(u.Version[:]),
		Machine:  unix.ByteSliceToString(u.Machine[:]),
	}, nil
}

const systemProfiles = "nix/var/nix/profiles"

func readlinkIfExists(path string) (string, error) {
	target, err := os.Readlink(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return target, err
}

func bootDiffers(root, booted, current string) (bool, error) {
	for _, name := range []string{"kernel", "initrd", "kernel-modules"} {
		b, err := readlinkIfExists(filepath.Join(root, booted, name))
		if err != nil {
			return false, err
		}
		c, err := readlinkIfExists(filepath.Join(root, current, name))
		if err != nil {
			return false, err
		}
		if b != c {
			return true, nil
		}
	}
	return false, nil
}

// nixosGeneration returns nil on hosts without a running NixOS system below root.
// nixos-version is the marker, as nix-darwin also has /run/current-system.
func nixosGeneration(root string) (*api.NixOSGeneration, error) {
	current, err := os.Readlink(filepath.Join(root, "run/current-system"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	version, err := os.ReadFile(filepath.Join(root, current, "nixos-version"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	generation := &api.NixOSGeneration{StorePath: current, NixosVersion: strings.TrimSpace(string(version))}
	if generation.BootedStorePath, err = readlinkIfExists(filepath.Join(root, "run/booted-system")); err != nil {
		return nil, err
	}
	if generation.BootedStorePath != "" && generation.BootedStorePath != current {
		if generation.RebootRequired, err = bootDiffers(root, generation.BootedStorePath, current); err != nil {
			return nil, err
		}
	}
	profiles := filepath.Join(root, systemProfiles)
	profileLink, err := readlinkIfExists(filepath.Join(profiles, "system"))
	if err != nil {
		return nil, err
	}
	if profileLink != "" {
		if !filepath.IsAbs(profileLink) {
			profileLink = filepath.Join(profiles, profileLink)
		}
		if generation.ProfileStorePath, err = os.Readlink(profileLink); err != nil {
			return nil, err
		}
	}

	links, err := filepath.Glob(filepath.Join(profiles, "system-*-link"))
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		target, err := os.Readlink(link)
		if err != nil || target != current {
			continue
		}
		number, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(link), "system-"), "-link"), 10, 32)
		// Rebuilding an unchanged configuration links several generations to the same path.
		if err == nil && uint32(number) > generation.Number {
			generation.Number = uint32(number)
		}
	}
	return generation, nil
}
