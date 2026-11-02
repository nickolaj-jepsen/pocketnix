package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// liftedLimitMiB is past any phone's RAM. memory-limiter's manual takes MiB,
// whatever its help says, and sets memory.high and memory.swap.max alike.
const liftedLimitMiB = 1 << 20

// limitWait bounds system_server applying a limit, which its handler does later.
var limitWait = 5 * time.Second

// limiter runs a `cmd activity memory-limiter` subcommand, which prints nothing
// on success.
func limiter(dev adb.Device, args ...string) error {
	line := "cmd activity memory-limiter " + strings.Join(args, " ")
	out, status, err := shellStatus(dev, line)
	if err != nil {
		return err
	}
	if out = strings.TrimSpace(out); status != 0 || out != "" {
		return fmt.Errorf("%s: exit status %d: %s", line, status, out)
	}
	return nil
}

// liftLimit takes pkg out of MemoryLimiter until the phone reboots, and a process
// started later gets no limit at all. It removes a protection from the rest of
// the phone, and the command is the limiter's own test support, which Android may
// remove: it's a developer's stopgap and a test's control.
func liftLimit(ctx context.Context, dev adb.Device, pkg string, w io.Writer) error {
	var l *doctorLimiter
	if status := shellOK(dev, limiterStatus); status != nil {
		l = parseLimiter(*status)
	}
	if l == nil || !l.Enabled {
		return fmt.Errorf("this phone has no working memory limiter to lift")
	}
	uids, err := appUID(dev, pkg)
	if err != nil {
		return err
	}
	pid := shellOK(dev, "pidof "+adb.ShellQuote(pkg))
	// The app has a uid per Android user, and the limiter ignores one uid: the
	// running process's, or else the first user's.
	uid, _, _ := strings.Cut(uids, ",")
	if pid != nil {
		if owner := shellOK(dev, "stat -c %u /proc/"+*pid); owner != nil {
			uid = *owner
		}
	}
	// manual is dropped while its uid is ignored, so ignoring comes last.
	if l.Ignored != "" {
		if l.Ignored != uid {
			fmt.Fprintf(w, "the limiter stops ignoring %s: it holds one uid\n", l.Ignored)
		}
		if err := limiter(dev, "ignore", "none"); err != nil {
			return err
		}
	}
	if pid != nil {
		if err := limiter(dev, "manual", *pid, strconv.Itoa(liftedLimitMiB)); err != nil {
			return err
		}
	}
	if err := limiter(dev, "ignore", uid); err != nil {
		return err
	}
	if pid != nil {
		want := strconv.FormatInt(liftedLimitMiB<<20, 10)
		err := pollUntil(ctx, limitWait, func() (bool, error) {
			out := shellOK(dev, appMemoryCommand(pkg))
			return out != nil && parseAppMemory(*out).High == want, nil
		})
		if err != nil {
			return fmt.Errorf("process %s's memory.high didn't lift: %w", *pid, err)
		}
		fmt.Fprintf(w, "lifted the limit of process %s\n", *pid)
	}
	fmt.Fprintf(w, "the limiter ignores uid %s until the phone reboots\n", uid)
	return nil
}

// restoreLimit undoes liftLimit for later processes and proc states. The running
// process keeps its lifted limit until the app next moves in or out of front.
func restoreLimit(dev adb.Device, w io.Writer) error {
	if err := limiter(dev, "ignore", "none"); err != nil {
		return err
	}
	fmt.Fprintln(w, "the limiter ignores nothing; the running process's limit returns when the app next moves in or out of front")
	return nil
}
