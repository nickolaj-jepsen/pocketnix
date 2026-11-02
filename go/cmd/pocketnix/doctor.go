package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const doctorUsage = `usage: pocketnix doctor [--serial <s>] [--debug] [--json] [--smoke] [--gpu-apps <id>,<id>] [--record <dir>]

Probes a phone for what pocketnix needs (AVF, its kernel, u-boot, the app)
and prints a verdict: ok, warn or fail. It exits 1 only on fail, which means
the phone definitely can't run the VM.

flags:
  --json             print the report as JSON, for a device report
  --debug            look at the debug build (dev.nickolaj.pocketnix.debug)
  --smoke            also run the end-to-end checks against the debug build's
                     running VM (implies --debug; the phone unlocked, pocketnix in
                     front). They open tabs, a forward, a notification, an
                     inhibitor and apps, end them all, and reboot the guest last;
                     a failing check also exits 1
  --gpu-apps <ids>   two app ids from pocketnix control dump's apps=, for the GPU step
  --record <dir>     record the run (screen, logcat, commands) into a bundle under dir
  --serial <serial>  adb device to use (default: adb's only device)
`

const (
	// ubootPath mirrors AvfEngine.UBOOT: the bootloader the app boots the image with.
	ubootPath = "/apex/com.android.virt/etc/u-boot.bin"
	vmTool    = "/apex/com.android.virt/bin/vm"
	// avfFeature is the system feature an AVF device declares.
	avfFeature = "android.software.virtualization_framework"
	// grapheneOSPackage is GrapheneOS's App Store, present on every GrapheneOS install.
	grapheneOSPackage = "app.grapheneos.apps"
	// testedKernel is the kernel branch pocketnix has booted on.
	testedKernel = "6.1"
)

type doctorOpts struct {
	serial, record     string
	debug, json, smoke bool
	gpuApps            []string
}

func parseDoctor(args []string) (*doctorOpts, error) {
	o := &doctorOpts{}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, doctorUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.smoke, "smoke", false, "")
	fs.StringVar(&o.record, "record", "", "")
	fs.Func("gpu-apps", "", func(v string) error {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.gpuApps = append(o.gpuApps, id)
			}
		}
		return nil
	})
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return nil, errUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, errUsage
	}
	if o.smoke {
		o.debug = true
	}
	return o, nil
}

func (o *doctorOpts) pkg() string {
	if o.debug {
		return debugPackage
	}
	return releasePackage
}

// runDoctor prints the report, and returns exitStatus(1) on a fail verdict
// so that main adds nothing to it.
func runDoctor(args []string) error {
	o, err := parseDoctor(args)
	if err != nil {
		return err
	}
	if o.record != "" {
		return errors.New("doctor: --record is not implemented yet")
	}
	dev := adb.Device{Serial: o.serial}
	if state, err := dev.State(); err != nil || state != "device" {
		return fmt.Errorf("no usable adb device (state %q): %v", state, err)
	}
	r := probe(dev, o.pkg())
	if o.smoke {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		// After the first interrupt smoke cleans up; a second one kills it as usual.
		context.AfterFunc(ctx, stop)
		r.Smoke = runSmoke(ctx, dev, o.pkg(), o.gpuApps, os.Stderr)
		stop()
	}
	if o.json {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", b)
	} else {
		printDoctor(os.Stdout, r)
	}
	if r.Verdict.Level == levelFail {
		return exitStatus(1)
	}
	for _, st := range r.Smoke {
		if st.OK != nil && !*st.OK {
			return exitStatus(1)
		}
	}
	return nil
}

// probe reads every fact doctor reports from dev's shell, the app's about
// pkg, and judges them. It reads named props only (B1).
func probe(dev adb.Device, pkg string) doctorReport {
	r := doctorReport{
		Schema: doctorSchema,
		Device: probeDevice(dev),
		AVF:    probeAVF(dev),
		App:    probeApp(dev, pkg),
	}
	r.Verdict = shellVerdict(r.Device, r.AVF)
	appVerdict(&r.Verdict, r.App)
	return r
}

// shellStatus runs cmdline in dev's shell and returns its stdout and exit
// status; err is only adb's own failure. The marker is adb.Device.Shell's.
func shellStatus(dev adb.Device, cmdline string) (string, int, error) {
	const marker = "pocketnix-exit="
	out, err := dev.Run("shell", cmdline+"; echo "+marker+"$?")
	if err != nil {
		return "", -1, err
	}
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return "", -1, fmt.Errorf("adb shell %s: no exit status in output", cmdline)
	}
	status, err := strconv.Atoi(strings.TrimSpace(out[i+len(marker):]))
	if err != nil {
		return "", -1, fmt.Errorf("adb shell %s: exit status %q", cmdline, out[i+len(marker):])
	}
	return out[:i], status, nil
}

// shellOK is a command's trimmed output, nil unless it exited 0 printing some.
func shellOK(dev adb.Device, cmdline string) *string {
	out, status, err := shellStatus(dev, cmdline)
	if out = strings.TrimSpace(out); err != nil || status != 0 || out == "" {
		return nil
	}
	return &out
}

// getprop is a system property, nil when it reads empty: getprop can't tell
// an unset prop from one it may not read.
func getprop(dev adb.Device, name string) *string { return shellOK(dev, "getprop "+name) }

// propBool reads a boolean as Android's GetBoolProperty does; nil for
// anything else.
func propBool(v *string) *bool {
	if v == nil {
		return nil
	}
	var b bool
	switch *v {
	case "1", "true", "y", "yes", "on":
		b = true
	case "0", "false", "n", "no", "off":
		b = false
	default:
		return nil
	}
	return &b
}

func probeDevice(dev adb.Device) doctorDevice {
	d := doctorDevice{
		Fingerprint:  getprop(dev, "ro.build.fingerprint"),
		Model:        getprop(dev, "ro.product.model"),
		Manufacturer: getprop(dev, "ro.product.manufacturer"),
		SDK:          getprop(dev, "ro.build.version.sdk"),
		Release:      getprop(dev, "ro.build.version.release"),
		Kernel:       shellOK(dev, "uname -r"),
		OS:           osUnknown,
	}
	var soc []string
	for _, p := range []string{"ro.soc.manufacturer", "ro.soc.model"} {
		if v := getprop(dev, p); v != nil {
			soc = append(soc, *v)
		}
	}
	if len(soc) > 0 {
		s := strings.Join(soc, " ")
		d.SoC = &s
	}
	// pm path exits 1, printing nothing, for a package that isn't installed.
	switch out, status, err := shellStatus(dev, "pm path "+grapheneOSPackage); {
	case err != nil:
	case status == 0:
		d.OS = osGrapheneOS
	case status == 1 && strings.TrimSpace(out) == "":
		d.OS = osStock
	}
	return d
}

var apexVersion = regexp.MustCompile(`(?m)^package:com\.(?:google\.)?android\.virt versionCode:(\d+)\s*$`)

func probeAVF(dev adb.Device) doctorAVF {
	a := doctorAVF{
		VMSupported:          propBool(getprop(dev, "ro.boot.hypervisor.vm.supported")),
		ProtectedVMSupported: propBool(getprop(dev, "ro.boot.hypervisor.protected_vm.supported")),
		HypervisorVersion:    getprop(dev, "ro.boot.hypervisor.version"),
		VMInfo:               []string{},
	}
	// pm has-feature prints true or false, exiting 0 or 1.
	if out, status, err := shellStatus(dev, "pm has-feature "+avfFeature); err == nil && (status == 0 || status == 1) {
		a.Feature = propBool(ptr(strings.TrimSpace(out)))
	}
	if out := shellOK(dev, "pm list packages --apex-only --show-versioncode"); out != nil {
		if m := apexVersion.FindStringSubmatch(*out); m != nil {
			a.ApexVersion = &m[1]
		}
	}
	_, status, err := shellStatus(dev, "ls "+ubootPath)
	a.UBoot = err == nil && status == 0
	// vm info exits non-zero after its sysprop lines when it can't reach
	// virtmgr; those lines still count. 126 and 127 are the shell's own.
	if out, status, err := shellStatus(dev, vmTool+" info"); err == nil && status != 126 && status != 127 {
		for _, line := range strings.Split(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				a.VMInfo = append(a.VMInfo, line)
			}
		}
	}
	return a
}

var versionName = regexp.MustCompile(`(?m)^\s*versionName=(\S+)`)

func probeApp(dev adb.Device, pkg string) doctorApp {
	a := doctorApp{Package: pkg}
	if _, status, err := shellStatus(dev, "pm path "+pkg); err != nil || status != 0 {
		a.CapsError = "the app isn't installed"
		return a
	}
	a.Installed = true
	if out := shellOK(dev, "dumpsys package "+pkg); out != nil {
		if m := versionName.FindStringSubmatch(*out); m != nil {
			a.VersionName = &m[1]
		}
	}
	caps, err := provider{dev: dev, authority: pkg + ".provision"}.call("caps", "")
	if err != nil {
		a.CapsError = strings.Join(strings.Fields(err.Error()), " ")
		return a
	}
	delete(caps, "ok")
	// ProvisionProvider's capsAnswer URL-encodes these.
	for _, k := range []string{"engine", "appVersion"} {
		if v, err := url.QueryUnescape(caps[k]); err == nil && caps[k] != "" {
			caps[k] = v
		}
	}
	a.Caps = caps
	return a
}

// capNonProtectedVM mirrors AvfReflect.CAPABILITY_NON_PROTECTED_VM.
const capNonProtectedVM = 2

// appVerdict adds the app's caps answer to v: it can only warn, as the
// app's view is never definite.
func appVerdict(v *doctorVerdict, a doctorApp) {
	if !a.Installed {
		return
	}
	if a.Caps == nil {
		v.warn("the app answered no caps (a build older than doctor?): %s", a.CapsError)
		return
	}
	switch n, err := strconv.Atoi(a.Caps["capabilities"]); {
	case err != nil:
		v.warn("the app's caps has no capabilities number: %q", a.Caps["capabilities"])
	case n == 0:
		v.warn("the app read capabilities 0: AVF offers no VMs, or the app couldn't ask")
	case n&capNonProtectedVM == 0:
		v.warn("the app read capabilities %d, without non-protected VMs: the app refuses to start its VM", n)
	}
}

func ptr[T any](v T) *T { return &v }

var kernelBranch = regexp.MustCompile(`^(\d+\.\d+)[.-]`)

// shellVerdict judges the shell facts: fail only on a definite "can't run
// the VM", warn on anything uncertain. Each reason starts with its level.
func shellVerdict(d doctorDevice, a doctorAVF) doctorVerdict {
	v := doctorVerdict{Level: levelOK, Reasons: []string{}}
	fail, warn := v.fail, v.warn

	switch {
	case a.Feature == nil:
		warn("couldn't read the %s feature", avfFeature)
	case !*a.Feature:
		fail("the phone lacks the %s feature", avfFeature)
	}

	switch {
	case a.VMSupported == nil:
		warn("ro.boot.hypervisor.vm.supported is empty or unreadable")
	case !*a.VMSupported && a.ProtectedVMSupported != nil && *a.ProtectedVMSupported:
		fail("the hypervisor runs only protected VMs (ro.boot.hypervisor.vm.supported is false), and pocketnix's VM isn't one")
	case !*a.VMSupported:
		warn("ro.boot.hypervisor.vm.supported is false")
	}
	if a.ProtectedVMSupported == nil {
		warn("ro.boot.hypervisor.protected_vm.supported is empty or unreadable")
	}

	reachedVirtmgr := false
	for _, line := range a.VMInfo {
		switch {
		case strings.HasPrefix(line, "VMs are not supported."), strings.HasPrefix(line, "Only protected VMs are supported."):
			fail("vm info: %s", line)
		// vm info prints these only after spawning virtmgr.
		case strings.HasPrefix(line, "Assignable devices:"), strings.HasPrefix(line, "Available OS list:"):
			reachedVirtmgr = true
		}
	}
	switch {
	case len(a.VMInfo) == 0:
		warn("couldn't run %s info", vmTool)
	case !reachedVirtmgr:
		warn("vm info stopped before asking virtmgr: the shell couldn't reach it")
	}

	switch m := kernelBranch.FindStringSubmatch(deref(d.Kernel)); {
	case d.Kernel == nil:
		warn("couldn't read the kernel release")
	case m == nil:
		warn("kernel %s is on an unknown branch; pocketnix is tested on %s", *d.Kernel, testedKernel)
	case m[1] != testedKernel:
		warn("kernel %s is on the %s branch; pocketnix is tested on %s", *d.Kernel, m[1], testedKernel)
	}

	if !a.UBoot {
		warn("no %s: the app has no bootloader for the image", ubootPath)
	}
	return v
}

// fail makes v a fail, for a reason.
func (v *doctorVerdict) fail(format string, args ...any) {
	v.Level = levelFail
	v.Reasons = append(v.Reasons, "fail: "+fmt.Sprintf(format, args...))
}

// warn makes an ok v a warn, for a reason.
func (v *doctorVerdict) warn(format string, args ...any) {
	if v.Level == levelOK {
		v.Level = levelWarn
	}
	v.Reasons = append(v.Reasons, "warn: "+fmt.Sprintf(format, args...))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// printDoctor writes r for a person; an unreadable fact prints as "?".
func printDoctor(w io.Writer, r doctorReport) {
	str := func(s *string) string {
		if s == nil {
			return "?"
		}
		return *s
	}
	yes := func(b *bool) string {
		switch {
		case b == nil:
			return "?"
		case *b:
			return "yes"
		}
		return "no"
	}
	d, a := r.Device, r.AVF
	fmt.Fprintf(w, "device       %s %s (%s), Android %s (SDK %s), %s\n", str(d.Manufacturer), str(d.Model), str(d.SoC), str(d.Release), str(d.SDK), d.OS)
	fmt.Fprintf(w, "fingerprint  %s\n", str(d.Fingerprint))
	fmt.Fprintf(w, "kernel       %s\n", str(d.Kernel))
	fmt.Fprintf(w, "avf          feature %s, VMs %s, protected VMs %s, hypervisor %s, virt APEX %s, u-boot %s\n",
		yes(a.Feature), yes(a.VMSupported), yes(a.ProtectedVMSupported), str(a.HypervisorVersion), str(a.ApexVersion), yes(&a.UBoot))
	for i, line := range a.VMInfo {
		label := ""
		if i == 0 {
			label = "vm info"
		}
		fmt.Fprintf(w, "%-12s %s\n", label, line)
	}
	app := "not installed"
	if r.App.Installed {
		app = "installed, version " + str(r.App.VersionName)
	}
	fmt.Fprintf(w, "app          %s: %s\n", r.App.Package, app)
	if r.App.Installed {
		caps := r.App.CapsError
		if r.App.Caps != nil {
			var pairs []string
			for k, v := range r.App.Caps {
				pairs = append(pairs, k+"="+v)
			}
			sort.Strings(pairs)
			caps = strings.Join(pairs, " ")
		}
		fmt.Fprintf(w, "caps         %s\n", caps)
	}
	fmt.Fprintf(w, "verdict      %s\n", r.Verdict.Level)
	for _, reason := range r.Verdict.Reasons {
		fmt.Fprintf(w, "  %s\n", reason)
	}
	if r.Smoke != nil {
		fmt.Fprintf(w, "smoke\n")
	}
	for _, st := range r.Smoke {
		result := "skip"
		switch {
		case st.OK == nil:
		case *st.OK:
			result = "ok"
		default:
			result = "FAIL"
		}
		fmt.Fprintf(w, "  %-13s %-5s %s\n", st.Name, result, st.Detail)
	}
}

// smokeTimes bounds smoke's waits; tests shorten them.
var smokeTimes = struct {
	// cmd is a short guest command's output, and a tab's agent.
	cmd time.Duration
	// pty is the big output's end marker.
	pty time.Duration
	// offer is the agent offering the listener.
	offer time.Duration
	// notify and wake are Android showing the guest's notification and wake lock.
	notify, wake time.Duration
	// window, frames and gone are a launched app's window opening, drawing and going.
	window, frames, gone time.Duration
	// exit is a tab's shell ending after exit.
	exit time.Duration
	// forward is an allowed forward ending once its listener is gone:
	// ForwardOffers.GRACE_MS, and some.
	forward time.Duration
	// reboot is the VM going down after systemctl reboot, and boot its agent answering again.
	reboot, boot time.Duration
}{
	cmd: 60 * time.Second, pty: 5 * time.Minute, offer: 20 * time.Second,
	notify: 20 * time.Second, wake: 20 * time.Second,
	window: 60 * time.Second, frames: 20 * time.Second, gone: 20 * time.Second,
	exit: 5 * time.Second, forward: 45 * time.Second,
	reboot: 2 * time.Minute, boot: 5 * time.Minute,
}

const (
	// smokePTYBytes is how much base64 the PTY step prints.
	smokePTYBytes = 64 << 20
	// smokeForwardPort is the guest port the forward step listens on: unprivileged, and rarely taken.
	smokeForwardPort = 47613
	// smokeListenSecs caps the forward step's listener, in case nothing connects.
	smokeListenSecs = 120
	// smokeToken is what the listener answers.
	smokeToken = "pocketnix-smoke-forward"
	// transcriptRows mirrors Tabs.TRANSCRIPT_ROWS: a dump of that many lines holds a whole transcript.
	transcriptRows = 5000
	// rebootLine is the console's line in AvfEngine.onStopped for a guest reboot.
	rebootLine = "[pocketnix: the guest rebooted; starting it again]"
	// wakeLockTag mirrors VmService's wake lock tag.
	wakeLockTag = "pocketnix:guest"
	// guestNotificationTag mirrors GuestNotifications.TAG.
	guestNotificationTag = "guest"
	// gpuFailure is crosvm's line when a GPU context after the first can't map its blob (app/AGENTS.md).
	gpuFailure = "ResourceUnmapBlob: ErrUnspec"
)

// smokePrelude lets a tab's commands reach the user's systemd and session
// bus, which a tab without a login session may have no address for.
const smokePrelude = `export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}; export DBUS_SESSION_BUS_ADDRESS=${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}; `

// The guest scripts, run by sh with "@" replaced by a marker. Each prints
// "@-<key>=<value>" lines for smoke to read, the last one "@-exit=<status>".
const (
	// $1 is the port, $2 the answer and $3 the listener's lifetime in seconds; it serves one connection.
	listenScript = `if command -v socat >/dev/null; then echo @-tool=socat; echo "$2" | timeout $3 socat - TCP-LISTEN:$1,reuseaddr; ` +
		`elif command -v python3 >/dev/null; then echo @-tool=python3; timeout $3 python3 -c "import socket,sys; s=socket.create_server((str(),int(sys.argv[1]))); c=s.accept()[0]; c.sendall(sys.argv[2].encode()); c.close()" "$1" "$2"; ` +
		`elif command -v nc >/dev/null; then echo @-tool=nc; echo "$2" | timeout $3 nc -l "$1"; ` +
		`else echo @-tool=none; fi; echo @-exit=$?`
	// $1 is the title and $2 the body; -p prints the id, which libnotify before 0.7.9 lacks.
	notifyScript = `command -v notify-send >/dev/null || { echo @-tool=none; echo @-exit=0; exit; }; ` +
		`id=$(notify-send -p "$1" "$2" 2>/dev/null || notify-send "$1" "$2"); s=$?; echo @-id=$id; echo @-exit=$s`
	// $1 is the notification's id.
	unnotifyScript = `busctl --user call org.freedesktop.Notifications /org/freedesktop/Notifications org.freedesktop.Notifications CloseNotification u "$1"; echo @-exit=$?`
	inhibitScript  = `command -v systemd-inhibit >/dev/null || { echo @-tool=none; echo @-exit=0; exit; }; ` +
		`systemd-inhibit --what=sleep --who=pocketnix-smoke --why=smoke sleep 600 >/dev/null 2>&1 & p=$!; sleep 1; ` +
		`if kill -0 $p 2>/dev/null; then echo @-pid=$p; else echo @-pid=gone; fi; echo @-exit=0`
	// $1 is systemd-inhibit's pid; its sleep goes first, while it's still the parent.
	uninhibitScript = `pkill -P "$1"; kill "$1"; echo @-exit=$?`
	// $1 is the app id. The scope name is the compositor's launch::spawn unit; a
	// case pattern, unlike systemctl's glob, takes systemd-escape's backslashes literally.
	killAppScript = `e=$(systemd-escape -- "$1"); n=0; ` +
		`for u in $(systemctl --user list-units --plain --no-legend --type=scope "app-pocketnix-*" | cut -d" " -f1); do ` +
		`case "$u" in "app-pocketnix-$e-"*) pkill --cgroup "$(systemctl --user show -P ControlGroup "$u")" && n=$((n+1));; esac; done; ` +
		`echo @-killed=$n; echo @-exit=0`
	rebootScript = `systemctl reboot || sudo -n systemctl reboot; echo @-exit=$?`
)

// shLine is the command line that runs script with args under sh in a
// tab, whatever the user's shell. fish reads \' and \\ even inside single
// quotes, so neither may hold a quote or a backslash.
func shLine(script string, args ...string) (string, error) {
	line := "sh -c '" + script + "' sh"
	for _, a := range append([]string{script}, args...) {
		if strings.ContainsAny(a, `'\`) {
			return "", fmt.Errorf("%q holds a quote or a backslash, which a tab's shell may read otherwise", a)
		}
	}
	for _, a := range args {
		line += " '" + a + "'"
	}
	return line, nil
}

// smoker is one --smoke run against the debug app's running VM, and what
// it made that its cleanup must end. Its adb calls run one at a time: a
// burst of concurrent adb shells has hung a phone.
type smoker struct {
	ctx     context.Context
	dev     adb.Device
	pkg     string
	ctl     control
	gpuApps []string
	n       int
	// tabs are the tabs smoke opened and hasn't closed; util is the one for
	// short commands, -1 until opened.
	tabs []int
	util int
	// launched are the apps smoke launched and hasn't killed.
	launched []string
	// forwarded is set once smoke allowed its forward.
	forwarded bool
	// inhibitor is systemd-inhibit's pid; wakeOurs that the wake lock came from it.
	inhibitor string
	wakeOurs  bool
	// notification is the guest notification's id, when notify-send printed one.
	notification string
}

func stepResult(name string, ok *bool, format string, args ...any) smokeStep {
	return smokeStep{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)}
}

func stepOK(name, format string, args ...any) smokeStep {
	return stepResult(name, ptr(true), format, args...)
}

func stepFail(name, format string, args ...any) smokeStep {
	return stepResult(name, ptr(false), format, args...)
}

// stepSkip is a step that didn't apply, and why.
func stepSkip(name, format string, args ...any) smokeStep {
	return stepResult(name, nil, format, args...)
}

// smokeSteps are runSmoke's step names, in its order.
var smokeSteps = []string{"forward", "pty", "notification", "inhibitor", "gpu", "pages", "cleanup", "reboot"}

// runSmoke runs the end-to-end checks against pkg's running VM, printing
// progress to progress. Cleanup runs even once ctx is done (an interrupt),
// and the guest reboot comes last.
func runSmoke(ctx context.Context, dev adb.Device, pkg string, gpuApps []string, progress io.Writer) []smokeStep {
	s := &smoker{ctx: ctx, dev: dev, pkg: pkg, ctl: control{provider{dev: dev, authority: pkg + ".control"}}, gpuApps: gpuApps, util: -1}
	skipAll := func(format string, args ...any) []smokeStep {
		var steps []smokeStep
		for _, name := range smokeSteps {
			steps = append(steps, stepSkip(name, format, args...))
		}
		return steps
	}
	dump, err := s.ctl.dump(-1, 1)
	if err != nil {
		return skipAll("the debug app didn't answer: %v", err)
	}
	if vm := parseVM(dump); !vm.running() {
		return skipAll("the debug app's VM isn't running (phase=%s): start it first", vm.phase)
	}
	var steps []smokeStep
	run := func(names []string, f func() []smokeStep) {
		if s.ctx.Err() != nil {
			for _, name := range names {
				steps = append(steps, stepSkip(name, "interrupted"))
			}
			return
		}
		fmt.Fprintf(progress, "smoke: %s\n", strings.Join(names, ", "))
		for _, st := range f() {
			if s.ctx.Err() != nil && st.OK != nil && !*st.OK {
				st.Detail = "interrupted: " + st.Detail
			}
			steps = append(steps, st)
		}
	}
	one := func(f func() smokeStep) func() []smokeStep { return func() []smokeStep { return []smokeStep{f()} } }
	run([]string{"forward"}, one(s.forward))
	run([]string{"pty"}, one(s.pty))
	run([]string{"notification"}, one(s.notify))
	run([]string{"inhibitor"}, one(s.inhibit))
	run([]string{"gpu", "pages"}, s.gpu)
	interrupted := s.ctx.Err() != nil
	s.ctx = context.Background()
	fmt.Fprintf(progress, "smoke: cleanup\n")
	steps = append(steps, s.cleanup())
	if interrupted {
		steps = append(steps, stepSkip("reboot", "interrupted"))
	} else {
		run([]string{"reboot"}, one(s.reboot))
	}
	return steps
}

// smokeVM is what a dump says about the VM.
type smokeVM struct {
	phase, gpu    string
	gui, guestRam bool
	apps          []string
}

var (
	dumpPhase  = regexp.MustCompile(`^state=\S+ phase=(\S+)`)
	dumpEngine = regexp.MustCompile(`^engine gpu=(\S+) .*\bguestRam=(\S+)`)
	dumpGUI    = regexp.MustCompile(`^gui connected=true ready=true`)
	dumpApps   = regexp.MustCompile(`^gui apps=\[(.*?)\] recent=`)
	// phaseRunning matches the state line once the agent answered (VmPhase.Running).
	phaseRunning = regexp.MustCompile(`^state=\S+ phase=Running\b`)
)

func parseVM(dump string) smokeVM {
	v := smokeVM{phase: "?"}
	if m := ownLine(dump, dumpPhase); m != nil {
		v.phase = m[1]
	}
	if m := ownLine(dump, dumpEngine); m != nil {
		v.gpu, v.guestRam = m[1], m[2] == "true"
	}
	v.gui = ownLine(dump, dumpGUI) != nil
	if m := ownLine(dump, dumpApps); m != nil && m[1] != "" {
		v.apps = strings.Split(m[1], ", ")
	}
	return v
}

func (v smokeVM) running() bool { return strings.HasPrefix(v.phase, "Running") }

var tabHeader = regexp.MustCompile(`^tab (\d+) .* running=(true|false)$`)

// tabRunning is whether dump lists tab, and whether its session runs.
func tabRunning(dump string, tab int) (listed, running bool) {
	for _, l := range strings.Split(dump, "\n") {
		if m := tabHeader.FindStringSubmatch(l); m != nil && m[1] == strconv.Itoa(tab) {
			return true, m[2] == "true"
		}
	}
	return false, false
}

// pollUntil calls f every controlPoll until it's done or fails; past
// timeout, it returns an error wrapping errNotYet.
func pollUntil(ctx context.Context, timeout time.Duration, f func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := f()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (%s)", errNotYet, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(controlPoll):
		}
	}
}

func (s *smoker) marker() string {
	s.n++
	return fmt.Sprintf("smoke-%d", s.n)
}

// open opens a tab and types line into it, once the agent is up.
func (s *smoker) open(line string) (int, error) {
	extras := append([]string{"timeoutMs:l:" + strconv.FormatInt(smokeTimes.cmd.Milliseconds(), 10)}, inputExtras(line, true)...)
	res, err := s.ctl.call("tab", "", extras...)
	if err != nil {
		return -1, err
	}
	id, err := strconv.Atoi(res["tab"])
	if err != nil {
		return -1, fmt.Errorf("tab: the app answered %q", res["tab"])
	}
	s.tabs = append(s.tabs, id)
	return id, nil
}

// start runs script (with p for "@") and args in a new tab.
func (s *smoker) start(p, script string, args ...string) (int, error) {
	line, err := shLine(strings.ReplaceAll(script, "@", p), args...)
	if err != nil {
		return -1, err
	}
	return s.open(line)
}

var markerLine = regexp.MustCompile(`^(smoke-\d+)-(\w+)=(.*?)\s*$`)

// await waits until tab shows p's line for key, and returns every value
// p's lines gave so far.
func (s *smoker) await(tab int, p, key string, timeout time.Duration) (map[string]string, error) {
	vals := map[string]string{}
	_, err := s.ctl.until(s.ctx, tab, 0, timeout, func(dump string) bool {
		prefix := fmt.Sprintf("|%d| ", tab)
		for _, l := range strings.Split(dump, "\n") {
			t, ok := strings.CutPrefix(l, prefix)
			if !ok {
				continue
			}
			// A wrapped command line can start a row "@-<key>=$…"; a value the shell printed can't.
			if m := markerLine.FindStringSubmatch(t); m != nil && m[1] == p && !strings.HasPrefix(m[3], "$") {
				vals[m[2]] = m[3]
			}
		}
		_, ok := vals[key]
		return ok
	})
	if errors.Is(err, errNotYet) {
		err = fmt.Errorf("tab %d printed no %s-%s line within %s", tab, p, key, timeout)
	}
	return vals, err
}

// guest runs script (after smokePrelude) with args in smoke's tab for
// short commands, and returns its values once it exited.
func (s *smoker) guest(script string, args ...string) (map[string]string, error) {
	p := s.marker()
	line, err := shLine(strings.ReplaceAll(smokePrelude+script, "@", p), args...)
	if err != nil {
		return nil, err
	}
	if s.util < 0 {
		if s.util, err = s.open(line); err != nil {
			return nil, err
		}
	} else if _, err := s.ctl.call("type", strconv.Itoa(s.util), inputExtras(line, true)...); err != nil {
		return nil, err
	}
	vals, err := s.await(s.util, p, "exit", smokeTimes.cmd)
	if err != nil {
		// Its shell may still run the command: the next one gets a tab of its own.
		s.util = -1
	}
	return vals, err
}

var offerLine = regexp.MustCompile(`(?m)^port=(\d+) state=(\w+) phone=(\S+)$`)

// offer is guest port's offer state and phone port, "" when it isn't offered.
func (s *smoker) offer(port int) (state, phone string, err error) {
	out, err := s.ctl.offers()
	if err != nil {
		return "", "", err
	}
	for _, m := range offerLine.FindAllStringSubmatch(out, -1) {
		if m[1] == strconv.Itoa(port) {
			return m[2], m[3], nil
		}
	}
	return "", "", nil
}

// forward has a guest listener offered, allows it, and reaches it from the phone.
func (s *smoker) forward() smokeStep {
	const name = "forward"
	port := strconv.Itoa(smokeForwardPort)
	if state, _, err := s.offer(smokeForwardPort); err != nil {
		return stepFail(name, "offers: %v", err)
	} else if state != "" {
		return stepSkip(name, "guest port %s is offered already (%s): something else listens there", port, state)
	}
	p := s.marker()
	tab, err := s.start(p, listenScript, port, smokeToken, strconv.Itoa(smokeListenSecs))
	if err != nil {
		return stepFail(name, "starting a listener: %v", err)
	}
	vals, err := s.await(tab, p, "tool", smokeTimes.cmd)
	switch {
	case err != nil:
		return stepFail(name, "starting a listener: %v", err)
	case vals["tool"] == "none":
		return stepSkip(name, "the guest has none of nc, socat or python3 to listen with")
	}
	tool := vals["tool"]
	err = pollUntil(s.ctx, smokeTimes.offer, func() (bool, error) {
		state, _, err := s.offer(smokeForwardPort)
		return state != "", err
	})
	if err != nil {
		return stepFail(name, "%s listens on guest port %s, but it wasn't offered: %v (is pocketnix.guest.forwardOffers.enable on?)", tool, port, err)
	}
	if _, err := s.ctl.call("offer", port, "answer:s:allow"); err != nil {
		return stepFail(name, "allowing guest port %s: %v", port, err)
	}
	s.forwarded = true
	_, phone, err := s.offer(smokeForwardPort)
	if err != nil {
		return stepFail(name, "offers: %v", err)
	}
	if _, err := strconv.Atoi(phone); err != nil {
		return stepFail(name, "guest port %s is allowed, without a phone port (phone=%s)", port, phone)
	}
	out, _, err := shellStatus(s.dev, "timeout 10 nc 127.0.0.1 "+phone+" </dev/null")
	if err != nil {
		return stepFail(name, "connecting to phone port %s: %v", phone, err)
	}
	if !strings.Contains(out, smokeToken) {
		return stepFail(name, "phone port %s answered %q, not the guest listener's %q", phone, strings.TrimSpace(out), smokeToken)
	}
	return stepOK(name, "%s on guest port %s answered on phone port %s", tool, port, phone)
}

// pty has a tab print smokePTYBytes of base64.
func (s *smoker) pty() smokeStep {
	const name = "pty"
	p := s.marker()
	start := time.Now()
	// base64 makes 4 bytes of 3. Random lines differ, so outputState can see them move.
	tab, err := s.start(p, fmt.Sprintf(`head -c %d /dev/urandom | base64; echo @-exit=$?`, smokePTYBytes/4*3))
	if err != nil {
		return stepFail(name, "opening a tab: %v", err)
	}
	vals, err := s.await(tab, p, "exit", smokeTimes.pty)
	if err != nil {
		return stepFail(name, "%v; %s", err, s.outputState(tab))
	}
	if vals["exit"] != "0" {
		return stepFail(name, "the output ended with status %s", vals["exit"])
	}
	took := time.Since(start).Round(time.Second)
	dump, err := s.ctl.dump(tab, 1)
	if err != nil {
		return stepFail(name, "after the output: %v", err)
	}
	if vm := parseVM(dump); !vm.running() {
		return stepFail(name, "the VM is %s after the output", vm.phase)
	}
	if _, running := tabRunning(dump, tab); !running {
		return stepFail(name, "tab %d's session ended with the output", tab)
	}
	return stepOK(name, "%d MiB of base64 in %s", smokePTYBytes>>20, took)
}

// outputState says whether tab's last lines still change, which tells output
// that's slow from output that's stuck.
func (s *smoker) outputState(tab int) string {
	tail := func(dump string) string {
		var rows []string
		prefix := fmt.Sprintf("|%d| ", tab)
		for _, l := range strings.Split(dump, "\n") {
			if strings.HasPrefix(l, prefix) {
				rows = append(rows, l)
			}
		}
		return strings.Join(rows, "\n")
	}
	first, err := s.ctl.dump(tab, 3)
	if err != nil {
		return "couldn't read the tab: " + err.Error()
	}
	_, err = s.ctl.until(s.ctx, tab, 3, smokeTimes.exit, func(dump string) bool { return tail(dump) != tail(first) })
	switch {
	case err == nil:
		return "the output was still moving: slow, not stuck"
	case errors.Is(err, errNotYet):
		return "the output had stopped moving"
	}
	return "couldn't read the tab: " + err.Error()
}

// notificationPosted is whether dumpsys notification's lines show a
// notification of pkg's, under the guest's tag, holding text.
func notificationPosted(out, pkg, text string) bool {
	record := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "NotificationRecord(") {
			record = l
		}
		if strings.Contains(l, text) && strings.Contains(record, "pkg="+pkg+" ") && strings.Contains(record, " tag="+guestNotificationTag+" ") {
			return true
		}
	}
	return false
}

// notify posts a guest notification and finds it on the phone.
func (s *smoker) notify() smokeStep {
	const name = "notification"
	b := make([]byte, 4)
	rand.Read(b)
	text := "smoke-" + hex.EncodeToString(b)
	vals, err := s.guest(notifyScript, "pocketnix smoke", text)
	switch {
	case err != nil:
		return stepFail(name, "%v", err)
	case vals["tool"] == "none":
		return stepSkip(name, "the guest has no notify-send")
	case vals["exit"] != "0":
		return stepFail(name, "notify-send exited %s", vals["exit"])
	}
	s.notification = vals["id"]
	err = pollUntil(s.ctx, smokeTimes.notify, func() (bool, error) {
		out, _, err := shellStatus(s.dev, "dumpsys notification --noredact | grep -e NotificationRecord -e android.text=")
		return err == nil && notificationPosted(out, s.pkg, text), err
	})
	if err != nil {
		return stepFail(name, "notify-send ran, but no notification of %s under tag %s holds %q: %v", s.pkg, guestNotificationTag, text, err)
	}
	return stepOK(name, "notify-send's notification shows under tag %s", guestNotificationTag)
}

// wakeLock is whether the app with uids (appUID's) holds its wake lock.
func (s *smoker) wakeLock(uids string) (bool, error) {
	// grep exits 1 when no line matches.
	out, status, err := shellStatus(s.dev, "dumpsys power | grep -F "+wakeLockTag)
	if err != nil || status > 1 {
		return false, fmt.Errorf("dumpsys power: %v (status %d)", err, status)
	}
	owner := regexp.MustCompile(`\buid=(` + strings.ReplaceAll(uids, ",", "|") + `)\b`)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WAKE_LOCK") && owner.MatchString(l) {
			return true, nil
		}
	}
	return false, nil
}

// inhibit takes a sleep inhibitor in the guest and finds the app's wake lock.
func (s *smoker) inhibit() smokeStep {
	const name = "inhibitor"
	uid, err := appUID(s.dev, s.pkg)
	if err != nil {
		return stepFail(name, "%v", err)
	}
	if held, err := s.wakeLock(uid); err != nil {
		return stepFail(name, "%v", err)
	} else if held {
		return stepSkip(name, "the app holds its %s wake lock already, for an inhibitor of the guest's own", wakeLockTag)
	}
	vals, err := s.guest(inhibitScript)
	switch {
	case err != nil:
		return stepFail(name, "%v", err)
	case vals["tool"] == "none":
		return stepSkip(name, "the guest has no systemd-inhibit")
	case vals["pid"] == "gone":
		return stepFail(name, "systemd-inhibit exited at once: may the guest's user take inhibitors (pocketnix.guest.power.wakeLocks.enable)?")
	}
	s.inhibitor = vals["pid"]
	err = pollUntil(s.ctx, smokeTimes.wake, func() (bool, error) { return s.wakeLock(uid) })
	if err != nil {
		return stepFail(name, "the guest holds a sleep inhibitor, but the app took no %s wake lock: %v (wake locks off in the guest, or refused in the app's settings?)", wakeLockTag, err)
	}
	s.wakeOurs = true
	return stepOK(name, "a guest sleep inhibitor holds the app's %s wake lock", wakeLockTag)
}

// counters reads the app's GUI counters.
func (s *smoker) counters() (map[string]float64, error) {
	out, err := s.ctl.stats()
	if err != nil {
		return nil, err
	}
	var st struct {
		Counters map[string]float64 `json:"counters"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	return st.Counters, nil
}

// front brings the app's main screen forward: a window opens as its own
// task only while a pocketnix window shows.
func (s *smoker) front() error {
	// The action mirrors ControlActivity.ACTION_START, which leaves a running VM as it is.
	_, err := s.dev.Shell("am start -n " + s.pkg + "/dev.nickolaj.pocketnix.ControlActivity -a dev.nickolaj.pocketnix.action.START")
	return err
}

// kill ends app's launch scopes from a tab, and waits for its windows to
// go. Unless running, finding no scope is no error: the launch never started.
func (s *smoker) kill(app string, running bool) error {
	vals, err := s.guest(killAppScript, app)
	if err != nil {
		return err
	}
	if vals["killed"] == "0" && running {
		return fmt.Errorf("pkill found no launch scope of %s", app)
	}
	_, err = s.ctl.until(s.ctx, -1, 1, smokeTimes.gone, func(dump string) bool { return len(windows(dump, app)) == 0 })
	if errors.Is(err, errNotYet) {
		err = fmt.Errorf("%s's window stayed %s after pkill", app, smokeTimes.gone)
	}
	if err == nil {
		s.launched = slices.DeleteFunc(s.launched, func(a string) bool { return a == app })
	}
	return err
}

// gpu launches two GPU apps in a row, ending the first with pkill, and
// checks crosvm's log for the second context's failure. The PAGES step
// reads the frames they drew.
func (s *smoker) gpu() []smokeStep {
	skip := func(format string, args ...any) []smokeStep {
		return []smokeStep{stepSkip("gpu", format, args...), stepSkip("pages", format, args...)}
	}
	if len(s.gpuApps) < 2 {
		return skip("needs --gpu-apps <id>,<id>: two app ids from pocketnix control dump's apps=")
	}
	apps := s.gpuApps[:2]
	dump, err := s.ctl.dump(-1, 1)
	if err != nil {
		return skip("the debug app didn't answer: %v", err)
	}
	vm := parseVM(dump)
	switch {
	case !vm.gui:
		return skip("the app has no GUI connection: is GUI on in the guest?")
	}
	for _, app := range apps {
		switch {
		case !slices.Contains(vm.apps, app):
			return skip("%s isn't in the guest's app list (pocketnix control dump's apps=)", app)
		case len(windows(dump, app)) > 0:
			return skip("%s has a window already: close it first, as smoke kills the app it launches", app)
		case strings.ContainsAny(app, `'\`):
			return skip("%s holds a quote or a backslash", app)
		}
	}
	if locked, err := (&phone{dev: s.dev}).locked(); err != nil {
		return skip("%v", err)
	} else if locked {
		return skip("the phone is locked or its screen is off: unlock it, with pocketnix in front (windows open only on a shown display)")
	}
	since, err := s.dev.Shell("date +%s")
	if err != nil {
		return skip("reading the phone's clock: %v", err)
	}
	before, err := s.counters()
	if err != nil {
		return skip("%v", err)
	}
	problem, drew := s.launchBoth(apps)
	var log strings.Builder
	logErr := logcatSince(s.dev, s.pkg, strings.TrimSpace(since), &log)
	after, err := s.counters()
	if err != nil && problem == "" {
		problem = err.Error()
	}

	var gpu smokeStep
	switch {
	case vm.gpu == "gfxstream" && strings.Contains(log.String(), gpuFailure):
		gpu = stepFail("gpu", "crosvm logged %s: a GPU context after the first failed (app/AGENTS.md, gfxstream)", gpuFailure)
		if problem != "" {
			gpu.Detail += "; " + problem
		}
	case problem != "":
		gpu = stepFail("gpu", "%s", problem)
	case vm.gpu != "gfxstream":
		gpu = stepSkip("gpu", "the VM has no GPU (engine gpu=%s), so no GPU context was tested; %s, then %s after pkill, drew frames in software", vm.gpu, apps[0], apps[1])
	case logErr != nil:
		gpu = stepFail("gpu", "both drew, but crosvm's log couldn't be read: %v", logErr)
	default:
		gpu = stepOK("gpu", "%s, then %s after pkill, drew frames on gfxstream; crosvm logged no %s", apps[0], apps[1], gpuFailure)
	}
	var pages smokeStep
	pagesN, commits := after["pagesCommits"]-before["pagesCommits"], 0.0
	for _, c := range []string{"rawCommits", "zstdCommits", "pagesCommits"} {
		commits += after[c] - before[c]
	}
	switch {
	case !vm.guestRam:
		pages = stepSkip("pages", "guest RAM is off (engine guestRam=false): frames go over the stream")
	case !drew:
		pages = stepSkip("pages", "no window drew a frame")
	case pagesN > 0:
		pages = stepOK("pages", "%.0f of %.0f commits with pixels were PAGES", pagesN, commits)
	default:
		pages = stepFail("pages", "none of %.0f commits with pixels was PAGES, with guest RAM on (an app without the pagesCommits counter reads 0 too)", commits)
	}
	return []smokeStep{gpu, pages}
}

// launchBoth launches apps in turn, each drawing before the first is
// killed; problem is what went wrong, drew whether any window drew.
func (s *smoker) launchBoth(apps []string) (problem string, drew bool) {
	for i, app := range apps {
		if err := s.front(); err != nil {
			return err.Error(), drew
		}
		s.launched = append(s.launched, app)
		line, err := s.ctl.launch(s.ctx, app, smokeTimes.window)
		if err != nil {
			return err.Error(), drew
		}
		id := guiWindow.FindStringSubmatch(line)[1]
		_, err = s.ctl.until(s.ctx, -1, 1, smokeTimes.frames, func(dump string) bool {
			for _, l := range strings.Split(dump, "\n") {
				if m := guiWindow.FindStringSubmatch(l); m != nil && m[1] == id && windowDrew(l) {
					return true
				}
			}
			return false
		})
		if err != nil {
			return fmt.Sprintf("%s's window %s drew no frame: %v", app, id, err), drew
		}
		drew = true
		if i == 0 {
			if err := s.kill(app, true); err != nil {
				return err.Error(), drew
			}
		}
	}
	return "", drew
}

var windowSurface = regexp.MustCompile(`\d+:\w+:(\d+)x(\d+)`)

// windowDrew is whether a dump's window line shows a surface with content:
// its size comes from the buffers the window committed.
func windowDrew(line string) bool {
	_, surfaces, _ := strings.Cut(line, " surfaces=[")
	for _, m := range windowSurface.FindAllStringSubmatch(surfaces, -1) {
		if m[1] != "0" && m[2] != "0" {
			return true
		}
	}
	return false
}

// cleanup ends what smoke made: its apps, inhibitor, notification, tabs
// and forward. It fails naming what it couldn't end.
func (s *smoker) cleanup() smokeStep {
	var left, ended []string
	for _, app := range slices.Clone(s.launched) {
		if err := s.kill(app, false); err != nil {
			left = append(left, fmt.Sprintf("app %s: %v", app, err))
		} else {
			ended = append(ended, "app "+app)
		}
	}
	if s.inhibitor != "" {
		if _, err := s.guest(uninhibitScript, s.inhibitor); err != nil {
			left = append(left, fmt.Sprintf("inhibitor %s: %v", s.inhibitor, err))
		} else {
			ended = append(ended, "the inhibitor")
		}
		if s.wakeOurs {
			uid, err := appUID(s.dev, s.pkg)
			if err == nil {
				err = pollUntil(s.ctx, smokeTimes.wake, func() (bool, error) {
					held, err := s.wakeLock(uid)
					return !held, err
				})
			}
			if err != nil {
				left = append(left, fmt.Sprintf("the %s wake lock: %v", wakeLockTag, err))
			}
		}
	}
	if s.notification != "" {
		if vals, err := s.guest(unnotifyScript, s.notification); err != nil || vals["exit"] != "0" {
			left = append(left, fmt.Sprintf("notification %s: busctl exited %s: %v", s.notification, vals["exit"], err))
		} else {
			ended = append(ended, "the notification")
		}
	}
	tabs := len(s.tabs)
	for _, tab := range slices.Clone(s.tabs) {
		if err := s.closeTab(tab); err != nil {
			left = append(left, fmt.Sprintf("tab %d: %v", tab, err))
		}
	}
	if n := tabs - len(s.tabs); n > 0 {
		ended = append(ended, fmt.Sprintf("%d tabs", n))
	}
	if s.forwarded {
		err := pollUntil(s.ctx, smokeTimes.forward, func() (bool, error) {
			state, _, err := s.offer(smokeForwardPort)
			return state != "allowed", err
		})
		if err != nil {
			left = append(left, fmt.Sprintf("the forward of guest port %d: %v", smokeForwardPort, err))
		} else {
			ended = append(ended, "the forward")
		}
	}
	if len(left) > 0 {
		return stepFail("cleanup", "left %s", strings.Join(left, "; "))
	}
	if len(ended) == 0 {
		return stepOK("cleanup", "nothing to end")
	}
	return stepOK("cleanup", "ended %s", strings.Join(ended, ", "))
}

// closeTab types exit into tab, gives its shell a moment to end, and
// closes it, which hangs up whatever still runs there. A tab that's gone
// already counts as closed.
func (s *smoker) closeTab(tab int) error {
	if tab == s.util {
		s.util = -1
	}
	dump, err := s.ctl.dump(tab, 1)
	if err != nil && strings.Contains(err.Error(), fmt.Sprintf("no tab %d", tab)) {
		s.tabs = slices.DeleteFunc(s.tabs, func(t int) bool { return t == tab })
		return nil
	}
	if err != nil {
		return err
	}
	if _, running := tabRunning(dump, tab); running {
		if _, err := s.ctl.call("type", strconv.Itoa(tab), inputExtras("exit", true)...); err != nil {
			return err
		}
		s.ctl.until(s.ctx, tab, 1, smokeTimes.exit, func(dump string) bool {
			_, running := tabRunning(dump, tab)
			return !running
		})
	}
	if err := s.ctl.close(tab); err != nil {
		return err
	}
	s.tabs = slices.DeleteFunc(s.tabs, func(t int) bool { return t == tab })
	return nil
}

// countLine is how many of tab's transcript lines in dump hold text.
func countLine(dump string, tab int, text string) int {
	n := 0
	prefix := fmt.Sprintf("|%d| ", tab)
	for _, l := range strings.Split(dump, "\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l, text) {
			n++
		}
	}
	return n
}

// reboot reboots the guest from a tab, and waits for the app to boot it
// again and its agent to answer.
func (s *smoker) reboot() smokeStep {
	const name = "reboot"
	// Tab 0 is the console.
	console, err := s.ctl.dump(0, transcriptRows)
	if err != nil {
		return stepFail(name, "reading the console: %v", err)
	}
	before := countLine(console, 0, rebootLine)
	p := s.marker()
	start := time.Now()
	tab, err := s.start(p, rebootScript)
	if err != nil {
		return stepFail(name, "opening a tab: %v", err)
	}
	failed := regexp.MustCompile("^" + regexp.QuoteMeta(p) + `-exit=([1-9]\d*)\s*$`)
	var status []string
	// The agent goes before the VM stops, and the phase with it; small dumps
	// keep the polling light, and the console is read once at the end.
	err = pollUntil(s.ctx, smokeTimes.reboot, func() (bool, error) {
		dump, err := s.ctl.dump(-1, 1)
		if err != nil || !parseVM(dump).running() {
			return err == nil, err
		}
		if dump, err = s.ctl.dump(tab, 0); err != nil {
			return false, err
		}
		status = tabLine(dump, tab, failed)
		return status != nil, nil
	})
	switch {
	case status != nil:
		return stepFail(name, "systemctl reboot exited %s (and sudo -n)", status[1])
	case err != nil:
		return stepFail(name, "the VM stayed Running after systemctl reboot: %v", err)
	}
	if _, _, err := s.ctl.waitDump(s.ctx, phaseRunning, smokeTimes.boot); err != nil {
		return stepFail(name, "the guest went down, but its agent didn't answer again: %v", err)
	}
	took := time.Since(start).Round(time.Second)
	if console, err = s.ctl.dump(0, transcriptRows); err != nil {
		return stepFail(name, "reading the console: %v", err)
	}
	if countLine(console, 0, rebootLine) <= before {
		return stepFail(name, "the agent answered again in %s, but the console has no new %q: the app didn't see a reboot", took, rebootLine)
	}
	if err := s.closeTab(tab); err != nil {
		return stepFail(name, "rebooted, Running again in %s, but its ended tab %d didn't close: %v", took, tab, err)
	}
	return stepOK(name, "rebooted, and Running again in %s", took)
}
