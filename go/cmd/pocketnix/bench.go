package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const benchUsage = `usage: pocketnix bench run [flags] <workload>...
       pocketnix bench report <dir> [<baseline dir>]

Benchmarks the GUI pipeline. A workload is a pocketnix-gui-bench client mode
and its flags, quoted as one argument: "noise", "scroll --res 0.5",
"rect --rate 30", "touch". "pocketnix-gui-bench --help" lists them.

run measures each workload --reps times, interleaved, writes each run's
JSON to --out and prints the report. On a phone (the default), the guest's
client runs over --ssh while the phone's CPU, the app's own stats and the
battery are sampled over adb; the phone must be unlocked, with the debug
app's VM running and GUI on. --local runs the desktop tier instead: a
compositor and a stand-in app (pocketnix-gui-bench sink) on this machine.

report summarizes a run directory, against a baseline's when given.

run flags:
  --out <dir>          where the runs go (default bench-<time>)
  --reps <n>           runs per workload (3)
  --duration <dur>     each run's measured window (10s)
  --warmup <dur>       drawing before it (3s)
  --quick              one short run each, for iterating: --reps 1
                       --duration 5s --warmup 2s
  --cooldown <dur>     the longest wait before a run for the phone to stop
                       throttling (60s)
  --serial <serial>    adb device (default: adb's only device)
  --package <pkg>      the app (default dev.nickolaj.pocketnix.debug)
  --ssh <command>      runs a shell line in the guest (default "ssh phone-usb";
                       see pocketnix ssh-proxy -h)
  --guest-bench <path> the guest's pocketnix-gui-bench (default: beside the
                       compositor the guest's user manager runs)
  --local              the desktop tier
  --compositor <path>  for --local (default pocketnix-compositor on PATH)
  --gui-bench <path>   for --local (default pocketnix-gui-bench on PATH)
  --sink <flags>       for --local: the sink's display, as its flags
                       (default "--width 1008 --height 2244 --scale 2.625 --refresh 120000")
  --guest-ram          for --local: the sink stands in for an app that maps
                       guest RAM, so big frames cross as PAGES (gui-protocol.md)
`

func runBench(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, benchUsage)
		return errUsage
	}
	switch args[0] {
	case "run":
		return benchRun(args[1:])
	case "report":
		if len(args) < 2 || len(args) > 3 {
			fmt.Fprint(os.Stderr, benchUsage)
			return errUsage
		}
		base := ""
		if len(args) == 3 {
			base = args[2]
		}
		out, err := benchReport(args[1], base)
		fmt.Print(out)
		return err
	}
	fmt.Fprint(os.Stderr, benchUsage)
	return errUsage
}

type benchOpts struct {
	out                        string
	reps                       int
	duration, warmup, cooldown time.Duration
	serial, pkg, ssh, guestBin string
	local, guestRAM            bool
	compositor, guiBench, sink string
	workloads                  []string
}

func benchRun(args []string) error {
	fs := flag.NewFlagSet("bench run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchUsage) }
	var o benchOpts
	fs.StringVar(&o.out, "out", "", "")
	fs.IntVar(&o.reps, "reps", 3, "")
	fs.DurationVar(&o.duration, "duration", 10*time.Second, "")
	fs.DurationVar(&o.warmup, "warmup", 3*time.Second, "")
	fs.DurationVar(&o.cooldown, "cooldown", time.Minute, "")
	quick := fs.Bool("quick", false, "")
	fs.StringVar(&o.serial, "serial", "", "")
	fs.StringVar(&o.pkg, "package", debugPackage, "")
	fs.StringVar(&o.ssh, "ssh", "ssh phone-usb", "")
	fs.StringVar(&o.guestBin, "guest-bench", "", "")
	fs.BoolVar(&o.local, "local", false, "")
	fs.StringVar(&o.compositor, "compositor", "pocketnix-compositor", "")
	fs.StringVar(&o.guiBench, "gui-bench", "pocketnix-gui-bench", "")
	fs.StringVar(&o.sink, "sink", "--width 1008 --height 2244 --scale 2.625 --refresh 120000", "")
	fs.BoolVar(&o.guestRAM, "guest-ram", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *quick {
		o.reps, o.duration, o.warmup = 1, 5*time.Second, 2*time.Second
	}
	o.workloads = fs.Args()
	if len(o.workloads) == 0 || o.reps < 1 {
		fs.Usage()
		return errUsage
	}
	if o.out == "" {
		o.out = "bench-" + time.Now().Format("20060102-150405")
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}

	var t benchTarget
	var err error
	if o.local {
		t, err = startLocal(o)
	} else {
		t, err = startPhone(o)
	}
	if err != nil {
		return err
	}
	defer t.close()

	for rep := 1; rep <= o.reps; rep++ {
		for i, w := range o.workloads {
			t.settle(o.cooldown)
			fmt.Fprintf(os.Stderr, "bench: %s, rep %d/%d\n", w, rep, o.reps)
			res, err := t.run(clientArgs(w, o))
			if err != nil {
				return fmt.Errorf("%s, rep %d: %w", w, rep, err)
			}
			res["workload"] = w
			res["rep"] = rep
			b, _ := json.MarshalIndent(res, "", "  ")
			name := fmt.Sprintf("%02d-%s-%d.json", i+1, slug(w), rep)
			if err := os.WriteFile(filepath.Join(o.out, name), b, 0o644); err != nil {
				return err
			}
		}
	}
	report, err := benchReport(o.out, "")
	if err != nil {
		return err
	}
	fmt.Print(report)
	return os.WriteFile(filepath.Join(o.out, "report.txt"), []byte(report), 0o644)
}

func clientArgs(workload string, o benchOpts) []string {
	f := strings.Fields(workload)
	args := []string{"client", "--mode", f[0]}
	args = append(args, f[1:]...)
	return append(args, "--duration", secs(o.duration), "--warmup", secs(o.warmup))
}

func secs(d time.Duration) string { return fmt.Sprintf("%g", d.Seconds()) }

var nonWord = regexp.MustCompile(`[^a-zA-Z0-9.]+`)

func slug(s string) string { return strings.Trim(nonWord.ReplaceAllString(s, "_"), "_") }

// benchTarget runs one client and returns its result, joined by the
// target's own measurements of the same window.
type benchTarget interface {
	// settle waits, up to max, until the target is back in the state the
	// first run found it in.
	settle(max time.Duration)
	run(clientArgs []string) (map[string]any, error)
	close()
}

// clientRun starts the client command, calls onStart and onEnd as it
// enters and leaves its measured window, and returns its JSON.
func clientRun(cmd *exec.Cmd, onStart, onEnd func()) (map[string]any, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var tail []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			switch line {
			case "MEASURE-START":
				onStart()
			case "MEASURE-END":
				onEnd()
			default:
				fmt.Fprintln(os.Stderr, "  "+line)
				tail = append(tail, line)
			}
		}
	}()
	out, _ := io.ReadAll(stdout)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("the client failed: %w: %s", err, strings.Join(tail, "; "))
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("the client's result: %w", err)
	}
	return map[string]any{"client": res}, nil
}

// local is the desktop tier: its own compositor and sink in a private
// runtime directory.
type local struct {
	dir                  string
	guiBench             string
	compositor, sinkProc *exec.Cmd
}

func startLocal(o benchOpts) (benchTarget, error) {
	compositor, err := exec.LookPath(o.compositor)
	if err != nil {
		return nil, err
	}
	guiBench, err := exec.LookPath(o.guiBench)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pocketnix-bench-")
	if err != nil {
		return nil, err
	}
	l := &local{dir: dir, guiBench: guiBench}
	logs, err := os.Create(filepath.Join(o.out, "local.log"))
	if err != nil {
		return nil, err
	}
	compositorArgs := []string{"--gui-socket", l.path("gui.sock"), "--wayland-display", "wl-bench", "--stats-socket", l.path("compositor.sock")}
	sinkArgs := []string{"sink", "--socket", l.path("gui.sock"), "--stats-socket", l.path("sink.sock")}
	if o.guestRAM {
		compositorArgs = append(compositorArgs, "--guest-socket", l.path("guest.sock"))
		sinkArgs = append(sinkArgs, "--guest-ram", l.path("guest.sock"))
	}
	l.compositor = exec.Command(compositor, compositorArgs...)
	l.compositor.Env = l.env()
	l.compositor.Stderr = logs
	if err := l.compositor.Start(); err != nil {
		return nil, err
	}
	if err := waitFor(l.path("gui.sock"), l.path("wl-bench")); err != nil {
		l.close()
		return nil, err
	}
	l.sinkProc = exec.Command(guiBench, append(sinkArgs, strings.Fields(o.sink)...)...)
	l.sinkProc.Env = l.env()
	l.sinkProc.Stderr = logs
	if err := l.sinkProc.Start(); err != nil {
		l.close()
		return nil, err
	}
	if err := waitFor(l.path("sink.sock")); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}

func (l *local) path(name string) string { return filepath.Join(l.dir, name) }

func (l *local) env() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "WAYLAND_DISPLAY=") && !strings.HasPrefix(e, "DISPLAY=") && !strings.HasPrefix(e, "XDG_RUNTIME_DIR=") {
			env = append(env, e)
		}
	}
	return append(env, "XDG_RUNTIME_DIR="+l.dir, "WAYLAND_DISPLAY=wl-bench")
}

func (l *local) run(args []string) (map[string]any, error) {
	cmd := exec.Command(l.guiBench, append(args, "--no-default-stats",
		"--stats", "compositor="+l.path("compositor.sock"), "--stats", "sink="+l.path("sink.sock"))...)
	cmd.Env = l.env()
	res, err := clientRun(cmd, func() {}, func() {})
	if err != nil {
		return nil, err
	}
	res["target"] = "local"
	return res, nil
}

func (l *local) settle(time.Duration) {}

func (l *local) close() {
	for _, p := range []*exec.Cmd{l.sinkProc, l.compositor} {
		if p != nil && p.Process != nil {
			p.Process.Kill()
			p.Wait()
		}
	}
	os.RemoveAll(l.dir)
}

func waitFor(paths ...string) error {
	deadline := time.Now().Add(10 * time.Second)
	for _, p := range paths {
		for {
			if _, err := os.Stat(p); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s didn't appear", p)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil
}

// phone runs the client in the guest over ssh and samples the phone.
type phone struct {
	o        benchOpts
	dev      adb.Device
	ctl      control
	ssh      []string
	guestBin string
	stayOn   string
}

var execStartPath = regexp.MustCompile(`path=(\S+)`)

func startPhone(o benchOpts) (benchTarget, error) {
	p := &phone{o: o, dev: adb.Device{Serial: o.serial}, ssh: strings.Fields(o.ssh)}
	p.ctl = control{provider{dev: p.dev, authority: o.pkg + ".control"}}
	if len(p.ssh) == 0 {
		return nil, errors.New("--ssh is empty")
	}
	dump, err := p.ctl.dump(-1, 1)
	if err != nil {
		return nil, fmt.Errorf("the debug app: %w", err)
	}
	if !strings.Contains(dump, "gui connected=true ready=true") {
		return nil, fmt.Errorf("the app has no GUI connection; is the VM running with GUI on?\n%s", dump)
	}
	if locked, err := p.locked(); err != nil {
		return nil, err
	} else if locked {
		return nil, errors.New("the phone is locked or its screen is off: unlock it (windows only open on a shown display)")
	}
	p.guestBin = o.guestBin
	if p.guestBin == "" {
		out, err := p.guest("systemctl --user show -P ExecStart pocketnix-compositor.service")
		if err != nil {
			return nil, fmt.Errorf("finding the guest's compositor: %w", err)
		}
		m := execStartPath.FindStringSubmatch(out)
		if m == nil {
			return nil, fmt.Errorf("the compositor unit's ExecStart has no path: %q", out)
		}
		p.guestBin = filepath.Join(filepath.Dir(m[1]), "pocketnix-gui-bench")
	}
	if _, err := p.guest("test -x " + adb.ShellQuote(p.guestBin)); err != nil {
		return nil, fmt.Errorf("the guest has no %s: its compositor predates the benchmarks", p.guestBin)
	}
	// Keeps the screen on while plugged in, for the run; close restores it.
	if out, err := p.dev.Shell("settings get global stay_on_while_plugged_in"); err == nil {
		p.stayOn = strings.TrimSpace(out)
		p.dev.Shell("settings put global stay_on_while_plugged_in 7")
	}
	return p, nil
}

func (p *phone) guest(cmdline string) (string, error) {
	cmd := exec.Command(p.ssh[0], append(p.ssh[1:], cmdline)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", p.o.ssh, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (p *phone) locked() (bool, error) {
	power, err := p.dev.Shell("dumpsys power | grep -m1 mWakefulness=")
	if err != nil {
		return false, err
	}
	if !strings.Contains(power, "Awake") {
		return true, nil
	}
	win, err := p.dev.Shell("dumpsys window | grep -E 'mDreamingLockscreen=|isKeyguardShowing=|mShowingLockscreen=' || true")
	if err != nil {
		return false, err
	}
	return strings.Contains(win, "=true"), nil
}

func (p *phone) run(args []string) (map[string]any, error) {
	if locked, err := p.locked(); err != nil {
		return nil, err
	} else if locked {
		return nil, errors.New("the phone locked")
	}
	// A window opens as its own task only while a pocketnix window shows.
	if _, err := p.dev.Shell("am start -n " + p.o.pkg + "/dev.nickolaj.pocketnix.ControlActivity -a dev.nickolaj.pocketnix.action.START"); err != nil {
		return nil, err
	}
	time.Sleep(time.Second)
	before := p.conditions()

	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = adb.ShellQuote(a)
	}
	cmd := exec.Command(p.ssh[0], append(p.ssh[1:], adb.ShellQuote(p.guestBin)+" "+strings.Join(quoted, " "))...)
	var mu sync.Mutex
	var s0, s1 androidSample
	var app0, app1 map[string]any
	var sampleErr error
	var power *powerSampler
	var clocks *clockSampler
	var clocked map[string]any
	var taps *tapper
	sample := func(s *androidSample, app *map[string]any) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			v, err := p.sample()
			mu.Lock()
			*s, sampleErr = v, errors.Join(sampleErr, err)
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			v, err := p.appStats()
			mu.Lock()
			*app, sampleErr = v, errors.Join(sampleErr, err)
			mu.Unlock()
		}()
		wg.Wait()
	}
	res, err := clientRun(cmd, func() {
		sample(&s0, &app0)
		power = startPower(p.dev)
		clocks = startClocks(p.dev, p.o.pkg)
		if len(args) > 2 && args[2] == "touch" {
			taps = startTaps(p.dev)
		}
	}, func() {
		taps.stop()
		power.stop()
		clocked = clocks.stop()
		sample(&s1, &app1)
	})
	if err != nil {
		return nil, err
	}
	if sampleErr != nil {
		return nil, fmt.Errorf("sampling the phone: %w", sampleErr)
	}
	res["target"] = "phone"
	res["app"] = diffSnapshot(app0, app1)
	res["android"] = map[string]any{
		"cpu":    androidCPU(s0, s1),
		"clocks": clocked,
		"power":  power.result(),
		"before": before,
		"after":  p.conditions(),
	}
	return res, nil
}

// appStats reads the app's GuiStats snapshot.
func (p *phone) appStats() (map[string]any, error) {
	out, err := p.ctl.stats()
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("the app's stats: %w", err)
	}
	return v, nil
}

func (p *phone) close() {
	if p.stayOn != "" {
		p.dev.Shell("settings put global stay_on_while_plugged_in " + adb.ShellQuote(p.stayOn))
	}
}
