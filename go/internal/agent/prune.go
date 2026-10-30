package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// gcTimeout bounds one garbage collection.
const gcTimeout = time.Hour

// PruneAfterBoot prunes once per boot, after delay: a boot that got as far
// as the agent counts as a good one. marker (in /run) records that this
// boot was pruned, across agent restarts; empty prunes on every start.
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
	err := u.start(protocol.UpdateCleaning, func(c UpdatesConfig) error { return u.prune(c, true) })
	if err != nil {
		// Something else runs; the next boot or switch prunes.
		return
	}
	if marker != "" {
		os.WriteFile(marker, nil, 0o644)
	}
}

// prune deletes the system generations beyond the running, the booted and
// the newest KeepGenerations others, then collects garbage. After a boot it
// first empties the ad-hoc profile, and collects even with nothing deleted.
func (u *Updater) prune(c UpdatesConfig, boot bool) error {
	if boot && c.managed != nil {
		if err := u.clearAdhoc(c); err != nil {
			u.logf("ad-hoc profile: %v", err)
		}
	}
	if c.KeepGenerations == nil {
		return nil
	}
	u.setState(protocol.UpdateCleaning)
	current, booted := resolve(u.Current), resolve(u.Booted)
	keep := *c.KeepGenerations
	var del []string
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
	} else if !boot {
		return nil
	}
	u.logf("collecting garbage")
	if err := u.runArgv(gcTimeout, nil, u.throttled(c.managed, "", u.NixStore, "--gc")); err != nil {
		return err
	}
	if len(del) > 0 && current != "" {
		// The boot menu still lists the deleted generations until the boot loader is installed again.
		return u.activate(current, "boot")
	}
	return nil
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
