package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// gcTimeout bounds one garbage collection, trimTimeout the fstrim after it.
const (
	gcTimeout   = time.Hour
	trimTimeout = 10 * time.Minute
)

// PruneAfterBoot prunes once per boot, after delay: a boot that got as far
// as the agent counts as a good one. marker records that this boot was
// pruned, so it must be in a /run the agent's restarts don't empty; empty
// prunes on every start.
func (u *Updater) PruneAfterBoot(ctx context.Context, delay time.Duration, marker string) {
	if marker != "" {
		if _, err := os.Stat(marker); err == nil {
			return
		}
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	err := u.start(protocol.UpdateCleaning, func(c UpdatesConfig) error { return u.prune(c, pruneBoot) })
	if err != nil {
		// Something else runs; the next boot or switch prunes.
		return
	}
	if marker != "" {
		os.WriteFile(marker, nil, 0o644)
	}
}

// Reclaim prunes, collects garbage, hard-links the store's identical files
// and trims, in the background: what the app's Reclaim button does.
func (u *Updater) Reclaim() error {
	return u.start(protocol.UpdateCleaning, func(c UpdatesConfig) error { return u.prune(c, pruneReclaim) })
}

// When a prune runs, which decides what it does beyond deleting generations.
type pruneMode int

const (
	// pruneSwitch collects garbage only when it deleted a generation.
	pruneSwitch pruneMode = iota
	// pruneBoot first empties the ad-hoc profile, and always collects and trims.
	pruneBoot
	// pruneReclaim always collects, even when every generation is kept, and
	// optimises the store before it trims.
	pruneReclaim
)

// prune deletes the system generations beyond the running, the booted and
// the newest KeepGenerations others, and a managed guest's package profile
// generations beyond its current and as many others, then collects garbage
// and trims, as mode says. KeepGenerations nil deletes no generations, and
// then only a reclaim collects.
func (u *Updater) prune(c UpdatesConfig, mode pruneMode) error {
	if mode == pruneBoot && c.managed != nil {
		if err := u.clearAdhoc(c); err != nil {
			u.logf("ad-hoc profile: %v", err)
		}
	}
	if c.KeepGenerations == nil && mode != pruneReclaim {
		if mode == pruneBoot {
			u.trim()
		}
		return nil
	}
	u.setState(protocol.UpdateCleaning)
	current, booted := resolve(u.Current), resolve(u.Booted)
	var del []string
	pkgs := 0
	if c.KeepGenerations != nil {
		keep := *c.KeepGenerations
		for _, g := range u.generations(current, booted) {
			switch {
			case g.Current || g.Booted:
			case keep > 0:
				keep--
			default:
				del = append(del, strconv.Itoa(g.Number))
			}
		}
		if len(del) > 0 {
			u.logf("deleting generations %s", strings.Join(del, ", "))
			if err := u.cmd(envTimeout, u.NixEnv, append([]string{"-p", u.Profile, "--delete-generations"}, del...)...); err != nil {
				return err
			}
		}
		if c.managed != nil {
			n, err := u.prunePackageProfile(c, *c.KeepGenerations)
			if err != nil {
				u.logf("package profile: %v", err)
			}
			pkgs = n
		}
	}
	if len(del) == 0 && pkgs == 0 && mode == pruneSwitch {
		return nil
	}
	u.logf("collecting garbage")
	// It prints every path it deletes: only its summary goes to the log.
	stdout, stderr, err := u.capture(gcTimeout, u.throttled(c.managed, "", u.NixStore, "--gc"))
	if err != nil {
		return err
	}
	if s := gcSummary(stdout + "\n" + stderr); s != "" {
		u.logf("%s", s)
	}
	if mode == pruneReclaim {
		// New paths are linked as they arrive (auto-optimise-store); this catches the rest.
		u.logf("linking identical files")
		if err := u.runArgv(gcTimeout, nil, u.throttled(c.managed, "", u.NixStore, "--optimise")); err != nil {
			u.logf("optimising: %v", err)
		}
	}
	u.trim()
	if len(del) > 0 && current != "" {
		// The boot menu still lists the deleted generations until the boot loader is installed again.
		return u.activate(current, "boot")
	}
	return nil
}

// trim discards the root filesystem's free blocks: the disk image is sparse,
// so only a discard returns what the guest freed to the phone. A failure is
// logged, not returned.
func (u *Updater) trim() {
	if err := u.cmd(trimTimeout, u.Fstrim, "/"); err != nil {
		u.logf("trimming: %v", err)
	}
}

// packageProfile is where `nix profile add` without --profile keeps the
// session user's packages, relative to their home.
const packageProfile = ".local/state/nix/profiles/profile"

// prunePackageProfile deletes the package profile's generations beyond the
// current one and the newest keep others, returning how many it deleted.
// Each add, remove and upgrade makes a generation, which keeps the packages
// it held alive.
func (u *Updater) prunePackageProfile(c UpdatesConfig, keep int) (int, error) {
	home, _, err := u.userHome(c)
	if err != nil {
		return 0, err
	}
	dir, base := filepath.Split(filepath.Join(home, packageProfile))
	root, err := configuredDir(home, dir, false, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	defer root.Close()
	current, err := root.Readlink(base)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	var gens []int
	for _, e := range entries {
		if m := generationLink.FindStringSubmatch(e.Name()); m != nil && m[1] == base && e.Name() != current {
			n, _ := strconv.Atoi(m[2])
			gens = append(gens, n)
		}
	}
	slices.Sort(gens)
	deleted := 0
	for _, n := range gens[:max(len(gens)-keep, 0)] {
		if err := root.Remove(fmt.Sprintf("%s-%d-link", base, n)); err != nil {
			return deleted, err
		}
		deleted++
	}
	if deleted > 0 {
		u.logf("deleted %d package profile generations", deleted)
	}
	return deleted, nil
}

// pruneAfterSwitch prunes after a switch that worked, logging what fails
// rather than failing the switch.
func (u *Updater) pruneAfterSwitch(c UpdatesConfig) {
	if err := u.prune(c, pruneSwitch); err != nil {
		u.logf("switched, but old generations weren't pruned: %v", err)
	}
}

// clearAdhoc deletes the ad-hoc profile and its generations, so what a run
// request fetched becomes garbage.
func (u *Updater) clearAdhoc(c UpdatesConfig) error {
	home, _, err := u.userHome(c)
	if err != nil {
		return err
	}
	dir, base := filepath.Split(c.managed.AdhocProfile)
	root, err := configuredDir(home, filepath.Join(home, dir), false, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if n == base || (strings.HasPrefix(n, base+"-") && strings.HasSuffix(n, "-link")) {
			if err := root.Remove(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// gcSummary is nix-store --gc's closing line ("… store paths deleted, … freed"), or "".
func gcSummary(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasSuffix(l, " freed") {
			return l
		}
	}
	return ""
}
