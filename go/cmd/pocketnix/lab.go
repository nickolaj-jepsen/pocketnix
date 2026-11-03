package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const labUsage = `usage: pocketnix lab memory [flags]

Tests whether Android's memory limit freezes the phone: the same guest memory
fill with the limit lifted, then at the limit, each judged by whether the guest
stops answering. A breaker over adb raises the limit when it does, which ends a
freeze in seconds. A hard power-off is unlikely but possible, so it syncs first
and refuses a limit near the limiter's kill line. It needs the debug build's VM
running, and the guest's user needs passwordless sudo.

It runs only while the screen is off, and if the screen comes on it stops and
restores the limit. A guest on guest RAM keeps what it touched, so the VM
restarts between the arms and after them, which ends what runs in the guest.

flags:
  --fill <MiB>     how much guest memory to touch (default: the VM's less 400)
  --limit <MiB>    the limit under test (default: the phone's out-of-front limit)
  --force          run with the screen on
  --out <dir>      where to write report.json (default: the user cache directory)
  --ssh <command>  runs a shell line in the guest (default "ssh phone-usb";
                   see pocketnix ssh-proxy -h)
  --package <pkg>  the app to test (default: the debug build)
  --serial <s>     adb device to use (default: adb's only device)
`

const (
	// labFillRoot is a tmpfs, so the fill is guest memory rather than disk.
	labFillRoot = "/run/pocketnix-lab"
	// labFillSpare is what the fill leaves of the VM by default.
	labFillSpare = 400
	// labKillMargin keeps a limit clear of the limiter's kill line: over twice a
	// manual limit, as it sets swap alike, the process dies and the VM with it.
	labKillMargin = 512
	// labLowMemoryKiB stops an arm before the phone runs out of memory.
	labLowMemoryKiB = 200 * 1024
	// labRoomMiB is how far under the limit memory must be before lowering it: the
	// write reclaims synchronously, on a thread system_server shares.
	labRoomMiB = 272
	// labStallFails is how many guest echoes in a row must fail to call it a stall.
	labStallFails = 3
)

// labTimes bounds the lab's waits; tests shorten them.
var labTimes = struct {
	echoEvery, echoTimeout, sampleEvery, deadline, recovery, memory time.Duration
}{
	echoEvery: 2 * time.Second, echoTimeout: 10 * time.Second, sampleEvery: time.Second,
	deadline: 3 * time.Minute, recovery: 2 * time.Minute, memory: 2 * time.Minute,
}

// labEnded says why an arm stopped.
const (
	labFinished  = "finished"
	labStalled   = "stalled"
	labLowMemory = "low memory"
	labDeadline  = "deadline"
	labScreenOn  = "screen on"
	labFillError = "fill failed"
)

type labSample struct {
	Time       string `json:"time"`
	State      string `json:"state"`
	Current    int64  `json:"current"`
	HighEvents int64  `json:"highEvents"`
	AvailKiB   int64  `json:"availKiB"`
	Scan       int64  `json:"scan"`
	Steal      int64  `json:"steal"`
	SysPercent int64  `json:"sysPercent"`
}

type labArm struct {
	Name string `json:"name"`
	// LimitMiB is the limit under test, 0 for the lifted arm.
	LimitMiB int `json:"limitMiB"`
	// Ended is one of the lab* reasons.
	Ended          string  `json:"ended"`
	Seconds        float64 `json:"seconds"`
	EchoFails      int     `json:"echoFails"`
	SlowestEchoMs  int64   `json:"slowestEchoMs"`
	RecoverySecond float64 `json:"recoverySeconds,omitempty"`
	// InFront says a sample found the app in front, so the arm didn't test out-of-front.
	InFront bool        `json:"inFront"`
	Samples []labSample `json:"samples"`
}

func (a labArm) delta(field func(labSample) int64) int64 {
	if len(a.Samples) < 2 {
		return 0
	}
	return field(a.Samples[len(a.Samples)-1]) - field(a.Samples[0])
}

func (a labArm) highEvents() int64 { return a.delta(func(s labSample) int64 { return s.HighEvents }) }

// yield is the percentage of scanned pages reclaim freed; negative with no scans.
func (a labArm) yield() float64 {
	scan := a.delta(func(s labSample) int64 { return s.Scan })
	if scan <= 0 {
		return -1
	}
	return 100 * float64(a.delta(func(s labSample) int64 { return s.Steal })) / float64(scan)
}

func (a labArm) peaks() (current, sys, availKiB int64) {
	availKiB = -1
	for _, s := range a.Samples {
		current, sys = max(current, s.Current), max(sys, s.SysPercent)
		if s.AvailKiB > 0 && (availKiB < 0 || s.AvailKiB < availKiB) {
			availKiB = s.AvailKiB
		}
	}
	return current, sys, availKiB
}

// labVerdict is what the two arms rule in or out: it refuses an answer from arms
// that didn't test what they claim.
func labVerdict(lifted, limited labArm) string {
	switch {
	case lifted.InFront || limited.InFront:
		return "no verdict: the app was in front during an arm, under another limit"
	case lifted.Ended == labScreenOn || limited.Ended == labScreenOn:
		return "no verdict: the screen came on"
	case lifted.Ended == labStalled && limited.Ended == labStalled:
		return "the guest stalled with the limit lifted too: this limit isn't the only cause"
	case lifted.Ended == labStalled:
		return "the guest stalled only with the limit lifted: read the samples, as that's not the known failure"
	case limited.Ended == labStalled:
		return "the limit causes the stall: the guest stopped answering only at the limit"
	case lifted.Ended == labLowMemory || limited.Ended == labLowMemory:
		return "no verdict: the phone ran low on memory, and an arm stopped early"
	case limited.highEvents() == 0:
		return "no verdict: the fill never reached the limit, so try a bigger --fill or a lower --limit"
	case lifted.Ended != labFinished || limited.Ended != labFinished:
		return "no verdict: an arm didn't finish"
	}
	return "no stall at the limit: this fill reached it and the guest kept answering"
}

type labVM struct {
	running, guestRAM bool
	memoryMiB         int
}

type labRig struct {
	dev adb.Device
	pkg string
	o   labOpts
	w   io.Writer
	// guest runs a shell line in the guest until it exits or ctx ends.
	guest func(ctx context.Context, cmdline string) error
	// vm reads what the app's VM is running on.
	vm func() (labVM, error)
	// restartVM stops the VM and starts it again, freeing guest RAM.
	restartVM func(ctx context.Context) error
}

type labOpts struct {
	serial, pkg, ssh, out string
	fillMiB, limitMiB     int
	force                 bool
}

type labReport struct {
	Verdict string   `json:"verdict"`
	Fill    int      `json:"fillMiB"`
	Arms    []labArm `json:"arms"`
}

func runLab(args []string) error {
	if len(args) == 0 || args[0] != "memory" {
		fmt.Fprint(os.Stderr, labUsage)
		return errUsage
	}
	o := labOpts{}
	fs := flag.NewFlagSet("lab memory", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, labUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.StringVar(&o.pkg, "package", debugPackage, "")
	fs.StringVar(&o.ssh, "ssh", "ssh phone-usb", "")
	fs.StringVar(&o.out, "out", "", "")
	fs.IntVar(&o.fillMiB, "fill", 0, "")
	fs.IntVar(&o.limitMiB, "limit", 0, "")
	fs.BoolVar(&o.force, "force", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return errUsage
	}
	ssh := strings.Fields(o.ssh)
	if len(ssh) == 0 {
		return errors.New("--ssh is empty")
	}
	dev := adb.Device{Serial: o.serial}
	prov := provider{dev: dev, authority: o.pkg + ".provision"}
	c := control{provider{dev: dev, authority: o.pkg + ".control"}}
	r := &labRig{
		dev: dev, pkg: o.pkg, o: o, w: os.Stdout,
		guest: func(ctx context.Context, cmdline string) error {
			// A guest user's login shell may not be sh.
			cmd := exec.CommandContext(ctx, ssh[0], append(ssh[1:], "sh -c "+adb.ShellQuote(cmdline))...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		vm: func() (labVM, error) {
			dump, err := c.dump(-1, 1)
			if err != nil {
				return labVM{}, err
			}
			v := parseVM(dump)
			caps, err := prov.call("caps", "")
			if err != nil {
				return labVM{}, err
			}
			mib, err := strconv.Atoi(caps["memoryMb"])
			if err != nil {
				return labVM{}, fmt.Errorf("the app's caps has no VM size (a build older than this CLI?): %q", caps["memoryMb"])
			}
			return labVM{running: v.running(), guestRAM: v.guestRam, memoryMiB: mib}, nil
		},
		restartVM: func(context.Context) error {
			if _, err := amStart(dev, o.pkg, actionStop); err != nil {
				return err
			}
			if err := waitRunning(prov, false, stopDeadline); err != nil {
				return err
			}
			if _, err := amStart(dev, o.pkg, actionStart); err != nil {
				return err
			}
			return waitRunning(prov, true, startDeadline)
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return r.run(ctx)
}

// run tests the limit in two arms and leaves the limiter as it found it.
func (r *labRig) run(ctx context.Context) (err error) {
	var l *doctorLimiter
	if status := shellOK(r.dev, limiterStatus); status != nil {
		l = parseLimiter(*status)
	}
	if l == nil || !l.Enabled || !l.Limits {
		return errors.New("this phone has no working memory limiter to test")
	}
	vm, err := r.vm()
	if err != nil {
		return err
	}
	if !vm.running {
		return errors.New("the VM isn't running")
	}
	on, err := r.screenOn()
	if err != nil {
		return err
	}
	if on && !r.o.force {
		return errors.New("the screen is on: lock the phone, or pass --force")
	}
	limit := r.o.limitMiB
	if limit == 0 {
		if l.NotVisibleMiB == nil {
			return errors.New("the limiter names no out-of-front limit: pass --limit")
		}
		limit = *l.NotVisibleMiB
	}
	fill := r.o.fillMiB
	if fill == 0 {
		fill = vm.memoryMiB - labFillSpare
	}
	switch {
	case fill <= 0 || fill > vm.memoryMiB:
		return fmt.Errorf("a %d MiB fill doesn't fit a %d MiB VM", fill, vm.memoryMiB)
	case vm.memoryMiB+limiterOverheadMiB <= limit:
		return fmt.Errorf("a %d MiB VM fits under a %d MiB limit, so it can't reach it: pass a lower --limit", vm.memoryMiB, limit)
	case 2*limit < vm.memoryMiB+labKillMargin:
		return fmt.Errorf("%d MiB is too close to the limiter's kill line, twice the limit, for a %d MiB VM: the VM would die", limit, vm.memoryMiB)
	}
	pid := shellOK(r.dev, "pidof "+adb.ShellQuote(r.pkg))
	if pid == nil {
		return fmt.Errorf("%s isn't running", r.pkg)
	}
	var restored bool
	restore := func() {
		if restored {
			return
		}
		restored = true
		// Memory may be over the limit, so this must never lower it.
		if rerr := r.restore(l); rerr != nil && err == nil {
			err = rerr
		}
	}
	defer restore()

	report := labReport{Fill: fill}
	fmt.Fprintf(r.w, "lab: limit lifted, %d MiB fill\n", fill)
	if err := liftLimit(ctx, r.dev, r.pkg, io.Discard); err != nil {
		return err
	}
	lifted := r.arm(ctx, "lifted", 0, *pid, fill)
	report.Arms = append(report.Arms, lifted)
	if ctx.Err() != nil || lifted.Ended == labScreenOn {
		return r.finish(report, ctx.Err())
	}
	if err := r.free(ctx, vm, limit); err != nil {
		return r.finish(report, err)
	}

	// The VM may have restarted under the same app process, so this pid still holds.
	fmt.Fprintf(r.w, "lab: at %d MiB, %d MiB fill\n", limit, fill)
	if err := r.lower(ctx, *pid, limit); err != nil {
		return r.finish(report, err)
	}
	limited := r.arm(ctx, "limited", limit, *pid, fill)
	report.Arms = append(report.Arms, limited)
	report.Verdict = labVerdict(lifted, limited)
	var freeErr error
	if vm.guestRAM {
		freeErr = r.restartVM(ctx)
	}
	return r.finish(report, freeErr)
}

// finish prints and writes what the arms found.
func (r *labRig) finish(report labReport, err error) error {
	for _, a := range report.Arms {
		cur, sys, avail := a.peaks()
		fmt.Fprintf(r.w, "%-8s %-10s %5.0f s, echo fails %d (slowest %d ms), %d limit events, peak %d MiB, kernel CPU %d%%, least free %d MiB, reclaim yield %.1f%%",
			a.Name, a.Ended, a.Seconds, a.EchoFails, a.SlowestEchoMs, a.highEvents(), cur>>20, sys, avail/1024, a.yield())
		if a.RecoverySecond > 0 {
			fmt.Fprintf(r.w, ", guest back %.0f s after raising the limit", a.RecoverySecond)
		}
		fmt.Fprintln(r.w)
	}
	if report.Verdict != "" {
		fmt.Fprintln(r.w, "verdict:", report.Verdict)
	}
	path, werr := r.writeReport(report)
	if werr != nil {
		fmt.Fprintln(r.w, "lab: no report:", werr)
	} else {
		fmt.Fprintln(r.w, "lab: report in", path)
	}
	return err
}

func (r *labRig) writeReport(report labReport) (string, error) {
	dir := r.o.out
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "pocketnix", "lab")
	}
	dir = filepath.Join(dir, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "report.json")
	return path, os.WriteFile(path, b, 0o644)
}

func (r *labRig) screenOn() (bool, error) {
	out, _, err := r.dev.ShellStatus(wakefulnessCommand)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "mWakefulness=Awake"), nil
}

// free has the guest let go of the arm's memory, or restarts the VM when it can't.
func (r *labRig) free(ctx context.Context, vm labVM, limit int) error {
	if vm.guestRAM {
		fmt.Fprintln(r.w, "lab: restarting the VM, as guest RAM doesn't go back")
		if err := r.restartVM(ctx); err != nil {
			return fmt.Errorf("restarting the VM: %w", err)
		}
		if err := pollUntil(ctx, labTimes.recovery, func() (bool, error) {
			echo, cancel := context.WithTimeout(ctx, labTimes.echoTimeout)
			defer cancel()
			return r.guest(echo, "true") == nil, nil
		}); err != nil {
			return fmt.Errorf("the guest didn't come back: %w", err)
		}
	} else {
		cctx, cancel := context.WithTimeout(ctx, labTimes.echoTimeout)
		defer cancel()
		if err := r.guest(cctx, "sudo sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'"); err != nil {
			return fmt.Errorf("dropping the guest's caches: %w", err)
		}
	}
	target := int64(limit-labRoomMiB) << 20
	if err := pollUntil(ctx, labTimes.memory, func() (bool, error) {
		s, ok := r.sample()
		return ok && s.Current < target, nil
	}); err != nil {
		return fmt.Errorf("memory stayed over %d MiB, so the limit can't come down: %w", limit-labRoomMiB, err)
	}
	return nil
}

// lower puts the limit under test in place. The caller has checked memory is under it.
func (r *labRig) lower(ctx context.Context, pid string, limit int) error {
	if err := limiter(r.dev, "ignore", "none"); err != nil {
		return err
	}
	s, ok := r.sample()
	if !ok || s.Current >= int64(limit-labRoomMiB)<<20 {
		return fmt.Errorf("memory is at %d MiB, too close to %d MiB to lower the limit", s.Current>>20, limit)
	}
	if err := limiter(r.dev, "manual", pid, strconv.Itoa(limit)); err != nil {
		return err
	}
	want := strconv.FormatInt(int64(limit)<<20, 10)
	return pollUntil(ctx, limitWait, func() (bool, error) {
		out := shellOK(r.dev, appMemoryCommand(r.pkg))
		return out != nil && parseAppMemory(*out).High == want, nil
	})
}

// restore leaves the limiter as it was, never lowering a limit.
func (r *labRig) restore(was *doctorLimiter) error {
	if was.AppIgnored {
		return liftLimit(context.Background(), r.dev, r.pkg, io.Discard)
	}
	target := "none"
	if was.Ignored != "" {
		target = was.Ignored
	}
	if err := limiter(r.dev, "ignore", target); err != nil {
		return err
	}
	fmt.Fprintln(r.w, "lab: the app's limit stays raised until it next moves in or out of front")
	return nil
}

func labFillCommand(mib int) string {
	return fmt.Sprintf("set -e; sudo sync; sudo mkdir -p %[1]s; sudo mount -t tmpfs -o size=%[2]dm tmpfs %[1]s; "+
		"sudo dd if=/dev/urandom of=%[1]s/fill bs=1M count=%[3]d status=none", labFillRoot, mib+100, mib)
}

func labCleanCommand() string {
	return fmt.Sprintf("sudo pkill -x dd; sudo rm -f %[1]s/fill; sudo umount %[1]s; sudo rmdir %[1]s; true", labFillRoot)
}

// arm fills guest memory while watching for a stall, and ends with the fill gone
// and, whatever happened, any limit under test raised.
func (r *labRig) arm(ctx context.Context, name string, limit int, pid string, fill int) labArm {
	a := labArm{Name: name, LimitMiB: limit, Samples: []labSample{}}
	start := time.Now()
	sampleCtx, stopSampling := context.WithCancel(context.Background())
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sampleCtx.Err() == nil {
			if s, ok := r.sample(); ok {
				mu.Lock()
				a.Samples = append(a.Samples, s)
				a.InFront = a.InFront || strings.Contains(s.State, "TOP")
				mu.Unlock()
			}
			select {
			case <-sampleCtx.Done():
			case <-time.After(labTimes.sampleEvery):
			}
		}
	}()

	fillCtx, stopFill := context.WithCancel(ctx)
	defer stopFill()
	filled := make(chan error, 1)
	go func() { filled <- r.guest(fillCtx, labFillCommand(fill)) }()

	raise := func() {
		if limit == 0 {
			return
		}
		if err := limiter(r.dev, "manual", pid, strconv.Itoa(liftedLimitMiB)); err != nil {
			fmt.Fprintln(r.w, "lab: couldn't raise the limit:", err)
		}
	}
	ended := ""
	for ended == "" {
		echoStart := time.Now()
		echo, cancel := context.WithTimeout(ctx, labTimes.echoTimeout)
		err := r.guest(echo, "true")
		cancel()
		if err != nil {
			a.EchoFails++
		} else {
			a.EchoFails = 0
			a.SlowestEchoMs = max(a.SlowestEchoMs, time.Since(echoStart).Milliseconds())
		}
		mu.Lock()
		var avail int64
		if n := len(a.Samples); n > 0 {
			avail = a.Samples[n-1].AvailKiB
		}
		mu.Unlock()
		select {
		case err := <-filled:
			ended = labFinished
			if err != nil && ctx.Err() == nil {
				ended = labFillError
				fmt.Fprintln(r.w, "lab: the fill failed:", err)
			}
			continue
		default:
		}
		on, _ := r.screenOn()
		switch {
		case ctx.Err() != nil:
			ended = labDeadline
		case a.EchoFails >= labStallFails:
			ended = labStalled
		case on && !r.o.force:
			ended = labScreenOn
		case avail > 0 && avail < labLowMemoryKiB:
			ended = labLowMemory
		case time.Since(start) >= labTimes.deadline:
			ended = labDeadline
		}
		if ended == "" {
			select {
			case err := <-filled:
				ended = labFinished
				if err != nil && ctx.Err() == nil {
					ended = labFillError
				}
			case <-ctx.Done():
				ended = labDeadline
			case <-time.After(labTimes.echoEvery):
			}
		}
	}
	a.Ended = ended
	a.Seconds = time.Since(start).Seconds()

	raise()
	if ended == labStalled {
		raised := time.Now()
		_ = pollUntil(context.Background(), labTimes.recovery, func() (bool, error) {
			echo, cancel := context.WithTimeout(context.Background(), labTimes.echoTimeout)
			defer cancel()
			return r.guest(echo, "true") == nil, nil
		})
		a.RecoverySecond = time.Since(raised).Seconds()
	}
	stopFill()
	clean, cancel := context.WithTimeout(context.Background(), labTimes.recovery)
	defer cancel()
	if err := r.guest(clean, labCleanCommand()); err != nil {
		fmt.Fprintln(r.w, "lab: cleaning up the fill:", err)
	}
	stopSampling()
	wg.Wait()
	return a
}

func labSampleCommand(pkg string) string {
	return appMemoryCommand(pkg) + ` && ` +
		`echo "avail=$(sed -n 's/^MemAvailable: *\([0-9]*\).*/\1/p' /proc/meminfo)" && ` +
		`echo "scan=$(sed -n 's/^pgscan //p' $d/memory.stat)" && echo "steal=$(sed -n 's/^pgsteal //p' $d/memory.stat)" && ` +
		`echo "sys=$(top -b -n1 -m1 | sed -n 's/.* \([0-9]*\)%sys.*/\1/p' | head -1)"`
}

// sample reads the phone over adb, which stays up through a freeze.
func (r *labRig) sample() (labSample, bool) {
	out := shellOK(r.dev, labSampleCommand(r.pkg))
	if out == nil {
		return labSample{}, false
	}
	v := keyValues(*out)
	n := func(k string) int64 {
		i, _ := strconv.ParseInt(v[k], 10, 64)
		return i
	}
	return labSample{
		Time: time.Now().UTC().Format(time.RFC3339), State: v["state"], Current: n("current"), HighEvents: n("highEvents"),
		AvailKiB: n("avail"), Scan: n("scan"), Steal: n("steal"), SysPercent: n("sys"),
	}, true
}
