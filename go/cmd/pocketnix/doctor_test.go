package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// doctorJSON runs doctor --json with args and decodes its report, and the
// same JSON untyped, to check its field names.
func doctorJSON(t *testing.T, args ...string) (doctorReport, map[string]any, error) {
	t.Helper()
	out, err := stdoutOf(t, func() error { return runDoctor(append([]string{"--json"}, args...)) })
	var r doctorReport
	var raw map[string]any
	if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
		t.Fatalf("doctor --json printed no report (%v; run: %v):\n%s", jerr, err, out)
	}
	json.Unmarshal([]byte(out), &raw)
	return r, raw, err
}

// field is the JSON value at a dotted path, nil if absent.
func field(raw map[string]any, path string) any {
	var v any = raw
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// cannedPixel makes the fake device answer like the maintainer's Pixel 8 Pro
// on GrapheneOS, with its real answers.
func cannedPixel(d *fakeDevice) {
	for name, v := range map[string]string{
		"ro.build.fingerprint":                      "google/husky/husky:17/CP3A.261005.005/2026101601:user/release-keys",
		"ro.product.model":                          "Pixel 8 Pro",
		"ro.product.manufacturer":                   "Google",
		"ro.soc.manufacturer":                       "Google",
		"ro.soc.model":                              "Tensor G3",
		"ro.build.version.sdk":                      "37",
		"ro.build.version.release":                  "17",
		"ro.boot.hypervisor.vm.supported":           "1",
		"ro.boot.hypervisor.protected_vm.supported": "1",
		"ro.boot.hypervisor.version":                "kvm.arm-protected",
	} {
		d.setProp(name, v)
	}
	d.addPackages("app.grapheneos.apps")
	d.addFile("/apex/com.android.virt/etc/u-boot.bin", []byte("u-boot"))
	d.can(avfFeatureCommand, "true\n", 0)
	d.can("uname -r", "6.1.177-android14-11-ga001019bb34e\n", 0)
	d.can("/apex/com.android.virt/bin/vm info", `Both protected and non-protected VMs are supported.
Hypervisor version: kvm.arm-protected
/dev/kvm exists.
/dev/vfio/vfio does not exist.
VFIO-platform is not supported.
Assignable devices: []
Available OS list: ["microdroid"]
Debug policy: Ok(DebugPolicy { log: false, ramdump: false, adb: false })
Updatable VM is supported.
`, 0)
	d.can("pm list packages --apex-only --show-versioncode", `package:com.android.hardware.biometrics.fingerprint.virtual versionCode:1
package:com.android.virt versionCode:370399999
package:com.android.hardware.biometrics.face.virtual versionCode:2
`, 0)
}

func TestDoctorJSON(t *testing.T) {
	d := newFakeDevice(t)
	cannedPixel(d)
	_, raw, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"schema": float64(1), "device.model": "Pixel 8 Pro", "device.os": "grapheneos", "avf.vmSupported": true,
		"avf.protectedVmSupported": true, "verdict.level": "ok",
		"avf.apexVersion": "370399999", "device.soc": "Google Tensor G3", "avf.uboot": true,
	} {
		if got := field(raw, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if _, ok := field(raw, "verdict.reasons").([]any); !ok {
		t.Errorf("verdict.reasons isn't a list: %v", field(raw, "verdict.reasons"))
	}
}

// isNull reports whether the JSON has an explicit null at path.
func isNull(raw map[string]any, path string) bool {
	parent, key := path, path
	var m map[string]any = raw
	if i := strings.LastIndex(path, "."); i >= 0 {
		parent, key = path[:i], path[i+1:]
		m, _ = field(raw, parent).(map[string]any)
	}
	v, ok := m[key]
	return ok && v == nil
}

func TestDoctorVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(d *fakeDevice)
		level string
		// token is in some reason.
		token string
		// nulls are fields that read as null.
		nulls []string
	}{
		{name: "pixel-grapheneos", setup: cannedPixel, level: levelOK},
		{name: "protected-only", level: levelFail, token: "protected", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.setProp("ro.boot.hypervisor.vm.supported", "0")
			d.can("/apex/com.android.virt/bin/vm info", "Only protected VMs are supported.\nHypervisor version: kvm.arm-protected\n", 0)
		}},
		// The props alone are definite too.
		{name: "protected-only-props", level: levelFail, token: "vm.supported", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.setProp("ro.boot.hypervisor.vm.supported", "false")
			d.setProp("ro.boot.hypervisor.protected_vm.supported", "true")
			d.can("/apex/com.android.virt/bin/vm info", "", 127)
		}},
		{name: "no-feature", level: levelFail, token: "virtualization_framework", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.denyAVF()
		}},
		{name: "empty-props", level: levelWarn, token: "vm.supported", setup: func(d *fakeDevice) {
			cannedPixel(d)
			for _, p := range []string{"ro.product.model", "ro.boot.hypervisor.vm.supported", "ro.boot.hypervisor.protected_vm.supported", "ro.boot.hypervisor.version"} {
				d.setProp(p, "")
			}
		}, nulls: []string{"device.model", "avf.vmSupported", "avf.protectedVmSupported", "avf.hypervisorVersion"}},
		{name: "kernel-6.6", level: levelWarn, token: "6.6", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.can("uname -r", "6.6.50-android15-8-gabcdef012345\n", 0)
		}},
		// vm info's sysprop lines, then virtmgr couldn't be spawned.
		{name: "no-virtmgr", level: levelWarn, token: "virtmgr", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.can("/apex/com.android.virt/bin/vm info", `Both protected and non-protected VMs are supported.
Hypervisor version: kvm.arm-protected
/dev/kvm exists.
/dev/vfio/vfio does not exist.
VFIO-platform is not supported.
`, 1)
		}},
		{name: "no-uboot", level: levelWarn, token: "u-boot", setup: func(d *fakeDevice) {
			cannedPixel(d)
			d.can("ls /apex/com.android.virt/etc/u-boot.bin", "ls: /apex/com.android.virt/etc/u-boot.bin: No such file or directory\n", 1)
		}},
		// Every command unknown to the shell: nothing is definite.
		{name: "unreadable", setup: func(*fakeDevice) {}, level: levelWarn,
			nulls: []string{"avf.feature", "avf.vmSupported", "device.kernel", "avf.apexVersion"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			tc.setup(d)
			r, raw, err := doctorJSON(t)
			if r.Verdict.Level != tc.level {
				t.Errorf("verdict = %+v, want %s", r.Verdict, tc.level)
			}
			var status exitStatus
			if tc.level == levelFail {
				if !errors.As(err, &status) || status != 1 {
					t.Errorf("a fail returned %v, want exit status 1", err)
				}
			} else if err != nil {
				t.Errorf("a %s returned %v", tc.level, err)
			}
			if tc.token != "" && !strings.Contains(strings.Join(r.Verdict.Reasons, "\n"), tc.token) {
				t.Errorf("no reason names %q: %q", tc.token, r.Verdict.Reasons)
			}
			for _, p := range tc.nulls {
				if !isNull(raw, p) {
					t.Errorf("%s = %v, want null", p, field(raw, p))
				}
			}
		})
	}
}

// The serial reaches neither the report nor the text.
func TestDoctorLeavesOutTheSerial(t *testing.T) {
	const serial = "TESTSERIAL123"
	d := newFakeDevice(t)
	cannedPixel(d)
	d.setProp("ro.serialno", serial)
	d.setProp("ro.boot.serialno", serial)
	for _, args := range [][]string{{"--json", "--serial", serial}, {"--serial", serial}} {
		out, err := stdoutOf(t, func() error { return runDoctor(args) })
		if err != nil {
			t.Fatal(err)
		}
		if out == "" || strings.Contains(out, serial) {
			t.Errorf("doctor %v printed the serial, or nothing:\n%s", args, out)
		}
	}

	// A phone that drops mid-run has adb name it in the errors the report quotes.
	for _, tc := range []struct {
		name, serial, env string
		args              []string
		// match is the command adb fails.
		match string
	}{
		{name: "flag", serial: serial, args: []string{"--serial", serial}, match: "--method caps"},
		{name: "env", serial: serial, env: serial, match: "--method caps"},
		{name: "transport", serial: fakeSerial, match: "--method caps"},
		{name: "smoke", serial: serial, args: []string{"--serial", serial, "--smoke"}, match: "--method tab"},
	} {
		for _, mode := range [][]string{{"--json"}, nil} {
			t.Run(fmt.Sprintf("%s %v", tc.name, mode), func(t *testing.T) {
				d := smokeDevice(t, everyTool...)
				d.installAPK([]byte("apk"))
				d.setCaps(pixelCaps)
				t.Setenv("ANDROID_SERIAL", tc.env)
				d.drop(tc.match, "", "adb: device '"+tc.serial+"' not found\n")
				out, _ := stdoutOf(t, func() error { return runDoctor(append(slices.Clone(mode), tc.args...)) })
				if !strings.Contains(out, "not found") {
					t.Errorf("the report lost adb's complaint:\n%s", out)
				}
				if strings.Contains(out, tc.serial) {
					t.Errorf("the report holds the serial:\n%s", out)
				}
			})
		}
	}

	t.Run("record", func(t *testing.T) {
		d := recordDevice(t)
		d.installAPK([]byte("apk"))
		d.drop("--method caps", "", "adb: device '"+recordSerial+"' not found\n")
		dir := t.TempDir()
		out, _ := stdoutOf(t, func() error { return runDoctor([]string{"--json", "--serial", recordSerial, "--record", dir}) })
		if !strings.Contains(out, "not found") || strings.Contains(out, recordSerial) {
			t.Errorf("stdout under --record:\n%s", out)
		}
		assertNoSerial(t, onlyBundle(t, dir))
	})
}

func TestDoctorApp(t *testing.T) {
	d := newFakeDevice(t)
	cannedPixel(d)
	r, raw, _ := doctorJSON(t)
	if r.App.Package != releasePackage || r.App.Installed || !isNull(raw, "app.versionName") {
		t.Errorf("no app: %+v", r.App)
	}

	d.installAPKAs(debugPackage, []byte("apk"))
	d.can("dumpsys package dev.nickolaj.pocketnix.debug", "Packages:\n  Package [dev.nickolaj.pocketnix.debug] (1a2b3c):\n    versionCode=42 minSdk=34 targetSdk=36\n    versionName=0.4.0-dev\n", 0)
	r, _, _ = doctorJSON(t, "--debug")
	if r.App.Package != debugPackage || !r.App.Installed || r.App.VersionName == nil || *r.App.VersionName != "0.4.0-dev" {
		t.Errorf("the debug app: %+v", r.App)
	}
}

func TestDoctorText(t *testing.T) {
	d := newFakeDevice(t)
	cannedPixel(d)
	d.denyAVF()
	out, err := stdoutOf(t, func() error { return runDoctor(nil) })
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("a fail returned %v, want exit status 1", err)
	}
	for _, token := range []string{"Pixel 8 Pro", levelFail, "virtualization_framework"} {
		if !strings.Contains(out, token) {
			t.Errorf("the text lacks %q:\n%s", token, out)
		}
	}
}

// pixelCaps is a caps answer as the app on the maintainer's Pixel should
// give it in a cold process (GrapheneOS 17's GpuConfig takes boxed Booleans).
const pixelCaps = "capabilities=3, manager=true, gpuConfig=boxed, rawConfigBinder=true, autoMemoryBalloon=true, engine=engine+launched%3Dnone, appVersion=0.4.0-debug"

func TestDoctorCaps(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		d := newFakeDevice(t)
		cannedPixel(d)
		d.installAPK([]byte("apk"))
		d.setCaps(pixelCaps)
		r, _, err := doctorJSON(t)
		if err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{"capabilities": "3", "gpuConfig": "boxed", "rawConfigBinder": "true", "engine": "engine launched=none", "appVersion": "0.4.0-debug"} {
			if got := r.App.Caps[k]; got != want {
				t.Errorf("app.caps.%s = %q, want %q", k, got, want)
			}
		}
		if _, ok := r.App.Caps["ok"]; ok || r.App.CapsError != "" {
			t.Errorf("app.caps = %v, capsError %q", r.App.Caps, r.App.CapsError)
		}
		if r.Verdict.Level != levelOK {
			t.Errorf("verdict = %+v", r.Verdict)
		}
		out, err := stdoutOf(t, func() error { return runDoctor(nil) })
		if err != nil || !strings.Contains(out, "gpuConfig=boxed") {
			t.Errorf("the text lacks the caps (%v):\n%s", err, out)
		}
	})

	for _, tc := range []struct {
		name string
		// caps is the answer; empty for an app from before caps.
		caps string
		// token is in some reason.
		token string
	}{
		{name: "before-caps", token: "unknown method caps"},
		// 0 is also what a failed getCapabilities gives.
		{name: "capabilities-0", caps: strings.Replace(pixelCaps, "capabilities=3", "capabilities=0", 1), token: "capabilities 0"},
		{name: "protected-only", caps: strings.Replace(pixelCaps, "capabilities=3", "capabilities=1", 1), token: "non-protected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			cannedPixel(d)
			d.installAPK([]byte("apk"))
			if tc.caps != "" {
				d.setCaps(tc.caps)
			}
			r, _, err := doctorJSON(t)
			if err != nil {
				t.Errorf("a warn returned %v, want exit status 0", err)
			}
			if r.Verdict.Level != levelWarn {
				t.Errorf("verdict = %+v, want warn", r.Verdict)
			}
			if !strings.Contains(strings.Join(r.Verdict.Reasons, "\n"), tc.token) {
				t.Errorf("no reason names %q: %q", tc.token, r.Verdict.Reasons)
			}
			if tc.caps == "" && (r.App.Caps != nil || !strings.Contains(r.App.CapsError, "unknown method caps")) {
				t.Errorf("an app without caps: caps %v, capsError %q", r.App.Caps, r.App.CapsError)
			}
		})
	}
}

// shortenSmoke sets smoke's waits for a fake device that answers at once.
func shortenSmoke() {
	controlPoll = 5 * time.Millisecond
	s := time.Second
	// Unkeyed, so that a new wait doesn't compile until it's shortened here too.
	smokeTimes = smokeWaits{s, s, s, s, s, s, s, s, s, s, s, s, s}
	smokeTimes.ptyPoll = controlPoll
	// outputState waits this long once, between its two reads.
	smokeTimes.exit = 50 * time.Millisecond
}

// fastSmoke is shortenSmoke for one test.
func fastSmoke(t *testing.T) {
	poll, times := controlPoll, smokeTimes
	t.Cleanup(func() { controlPoll, smokeTimes = poll, times })
	shortenSmoke()
}

// cannedPath is the state path of the shell command's canned answer, for a reaction to write.
func cannedPath(cmdline string) string { return filepath.Join("canned", fakeCannedKey(cmdline)) }

// powerHistory is what dumpsys power keeps of a wake lock after its release, as a Pixel prints it.
const powerHistory = "  11-02 16:59:16.878 - 10200 (dev.nickolaj.pocketnix.debug) - ACQ pocketnix:guest (partial)\n  11-02 17:00:12.971 - 10200 (dev.nickolaj.pocketnix.debug) - REL pocketnix:guest\n"

// writeCanned is a reaction's Write making cmdline answer out with status.
func writeCanned(cmdline, out string, status int) map[string]string {
	return map[string]string{cannedPath(cmdline): out, cannedPath(cmdline) + ".status": strconv.Itoa(status)}
}

// typedScript is a fakeReaction's Typed for script as smoke types it into a
// tab, through shLine, with smokePrelude or without. Its first marker is ${1},
// and args are regexps for its arguments, whose groups come next. Only a
// script's output is the fake's business: real sh runs what a script does.
func typedScript(script string, args ...string) string {
	holes := make([]string, len(args))
	for i := range args {
		holes[i] = fmt.Sprintf("<arg-%d>", i)
	}
	line, err := shLine(script, holes...)
	if err != nil {
		panic(err)
	}
	head, tail, _ := strings.Cut(line, script)
	re := "^" + regexp.QuoteMeta(head) + "(?:" + regexp.QuoteMeta(smokePrelude) + ")?" + regexp.QuoteMeta(script+tail) + "$"
	for i, a := range args {
		re = strings.Replace(re, holes[i], a, 1)
	}
	re = strings.Replace(re, "@", `(smoke-\d+)`, 1)
	return strings.ReplaceAll(re, "@", `smoke-\d+`)
}

// Reactions of a guest with every tool smoke uses.
var (
	reactListen = fakeReaction{Typed: typedScript(listenScript, `(\d+)`, `([^']+)`, `\d+`),
		Output: []string{"${1}-tool=socat"}, Write: map[string]string{"control/listening/${2}": "${3}"}}
	reactPTY = fakeReaction{Typed: typedScript(ptyScript(smokePTYFile, smokePTYHead)), Output: []string{"AAAAAAAAAAAA", "${1}-exit=0"},
		Write: map[string]string{"control/guest-pty": "1000\n"}}
	reactNotify = fakeReaction{Typed: typedScript(notifyScript, `[^']*`, `([^']+)`), Output: []string{"${1}-id=7", "${1}-exit=0"},
		Append: map[string]string{"notifications": "  NotificationRecord(0x0abc1234: pkg=dev.nickolaj.pocketnix.debug user=UserHandle{0} id=7 tag=guest importance=3 key=0|dev.nickolaj.pocketnix.debug|7|guest|10200: Notification(channel=guest))\n        android.text=String (${2})\n"}}
	reactUnnotify = fakeReaction{Typed: typedScript(unnotifyScript, "7"), Output: []string{"${1}-exit=0"},
		Write: map[string]string{"notifications": ""}}
	reactInhibit = fakeReaction{Typed: typedScript(inhibitScript), Output: []string{"${1}-pid=4242", "${1}-exit=0"},
		Write: writeCanned(wakeLockCommand, "    PARTIAL_WAKE_LOCK              'pocketnix:guest'                          ACQ=-1s2ms (uid=10200 pid=31337)\n", 0)}
	reactUninhibit = fakeReaction{Typed: typedScript(uninhibitScript, "4242"), Output: []string{"${1}-exit=0"},
		Write: writeCanned(wakeLockCommand, powerHistory, 0)}
	reactKill = fakeReaction{Typed: typedScript(killAppScript, `([^']+)`), Output: []string{"${1}-killed=1", "${1}-exit=0"},
		Remove: []string{"control/windows/${2}"}}
	reactReboot = fakeReaction{Typed: typedScript(rebootScript), Reboot: true}
)

// smokeApps are the guest's GPU apps in smokeDevice.
var smokeApps = []string{"org.example.Cube", "org.example.Gears"}

// smokeDevice is the maintainer's Pixel with the debug app's VM running,
// GUI on, guest RAM and a GPU, unlocked; its guest answers rs.
func smokeDevice(t *testing.T, rs ...fakeReaction) *fakeDevice {
	t.Helper()
	fastSmoke(t)
	d := newFakeDevice(t)
	cannedPixel(d)
	d.newAppModel()
	d.setGUI(smokeApps...)
	putFile(filepath.Join(d.state, "control", "pages"), "")
	d.react(rs...)
	d.can(wakeLockCommand, powerHistory, 0)
	// One uid per Android user, as on a phone with a work profile or Private space.
	d.can("pm list packages -U dev.nickolaj.pocketnix.debug", "package:dev.nickolaj.pocketnix.debug uid:10200,1010200,1110200\n", 0)
	d.can(wakefulnessCommand, "  mWakefulness=Awake\n", 0)
	d.can(keyguardCommand, "    isKeyguardShowing=false\n", 0)
	d.can("date +%s", "1762000000\n", 0)
	// An earlier boot's GPU failure, before smoke ran.
	d.write("logcat", []byte("1761999000.000  4321  4400 E crosvm  : ResourceUnmapBlob: ErrUnspec\n"))
	return d
}

// The guest's PTY timing file: base64 ran for 100 s, read 130 s after it started.
var (
	reactPTYTime = fakeReaction{Typed: typedScript(ptyTimeScript),
		Output: []string{"${1}-start=1000", "${1}-end=1100", "${1}-now=1130", "${1}-exit=0"}}
	// base64 hasn't exited by the time the file is read.
	reactPTYRunning = fakeReaction{Typed: reactPTYTime.Typed,
		Output: []string{"${1}-start=1000", "${1}-end=", "${1}-now=1600", "${1}-exit=0"}}
	reactPTYClean = fakeReaction{Typed: typedScript(ptyCleanScript), Output: []string{"${1}-exit=0"},
		Remove: []string{"control/guest-pty"}}
)

var everyTool = []fakeReaction{reactListen, reactPTY, reactPTYTime, reactPTYClean, reactNotify, reactUnnotify, reactInhibit, reactUninhibit, reactKill, reactReboot}

func stepNamed(steps []smokeStep, name string) (smokeStep, bool) {
	for _, s := range steps {
		if s.Name == name {
			return s, true
		}
	}
	return smokeStep{}, false
}

// leftovers is what smoke left on the fake device: tabs other than the
// console, windows, offers and the wake lock.
func leftovers(d *fakeDevice) []string {
	var left []string
	for _, id := range fakeTabs(d.state) {
		if id != 0 {
			left = append(left, fmt.Sprintf("tab %d", id))
		}
	}
	for _, app := range fakeWindows(d.state) {
		left = append(left, "window "+app)
	}
	if o := fakeOffers(d.state); o != "" {
		left = append(left, "offer "+o)
	}
	if b, _ := os.ReadFile(filepath.Join(d.state, cannedPath(wakeLockCommand))); strings.Contains(string(b), "WAKE_LOCK") {
		left = append(left, "wake lock")
	}
	if exists(filepath.Join(d.state, "control", "guest-pty")) {
		left = append(left, "the guest's PTY timing file")
	}
	return left
}

// exitedEveryTab checks that each tab smoke opened before the reboot got exit typed.
func exitedEveryTab(t *testing.T, d *fakeDevice) {
	t.Helper()
	typed := d.typed()
	if len(typed) == 0 {
		t.Fatal("smoke typed into no tab")
	}
	for tab, lines := range typed {
		if isReboot(lines[0]) {
			continue
		}
		// A leading space keeps a line out of fish's history.
		if !slices.Contains(lines, " exit") {
			t.Errorf("tab %d got no exit: %q", tab, lines)
		}
	}
}

func without(rs []fakeReaction, drop fakeReaction) []fakeReaction {
	var out []fakeReaction
	for _, r := range rs {
		if r.Typed != drop.Typed {
			out = append(out, r)
		}
	}
	return out
}

func TestSmokeRunsEveryStep(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	r, raw, err := doctorJSON(t, "--smoke", "--gpu-apps", strings.Join(smokeApps, ","))
	if err != nil {
		t.Errorf("doctor --smoke: %v", err)
	}
	for _, name := range smokeSteps {
		if st, ok := stepNamed(r.Smoke, name); !ok || st.OK == nil || !*st.OK {
			t.Errorf("step %s: %+v (found %v)", name, st, ok)
		}
	}
	if steps, ok := field(raw, "smoke").([]any); !ok || len(steps) == 0 {
		t.Errorf("the JSON has no smoke list: %v", field(raw, "smoke"))
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
	exitedEveryTab(t, d)
	for tab, lines := range d.typed() {
		for _, l := range lines {
			if !strings.HasPrefix(l, " ") {
				t.Errorf("tab %d: %q would land in the shell's history", tab, l)
			}
		}
	}
	if st, _ := stepNamed(r.Smoke, "pty"); !strings.Contains(st.Detail, "100s") {
		t.Errorf("pty: %+v, want base64's time in the guest", st)
	}
	if fakeReboots(d.state) != 1 {
		t.Errorf("the guest rebooted %d times", fakeReboots(d.state))
	}
	for _, e := range []string{"uninstall", "am force-stop"} {
		if hasEvent(d.events(), e) {
			t.Errorf("smoke ran %s:\n%s", e, d.events())
		}
	}
}

// ptyTab is the tab the PTY command was typed into, -1 if none.
func ptyTab(d *fakeDevice) int {
	for tab, lines := range d.typed() {
		if reactPTY.matches(lines[0]) {
			return tab
		}
	}
	return -1
}

// A device's strings can hold escape sequences, which reach a terminal only as text.
func TestDoctorTextHasNoControlCharacters(t *testing.T) {
	const hostile = "Pixel\x1b]52;c;aGk=\x07\x1b[2J\r\u009b8"
	d := newFakeDevice(t)
	cannedPixel(d)
	d.setProp("ro.product.model", hostile)
	d.can("/apex/com.android.virt/bin/vm info", "Only protected VMs are supported."+hostile+"\n", 0)
	out, _ := stdoutOf(t, func() error { return runDoctor(nil) })
	if strings.ContainsAny(out, "\x1b\x07\r\u009b") {
		t.Errorf("the text holds control characters: %q", out)
	}
	if !strings.Contains(out, "Pixel]52;c;aGk=[2J8") {
		t.Errorf("the text lost the model: %q", out)
	}
	r, _, _ := doctorJSON(t)
	if r.Device.Model == nil || *r.Device.Model != hostile {
		t.Errorf("the JSON changed the model: %v", r.Device.Model)
	}

	var smoke strings.Builder
	printSmoke(&smoke, []smokeStep{{Name: "pty", Detail: "a\x1b[2Jb"}})
	if strings.Contains(smoke.String(), "\x1b") || !strings.Contains(smoke.String(), "a[2Jb") {
		t.Errorf("smoke's text: %q", smoke.String())
	}

	var log strings.Builder
	o, err := parseProvision([]string{"--image", "x"})
	if err != nil {
		t.Fatal(err)
	}
	o.log = &log
	err = o.checkDevice(adb.Device{})
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("the refusal: %q", err)
	}
	o.force = true
	if err := o.checkDevice(adb.Device{}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(log.String(), "\x1b\x07") || !strings.Contains(log.String(), "protected") {
		t.Errorf("provision's log: %q", log.String())
	}
}

// Other apps' notifications stay on the phone: smoke's commands print nothing of them.
func TestSmokeKeepsOtherAppsNotificationsOnThePhone(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.write("notifications", []byte("  NotificationRecord(0x0def5678: pkg=org.example.sms user=UserHandle{0} id=3 tag=thread-4512345678 importance=4 key=0|org.example.sms|3|x|10300: Notification(channel=sms))\n        android.title=String (Mum)\n        android.text=String (your code is 123456)\n"))
	var printed strings.Builder
	dev := adb.Device{Trace: func(_ []string, _ time.Time, _ int, stdout string) { printed.WriteString(stdout) }}
	steps, _ := runSmoke(context.Background(), dev, debugPackage, nil, nil, io.Discard)
	if st, _ := stepNamed(steps, "notification"); st.OK == nil || !*st.OK {
		t.Errorf("notification: %+v", st)
	}
	for _, token := range []string{"123456", "org.example.sms", "4512345678"} {
		if strings.Contains(printed.String(), token) {
			t.Errorf("%q left the phone:\n%s", token, printed.String())
		}
	}
}

// What a command printed before failing stays out of the report, unlike adb's complaint.
func TestDoctorLeavesStdoutOutOfItsReport(t *testing.T) {
	const private = "export TOKEN=hunter2"
	row := "|1| alice@phone:~$ " + private + "\n"
	dump := "content read --uri content://dev.nickolaj.pocketnix.debug.control/dump?tab=1"
	for name, setup := range map[string]func(d *fakeDevice){
		"adb-fails": func(d *fakeDevice) { d.drop("dump?tab=1", row, "error: closed\n") },
		"command-fails": func(d *fakeDevice) {
			d.can(dump, row, 1)
		},
		"notification": func(d *fakeDevice) {
			d.drop("dumpsys notification", "        android.text=String ("+private+")\n", "error: closed\n")
		},
	} {
		for _, mode := range [][]string{{"--json"}, nil} {
			t.Run(fmt.Sprintf("%s %v", name, mode), func(t *testing.T) {
				d := smokeDevice(t, everyTool...)
				setup(d)
				out, err := stdoutOf(t, func() error { return runDoctor(append(slices.Clone(mode), "--smoke")) })
				var status exitStatus
				if !errors.As(err, &status) || status != 1 {
					t.Errorf("run: %v, want a failed step's exit status 1", err)
				}
				if strings.Contains(out, private) {
					t.Errorf("the report quotes the command's stdout:\n%s", out)
				}
			})
		}
	}
}

// A dump stalls the app's terminal output, so the PTY step reads few of them.
func TestSmokePTYPollsSlowly(t *testing.T) {
	d := smokeDevice(t, append(without(everyTool, reactPTY), reactPTYRunning)...)
	smokeTimes.pty = 400 * time.Millisecond
	smokeTimes.ptyPoll = 150 * time.Millisecond
	if _, _, err := doctorJSON(t, "--smoke"); err == nil {
		t.Error("a PTY step without its marker passed")
	}
	reads, _ := os.ReadFile(filepath.Join(d.state, "control", "dump-reads"))
	want := fmt.Sprintf("tab=%d", ptyTab(d))
	n := 0
	for _, l := range strings.Split(string(reads), "\n") {
		if l == want {
			n++
		}
	}
	// One at once, one per poll in the limit, and one more past it.
	if n == 0 || n > 6 {
		t.Errorf("the PTY step read tab %s's dump %d times", want, n)
	}
}

// A PTY step that times out still says what it found in the guest and the app.
func TestSmokePTYTimeoutSaysWhatItFound(t *testing.T) {
	smokeDevice(t, append(without(without(everyTool, reactPTY), reactPTYTime), reactPTYRunning)...)
	r, _, _ := doctorJSON(t, "--smoke")
	st, _ := stepNamed(r.Smoke, "pty")
	if st.OK == nil || *st.OK {
		t.Errorf("pty: %+v, want a failure", st)
	}
	for _, token := range []string{"still running", "600s", "stopped"} {
		if !strings.Contains(st.Detail, token) {
			t.Errorf("pty detail lacks %q: %s", token, st.Detail)
		}
	}
}

// A step that fails midway still leaves cleanup to end every tab.
func TestSmokeCleanupExitsEveryTab(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	r, _, err := doctorJSON(t, "--smoke", "--gpu-apps", strings.Join(smokeApps, ","))
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("a failed step returned %v, want exit status 1", err)
	}
	if st, _ := stepNamed(r.Smoke, "pty"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "stopped") {
		t.Errorf("pty without its output: %+v, want a failure saying the output stopped", st)
	}
	if st, _ := stepNamed(r.Smoke, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	exitedEveryTab(t, d)
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// An interrupt skips the steps left and the reboot, and still cleans up.
func TestSmokeInterruptCleansUp(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	smokeTimes.pty = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnTyped(ctx, cancel, d, reactPTY.matches)
	steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, smokeApps, nil, io.Discard)
	for _, name := range []string{"notification", "inhibitor", "gpu", "pages", "reboot"} {
		if st, _ := stepNamed(steps, name); st.OK != nil || !strings.Contains(st.Detail, "interrupted") {
			t.Errorf("step %s after the interrupt: %+v", name, st)
		}
	}
	if st, _ := stepNamed(steps, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
	if fakeReboots(d.state) != 0 {
		t.Error("an interrupted smoke rebooted the guest")
	}
}

// typedInto is whether a line that match accepts was typed into any tab.
func typedInto(d *fakeDevice, match func(line string) bool) bool {
	for _, lines := range d.typed() {
		if slices.ContainsFunc(lines, match) {
			return true
		}
	}
	return false
}

func isExit(line string) bool { return line == " exit" }

func isReboot(line string) bool { return reactReboot.matches(line) }

// cancelOnTyped cancels ctx once a line that match accepts was typed into a tab.
func cancelOnTyped(ctx context.Context, cancel context.CancelFunc, d *fakeDevice, match func(line string) bool) {
	go func() {
		for ctx.Err() == nil && !typedInto(d, match) {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
}

// Cleanup finishes after an interrupt it took during its run, and the guest stays up.
func TestSmokeInterruptDuringCleanupSkipsTheReboot(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnTyped(ctx, cancel, d, isExit)
	steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, nil, nil, io.Discard)
	if st, _ := stepNamed(steps, "reboot"); st.OK != nil || !strings.Contains(st.Detail, "interrupted") {
		t.Errorf("reboot: %+v, want it skipped", st)
	}
	if st, _ := stepNamed(steps, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	if typedInto(d, isReboot) {
		t.Error("the guest was told to reboot after the interrupt")
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// An interrupt ends the reboot step's waits, which are minutes long.
func TestSmokeInterruptEndsTheRebootsWait(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactReboot)...)
	smokeTimes.reboot = 10 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnTyped(ctx, cancel, d, isReboot)
	done := make(chan []smokeStep)
	go func() {
		steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, nil, nil, io.Discard)
		done <- steps
	}()
	select {
	case steps := <-done:
		if st, _ := stepNamed(steps, "reboot"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "interrupted") {
			t.Errorf("reboot: %+v, want a failure saying it was interrupted", st)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reboot step kept waiting after the interrupt")
	}
}

// Every way the reboot step fails still closes the tab it opened.
func TestSmokeFailedRebootClosesItsTab(t *testing.T) {
	typed := reactReboot.Typed
	for _, tc := range []struct {
		name  string
		react *fakeReaction
		setup func()
		// interrupt comes once the reboot is typed.
		interrupt bool
	}{
		{name: "refused", react: &fakeReaction{Typed: typed, Output: []string{"${1}-exit=1"}}},
		{name: "stays-running", setup: func() { smokeTimes.reboot = 50 * time.Millisecond }},
		{name: "agent-stays-away", react: &fakeReaction{Typed: typed, Write: map[string]string{"control/booting": "1000000"}},
			setup: func() { smokeTimes.boot = 50 * time.Millisecond }},
		// The VM stops and comes back, without the app counting a reboot.
		{name: "vm-stopped-and-started", react: &fakeReaction{Typed: typed, Write: map[string]string{"control/booting": "1"}}},
		{name: "interrupted", setup: func() { smokeTimes.reboot = 10 * time.Minute }, interrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := without(everyTool, reactReboot)
			if tc.react != nil {
				rs = append(rs, *tc.react)
			}
			d := smokeDevice(t, rs...)
			if tc.setup != nil {
				tc.setup()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.interrupt {
				cancelOnTyped(ctx, cancel, d, isReboot)
			}
			steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, nil, nil, io.Discard)
			if st, _ := stepNamed(steps, "reboot"); st.OK == nil || *st.OK {
				t.Errorf("reboot: %+v, want a failure", st)
			}
			if left := leftovers(d); len(left) > 0 {
				t.Errorf("smoke left %q", left)
			}
		})
	}
}

// An app without the reboot count can't show a reboot from a VM stop: the step
// says so instead of passing, and the guest stays up.
func TestSmokeRebootNeedsTheAppsCount(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.predateRebootCount()
	r, _, err := doctorJSON(t, "--smoke")
	if err != nil {
		t.Errorf("doctor --smoke: %v, want a skipped step to pass", err)
	}
	if st, ok := stepNamed(r.Smoke, "reboot"); !ok || st.OK != nil || !strings.Contains(st.Detail, "reboot count") {
		t.Errorf("reboot: %+v, want ok null naming the reboot count", st)
	}
	if typedInto(d, isReboot) {
		t.Error("the guest was told to reboot")
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// What a guest prints into a tab isn't the app's count.
func TestSmokeReadsTheRebootCountFromTheDumpsOwnLines(t *testing.T) {
	if got := parseVM("state=Running phase=Running\n|1| guest reboots=7\n").reboots; got != nil {
		t.Errorf("reboots = %d from a transcript line", *got)
	}
	got := parseVM("state=Running phase=Running\nguest reboots=7\n|1| guest reboots=9\n").reboots
	if got == nil || *got != 7 {
		t.Errorf("reboots = %v, want 7", got)
	}
}

// An interrupt while the inhibitor's script runs waits for its pid, which is all
// cleanup can end it by.
func TestSmokeInterruptDuringInhibitEndsIt(t *testing.T) {
	slow := reactInhibit
	slow.Output = nil
	d := smokeDevice(t, append(without(everyTool, reactInhibit), slow)...)
	smokeTimes.cmd = 30 * time.Second
	isInhibit := reactInhibit.matches
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil && !typedInto(d, isInhibit) {
			time.Sleep(time.Millisecond)
		}
		cancel()
		time.Sleep(20 * time.Millisecond)
		// The script gets to its output after the interrupt.
		for tab, lines := range d.typed() {
			if i := slices.IndexFunc(lines, isInhibit); i >= 0 {
				p := regexp.MustCompile(`smoke-\d+`).FindString(lines[i])
				appendFile(fakeTabPath(d.state, tab), p+"-pid=4242\n"+p+"-exit=0\n")
			}
		}
	}()
	steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, nil, nil, io.Discard)
	if st, _ := stepNamed(steps, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// A forward can end up allowed though the answer to the allow never arrived:
// cleanup still waits for it, and says so when it stays.
func TestSmokeCleanupWatchesAForwardWhoseAllowFailed(t *testing.T) {
	allowedAtOnce := reactListen
	allowedAtOnce.Write = map[string]string{"control/listening/${2}": "${3}", "control/allowed/${2}": "${2}"}
	d := smokeDevice(t, append(without(everyTool, reactListen), allowedAtOnce)...)
	smokeTimes.forward = 50 * time.Millisecond
	d.drop("answer:s:allow", "", "error: closed\n")
	steps, _ := runSmoke(context.Background(), adb.Device{}, debugPackage, nil, nil, io.Discard)
	if st, _ := stepNamed(steps, "forward"); st.OK == nil || *st.OK {
		t.Errorf("forward: %+v, want a failure", st)
	}
	if st, _ := stepNamed(steps, "cleanup"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "forward") {
		t.Errorf("cleanup: %+v, want a failure naming the forward", st)
	}
}

// A row of the typed line that starts at a marker's text isn't what the shell printed.
func TestSmokeIgnoresATypedLinesMarkerText(t *testing.T) {
	wrapped := reactInhibit
	wrapped.Output = []string{"${1}-exit=0; exit; }; systemd-inhibit --what=sleep --who=pocket"}
	smokeDevice(t, append(without(everyTool, reactInhibit), wrapped)...)
	steps, _ := runSmoke(context.Background(), adb.Device{}, debugPackage, nil, nil, io.Discard)
	if st, _ := stepNamed(steps, "inhibitor"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "printed no") {
		t.Errorf("inhibitor: %+v, want a failure saying it saw no exit line", st)
	}
}

func TestSmokeReadsMarkerValues(t *testing.T) {
	for _, tc := range []struct {
		row, value string
		ok         bool
	}{
		{"smoke-3-exit=0", "0", true},
		{"smoke-3-end=", "", true},
		{"smoke-3-tool=socat   ", "socat", true},
		{"smoke-3-exit=$s'", "", false},
		{"smoke-3-exit=0; exit; };", "", false},
		{"smoke-3-exit=0;", "", false},
		{"smoke-3-exit=0'", "", false},
	} {
		m := markerLine.FindStringSubmatch(tc.row)
		if (m != nil) != tc.ok || (m != nil && m[3] != tc.value) {
			t.Errorf("%q: got %q", tc.row, m)
		}
	}
}

// The PTY script's status covers head, whose failure would otherwise be an
// empty pipe that base64 turns into a clean exit. It runs here in a real sh.
func TestSmokePTYScriptReportsAFailingHead(t *testing.T) {
	failing := filepath.Join(t.TempDir(), "head")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path, want string
	}{
		"head-works": {os.Getenv("PATH"), "smoke-1-exit=0"},
		"head-fails": {filepath.Dir(failing) + string(os.PathListSeparator) + os.Getenv("PATH"), "smoke-1-exit=1"},
	} {
		t.Run(name, func(t *testing.T) {
			times := filepath.Join(t.TempDir(), "times")
			c := exec.Command("sh", "-c", strings.ReplaceAll(ptyScript(times, 3000), "@", "smoke-1"))
			c.Env = append(os.Environ(), "PATH="+tc.path)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("no %s in:\n%s", tc.want, out)
			}
			c = exec.Command("sh", "-c", strings.ReplaceAll(strings.ReplaceAll(ptyTimeScript, smokePTYFile, times), "@", "smoke-1"))
			out, err = c.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			for _, key := range []string{"start", "end"} {
				if !regexp.MustCompile(`(?m)^smoke-1-` + key + `=\d+$`).Match(out) {
					t.Errorf("no %s time in:\n%s", key, out)
				}
			}
		})
	}
}

func TestSmokeSkipsWithoutInputs(t *testing.T) {
	none := func(r fakeReaction) fakeReaction {
		return fakeReaction{Typed: r.Typed, Output: []string{"${1}-tool=none", "${1}-exit=0"}}
	}
	toolless := []fakeReaction{
		none(reactListen), none(reactNotify), none(reactInhibit),
		reactPTY, reactPTYTime, reactPTYClean, reactKill, reactReboot,
	}
	gpuApps := []string{"--gpu-apps", strings.Join(smokeApps, ",")}
	for _, tc := range []struct {
		name  string
		setup func(d *fakeDevice)
		args  []string
		// tokens are in the detail of each step that didn't apply.
		tokens map[string]string
	}{
		{name: "no-tools-no-gpu-apps", tokens: map[string]string{
			"forward": "socat", "notification": "notify-send", "inhibitor": "systemd-inhibit", "gpu": "--gpu-apps", "pages": "--gpu-apps",
		}},
		{name: "no-gui", args: gpuApps, setup: func(d *fakeDevice) {
			os.Remove(filepath.Join(d.state, "control", "gui"))
		}, tokens: map[string]string{"gpu": "GUI", "pages": "GUI"}},
		{name: "locked", args: gpuApps, setup: func(d *fakeDevice) {
			d.can(wakefulnessCommand, "  mWakefulness=Asleep\n", 0)
		}, tokens: map[string]string{"gpu": "locked", "pages": "locked"}},
		{name: "unknown-app", args: []string{"--gpu-apps", "org.example.Cube,org.example.Missing"}, tokens: map[string]string{"gpu": "org.example.Missing"}},
		{name: "no-guest-ram", args: gpuApps, setup: func(d *fakeDevice) {
			d.setHead(strings.Replace(fakeRunningHead, "guestRam=true", "guestRam=false", 1))
		}, tokens: map[string]string{"pages": "guestRam=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := smokeDevice(t, toolless...)
			if tc.setup != nil {
				tc.setup(d)
			}
			r, _, err := doctorJSON(t, append([]string{"--smoke"}, tc.args...)...)
			// Steps that don't apply are no failure while the others ran.
			if err != nil {
				t.Errorf("doctor --smoke: %v", err)
			}
			for name, token := range tc.tokens {
				if st, ok := stepNamed(r.Smoke, name); !ok || st.OK != nil || !strings.Contains(st.Detail, token) {
					t.Errorf("step %s: %+v, want ok null with %q", name, st, token)
				}
			}
			if left := leftovers(d); len(left) > 0 {
				t.Errorf("smoke left %q", left)
			}
		})
	}
}

func TestSmokeGPUFailsOnErrUnspec(t *testing.T) {
	second := fakeReaction{Launch: smokeApps[1], Append: map[string]string{
		"logcat": "1762000005.000  4321  4400 E crosvm  : ResourceUnmapBlob: ErrUnspec\n",
	}}
	smokeDevice(t, append(slices.Clone(everyTool), second)...)
	r, _, err := doctorJSON(t, "--smoke", "--gpu-apps", strings.Join(smokeApps, ","))
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("a failed GPU step returned %v, want exit status 1", err)
	}
	if st, _ := stepNamed(r.Smoke, "gpu"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "ErrUnspec") {
		t.Errorf("gpu: %+v, want a failure naming ErrUnspec", st)
	}
}

func TestSmokeNeedsARunningVM(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.setHead("state=Stopped phase=Stopped link=Down host=null\nengine launched=none audio=off")
	r, _, err := doctorJSON(t, "--smoke")
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("doctor --smoke on a stopped VM: %v, want exit status 1, as nothing ran", err)
	}
	for _, name := range smokeSteps {
		if st, ok := stepNamed(r.Smoke, name); !ok || st.OK != nil || !strings.Contains(st.Detail, "Stopped") {
			t.Errorf("step %s: %+v, want ok null naming the phase", name, st)
		}
	}
	if len(d.typed()) > 0 {
		t.Errorf("smoke typed into tabs of a stopped VM: %v", d.typed())
	}
}

// A debug app that isn't there means nothing ran either, and the report keeps
// every step, with the reason.
func TestSmokeNeedsTheDebugApp(t *testing.T) {
	d := newFakeDevice(t)
	cannedPixel(d)
	r, _, err := doctorJSON(t, "--smoke")
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("doctor --smoke without the app: %v, want exit status 1", err)
	}
	for _, name := range smokeSteps {
		if st, ok := stepNamed(r.Smoke, name); !ok || st.OK != nil || st.Detail == "" {
			t.Errorf("step %s: %+v, want ok null with a reason", name, st)
		}
	}
}

// Without a GPU or guest RAM there's nothing to judge, but the launches still run.
func TestSmokeNoGPUStillLaunches(t *testing.T) {
	rs := slices.Clone(everyTool)
	for _, app := range smokeApps {
		rs = append(rs, fakeReaction{Launch: app, Write: map[string]string{"launched/" + app: ""}})
	}
	d := smokeDevice(t, rs...)
	head := strings.Replace(fakeRunningHead, "gpu=gfxstream", "gpu=2d", 1)
	d.setHead(strings.Replace(head, "guestRam=true", "guestRam=false", 1))
	r, _, err := doctorJSON(t, "--smoke", "--gpu-apps", strings.Join(smokeApps, ","))
	if err != nil {
		t.Errorf("doctor --smoke: %v", err)
	}
	for name, token := range map[string]string{"gpu": "gpu=2d", "pages": "guestRam=false"} {
		if st, ok := stepNamed(r.Smoke, name); !ok || st.OK != nil || !strings.Contains(st.Detail, token) {
			t.Errorf("step %s: %+v, want ok null with %q", name, st, token)
		}
	}
	for _, app := range smokeApps {
		if !exists(filepath.Join(d.state, "launched", app)) {
			t.Errorf("%s was never launched:\n%s", app, d.events())
		}
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// A launch that fails is a failure even without a GPU.
func TestSmokeNoGPULaunchFails(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.setHead(strings.Replace(fakeRunningHead, "gpu=gfxstream", "gpu=2d", 1))
	smokeTimes.window = 50 * time.Millisecond
	// In the guest's list, but it opens no window.
	apps := []string{smokeApps[0], "org.example.Silent"}
	d.setGUI(apps...)
	d.react(append(slices.Clone(everyTool), fakeReaction{Launch: "org.example.Silent", Remove: []string{"control/windows/org.example.Silent"}})...)
	r, _, _ := doctorJSON(t, "--smoke", "--gpu-apps", strings.Join(apps, ","))
	if st, _ := stepNamed(r.Smoke, "gpu"); st.OK == nil || *st.OK || !strings.Contains(st.Detail, "org.example.Silent") {
		t.Errorf("gpu: %+v, want a failure naming the app", st)
	}
}

// With its transcript's ring wrapped, the terminal joins the marker to the row
// before or after it (TerminalRow.clear keeps mLineWrap), as a phone's 64 MiB does.
func TestSmokePTYFindsAJoinedMarker(t *testing.T) {
	for name, tc := range map[string]struct{ line, want string }{
		"own-row":           {"${1}-exit=0", "own row"},
		"to-the-row-before": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAA            ${1}-exit=0", "row before"},
		"to-the-row-after":  {"${1}-exit=0                          ~ > ", "row after"},
		"to-both":           {"AAAA    ${1}-exit=0    ~ > ", "rows before and after"},
	} {
		t.Run(name, func(t *testing.T) {
			joined := reactPTY
			joined.Output = []string{"AAAAAAAAAAAA", tc.line}
			smokeDevice(t, append([]fakeReaction{joined}, without(everyTool, reactPTY)...)...)
			r, _, _ := doctorJSON(t, "--smoke")
			st, _ := stepNamed(r.Smoke, "pty")
			if st.OK == nil || !*st.OK {
				t.Errorf("pty: %+v, want it to find the marker", st)
			}
			if !strings.Contains(st.Detail, tc.want) {
				t.Errorf("pty: %q, want it to say %q", st.Detail, tc.want)
			}
		})
	}
}

// wantSelected checks a report's steps: those in ran passed, and every other
// reads as skipped for not being selected.
func wantSelected(t *testing.T, steps []smokeStep, ran ...string) {
	t.Helper()
	for _, name := range smokeSteps {
		st, ok := stepNamed(steps, name)
		switch {
		case !ok:
			t.Errorf("the report lacks step %s", name)
		case slices.Contains(ran, name):
			if st.OK == nil || !*st.OK {
				t.Errorf("step %s: %+v, want it to pass", name, st)
			}
		case !notSelected(st):
			t.Errorf("step %s: %+v, want ok null saying it wasn't selected", name, st)
		}
	}
}

func notSelected(st smokeStep) bool {
	return st.OK == nil && strings.Contains(st.Detail, "not selected")
}

func wantNoReboot(t *testing.T, d *fakeDevice) {
	t.Helper()
	if typedInto(d, isReboot) || fakeReboots(d.state) != 0 {
		t.Errorf("the guest was told to reboot, and rebooted %d times", fakeReboots(d.state))
	}
}

func TestSmokeStepsRunsOneStepAndCleansUp(t *testing.T) {
	scripts := map[string]fakeReaction{"forward": reactListen, "pty": reactPTY, "notification": reactNotify, "inhibitor": reactInhibit}
	for name := range scripts {
		t.Run(name, func(t *testing.T) {
			d := smokeDevice(t, everyTool...)
			r, _, err := doctorJSON(t, "--smoke", "--steps", name)
			if err != nil {
				t.Errorf("doctor --smoke --steps %s: %v", name, err)
			}
			wantSelected(t, r.Smoke, name, "cleanup")
			for other, script := range scripts {
				if ran := typedInto(d, script.matches); ran != (other == name) {
					t.Errorf("step %s's script typed: %v", other, ran)
				}
			}
			exitedEveryTab(t, d)
			if left := leftovers(d); len(left) > 0 {
				t.Errorf("smoke left %q", left)
			}
			wantNoReboot(t, d)
		})
	}
}

func TestSmokeStepsCleanupAlone(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	r, _, err := doctorJSON(t, "--smoke", "--steps", "cleanup")
	if err != nil {
		t.Errorf("doctor --smoke --steps cleanup: %v", err)
	}
	wantSelected(t, r.Smoke, "cleanup")
	if len(d.typed()) > 0 {
		t.Errorf("nothing to clean up, yet typed %v", d.typed())
	}
}

func TestSmokeStepsRebootsWhenNamed(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	r, _, err := doctorJSON(t, "--smoke", "--steps", "reboot")
	if err != nil {
		t.Errorf("doctor --smoke --steps reboot: %v", err)
	}
	wantSelected(t, r.Smoke, "reboot", "cleanup")
	if fakeReboots(d.state) != 1 {
		t.Errorf("the guest rebooted %d times, want once", fakeReboots(d.state))
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

// Tab ids count up, so they tell the order steps opened theirs. A run's tabs
// close before the reboot, or exitedEveryTab would find no exit in them.
func TestSmokeStepsRunInTheirOwnOrder(t *testing.T) {
	for name, args := range map[string][]string{
		"given-out-of-order": {"--smoke", "--steps", "reboot,pty,forward"},
		"no-filter":          {"--smoke"},
	} {
		t.Run(name, func(t *testing.T) {
			d := smokeDevice(t, everyTool...)
			r, _, err := doctorJSON(t, args...)
			if err != nil {
				t.Errorf("doctor %v: %v", args, err)
			}
			var got []string
			for _, st := range r.Smoke {
				got = append(got, st.Name)
			}
			if !slices.Equal(got, smokeSteps) {
				t.Errorf("the report's order is %q, want %q", got, smokeSteps)
			}
			tabOf := func(match func(string) bool) int {
				for tab, lines := range d.typed() {
					if match(lines[0]) {
						return tab
					}
				}
				t.Errorf("a step opened no tab: %v", d.typed())
				return -1
			}
			forward, pty, reboot := tabOf(reactListen.matches), tabOf(reactPTY.matches), tabOf(isReboot)
			if forward >= pty || pty >= reboot {
				t.Errorf("tabs opened as forward %d, pty %d, reboot %d", forward, pty, reboot)
			}
			exitedEveryTab(t, d)
		})
	}
}

// launchReactions are every tool, plus marks of the launches, after rs.
func launchReactions(rs ...fakeReaction) []fakeReaction {
	rs = append(rs, everyTool...)
	for _, app := range smokeApps {
		rs = append(rs, fakeReaction{Launch: app, Write: map[string]string{"launched/" + app: ""}})
	}
	return rs
}

// reactErrUnspec has crosvm log the GPU failure as the second app launches.
var reactErrUnspec = fakeReaction{Launch: smokeApps[1], Append: map[string]string{
	"logcat": "1762000005.000  4321  4400 E crosvm  : ResourceUnmapBlob: ErrUnspec\n",
}}

func wantBothLaunchedAndEnded(t *testing.T, d *fakeDevice) {
	t.Helper()
	for _, app := range smokeApps {
		if !exists(filepath.Join(d.state, "launched", app)) {
			t.Errorf("%s was never launched:\n%s", app, d.events())
		}
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
}

func TestSmokeStepsSharedLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, steps string
		reactions   []fakeReaction
		ran         []string
	}{
		{name: "gpu-alone", steps: "gpu", reactions: launchReactions(), ran: []string{"gpu", "cleanup"}},
		{name: "pages-alone", steps: "pages", reactions: launchReactions(), ran: []string{"pages", "cleanup"}},
		{name: "both", steps: "pages,gpu", reactions: launchReactions(), ran: []string{"gpu", "pages", "cleanup"}},
		// gpu fails here, but wasn't selected.
		{name: "pages-alone-gpu-failing", steps: "pages", reactions: launchReactions(reactErrUnspec), ran: []string{"pages", "cleanup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := smokeDevice(t, tc.reactions...)
			r, _, err := doctorJSON(t, "--smoke", "--steps", tc.steps, "--gpu-apps", strings.Join(smokeApps, ","))
			if err != nil {
				t.Errorf("run: %v", err)
			}
			wantSelected(t, r.Smoke, tc.ran...)
			wantBothLaunchedAndEnded(t, d)
			wantNoReboot(t, d)
		})
	}
}

func TestSmokeStepsSelectedGPUFailureExits1(t *testing.T) {
	d := smokeDevice(t, launchReactions(reactErrUnspec)...)
	r, _, err := doctorJSON(t, "--smoke", "--steps", "gpu", "--gpu-apps", strings.Join(smokeApps, ","))
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("run: %v, want exit status 1", err)
	}
	if st, _ := stepNamed(r.Smoke, "gpu"); st.OK == nil || *st.OK {
		t.Errorf("gpu: %+v, want a failure", st)
	}
	if st, _ := stepNamed(r.Smoke, "pages"); !notSelected(st) {
		t.Errorf("pages: %+v, want it not selected", st)
	}
	wantBothLaunchedAndEnded(t, d)
}

// A launch that fails only fails gpu, so a run of pages alone passes, and its
// report still has to say why pages saw no frame.
func TestSmokeStepsPagesAloneKeepsTheLaunchsProblem(t *testing.T) {
	const silent = "org.example.Silent"
	d := smokeDevice(t, append(slices.Clone(everyTool), fakeReaction{Launch: silent, Remove: []string{"control/windows/" + silent}})...)
	smokeTimes.window = 50 * time.Millisecond
	d.setGUI(silent, smokeApps[0])
	r, _, err := doctorJSON(t, "--smoke", "--steps", "pages", "--gpu-apps", silent+","+smokeApps[0])
	if err != nil {
		t.Errorf("run: %v", err)
	}
	if st, _ := stepNamed(r.Smoke, "pages"); st.OK != nil {
		t.Errorf("pages: %+v, want ok null", st)
	}
	if st, _ := stepNamed(r.Smoke, "gpu"); !notSelected(st) || !strings.Contains(st.Detail, silent) {
		t.Errorf("gpu: %+v, want it not selected, naming the app", st)
	}
}

func TestSmokeStepsSharedLaunchNeedsGPUApps(t *testing.T) {
	for name, other := range map[string]string{"gpu": "pages", "pages": "gpu"} {
		t.Run(name, func(t *testing.T) {
			smokeDevice(t, everyTool...)
			r, _, err := doctorJSON(t, "--smoke", "--steps", name)
			if err != nil {
				t.Errorf("run: %v", err)
			}
			if st, _ := stepNamed(r.Smoke, name); st.OK != nil || !strings.Contains(st.Detail, "--gpu-apps") {
				t.Errorf("%s: %+v, want ok null naming --gpu-apps", name, st)
			}
			if st, _ := stepNamed(r.Smoke, other); !notSelected(st) {
				t.Errorf("%s: %+v, want it not selected", other, st)
			}
		})
	}
}

func TestSmokeStepsFailingStepExits1AndCleansUp(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	r, _, err := doctorJSON(t, "--smoke", "--steps", "pty")
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("a failed step returned %v, want exit status 1", err)
	}
	if st, _ := stepNamed(r.Smoke, "pty"); st.OK == nil || *st.OK {
		t.Errorf("pty: %+v, want a failure", st)
	}
	if st, _ := stepNamed(r.Smoke, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
	wantNoReboot(t, d)
}

// A run that can't start exits 1, filtered or not; what it left out still says so.
func TestSmokeStepsNeedARunningVM(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.setHead("state=Stopped phase=Stopped link=Down host=null\nengine launched=none audio=off")
	r, _, err := doctorJSON(t, "--smoke", "--steps", "forward")
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("run: %v, want exit status 1, as nothing ran", err)
	}
	for _, name := range []string{"forward", "cleanup"} {
		if st, _ := stepNamed(r.Smoke, name); st.OK != nil || !strings.Contains(st.Detail, "Stopped") {
			t.Errorf("step %s: %+v, want ok null naming the phase", name, st)
		}
	}
	if st, _ := stepNamed(r.Smoke, "reboot"); !notSelected(st) {
		t.Errorf("reboot: %+v, want it not selected", st)
	}
	if len(d.typed()) > 0 {
		t.Errorf("smoke typed into tabs of a stopped VM: %v", d.typed())
	}
}

// An interrupt skips the selected steps left, and leaves the others not selected.
func TestSmokeStepsInterrupted(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	smokeTimes.pty = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnTyped(ctx, cancel, d, reactPTY.matches)
	steps, _ := runSmoke(ctx, adb.Device{}, debugPackage, nil, []string{"pty", "notification"}, io.Discard)
	if st, _ := stepNamed(steps, "notification"); st.OK != nil || !strings.Contains(st.Detail, "interrupted") {
		t.Errorf("notification: %+v, want it interrupted", st)
	}
	for _, name := range []string{"forward", "inhibitor", "gpu", "pages", "reboot"} {
		if st, _ := stepNamed(steps, name); !notSelected(st) {
			t.Errorf("%s: %+v, want it not selected", name, st)
		}
	}
	if st, _ := stepNamed(steps, "cleanup"); st.OK == nil || !*st.OK {
		t.Errorf("cleanup: %+v", st)
	}
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
	wantNoReboot(t, d)
}

func TestSmokeStepsUsageErrorsComeBeforeThePhone(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// token is in what it prints.
		token string
	}{
		{name: "unknown", args: []string{"--smoke", "--steps", "warp"}, token: "warp"},
		{name: "unknown-among-known", args: []string{"--smoke", "--steps", "forward,warp"}, token: "warp"},
		{name: "unknown-without-smoke", args: []string{"--steps", "warp"}, token: "warp"},
		{name: "without-smoke", args: []string{"--steps", "forward"}},
		// Reading these as no filter would reboot the guest.
		{name: "empty", args: []string{"--smoke", "--steps", ""}},
		{name: "only-commas", args: []string{"--smoke", "--steps", ","}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := smokeDevice(t, everyTool...)
			var err error
			printed, _ := stderrOf(t, func() error {
				_, err = stdoutOf(t, func() error { return runDoctor(tc.args) })
				return nil
			})
			if !errors.Is(err, errUsage) {
				t.Errorf("doctor %q: %v, want a usage error", tc.args, err)
			}
			if !strings.Contains(printed, tc.token) {
				t.Errorf("it printed no %q:\n%s", tc.token, printed)
			}
			if d.adbCalled() {
				t.Errorf("doctor %q called adb", tc.args)
			}
		})
	}
	d := smokeDevice(t, everyTool...)
	if _, _, err := doctorJSON(t, "--smoke", "--steps", "cleanup"); err != nil || !d.adbCalled() {
		t.Errorf("a valid run: %v, adb called: %v", err, d.adbCalled())
	}
}
