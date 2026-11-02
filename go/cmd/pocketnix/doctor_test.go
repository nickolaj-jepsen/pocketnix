package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// doctorJSON runs doctor --json with args and decodes its report, and the
// same JSON untyped, to check C2's field names.
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
	d.can("pm has-feature android.software.virtualization_framework", "true\n", 0)
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
	r, raw, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"schema": float64(1), "device.os": "grapheneos", "avf.vmSupported": true,
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
	if r.Schema != doctorSchema {
		t.Errorf("schema = %d", r.Schema)
	}
	if r.Device.Model == nil || *r.Device.Model != "Pixel 8 Pro" {
		t.Errorf("device.model = %v", r.Device.Model)
	}
	if r.Device.OS != osGrapheneOS {
		t.Errorf("device.os = %q", r.Device.OS)
	}
	if r.AVF.VMSupported == nil || !*r.AVF.VMSupported {
		t.Errorf("avf.vmSupported = %v", r.AVF.VMSupported)
	}
	if r.Verdict.Level != levelOK {
		t.Errorf("verdict = %+v", r.Verdict)
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
			d.can("pm has-feature android.software.virtualization_framework", "false\n", 1)
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

// B1: the serial reaches neither the report nor the text.
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
	d.can("pm has-feature android.software.virtualization_framework", "false\n", 1)
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

// fastSmoke shortens smoke's waits, for a fake device that answers at once.
func fastSmoke(t *testing.T) {
	poll, times := controlPoll, smokeTimes
	t.Cleanup(func() { controlPoll, smokeTimes = poll, times })
	controlPoll = 5 * time.Millisecond
	s := time.Second
	smokeTimes.cmd, smokeTimes.pty, smokeTimes.offer, smokeTimes.notify, smokeTimes.wake = s, s, s, s, s
	smokeTimes.window, smokeTimes.frames, smokeTimes.gone, smokeTimes.exit = s, s, s, s
	smokeTimes.forward, smokeTimes.reboot, smokeTimes.boot = s, s, s
}

// cannedPath is the state path of the shell command's canned answer, for a reaction to write.
func cannedPath(cmdline string) string { return filepath.Join("canned", fakeCannedKey(cmdline)) }

const (
	powerLocks    = "dumpsys power | grep -F pocketnix:guest"
	notifications = "dumpsys notification --noredact | grep -e NotificationRecord -e android.text="
	// powerHistory is what dumpsys power keeps of a wake lock after its release, as a Pixel prints it.
	powerHistory = "  11-02 16:59:16.878 - 10200 (dev.nickolaj.pocketnix.debug) - ACQ pocketnix:guest (partial)\n  11-02 17:00:12.971 - 10200 (dev.nickolaj.pocketnix.debug) - REL pocketnix:guest\n"
)

// writeCanned is a reaction's Write making cmdline answer out with status.
func writeCanned(cmdline, out string, status int) map[string]string {
	return map[string]string{cannedPath(cmdline): out, cannedPath(cmdline) + ".status": strconv.Itoa(status)}
}

// Reactions of a guest with every tool smoke uses.
var (
	reactListen = fakeReaction{Typed: `echo (smoke-\d+)-tool=socat;.*' sh '(\d+)' '([^']+)'`,
		Output: []string{"${1}-tool=socat"}, Write: map[string]string{"control/listening/${2}": "${3}"}}
	reactPTY    = fakeReaction{Typed: `head -c \d+ /dev/urandom \| base64; echo (smoke-\d+)-exit=`, Output: []string{"AAAAAAAAAAAA", "${1}-exit=0"}}
	reactNotify = fakeReaction{Typed: `notify-send -p .*echo (smoke-\d+)-exit=\$s' sh '[^']*' '([^']+)'`, Output: []string{"${1}-id=7", "${1}-exit=0"},
		Write: writeCanned(notifications, "  NotificationRecord(0x0abc1234: pkg=dev.nickolaj.pocketnix.debug user=UserHandle{0} id=7 tag=guest importance=3 key=0|dev.nickolaj.pocketnix.debug|7|guest|10200: Notification(channel=guest))\n        android.text=String (${2})\n", 0)}
	reactUnnotify = fakeReaction{Typed: `CloseNotification u "\$1"; echo (smoke-\d+)-exit=\$\?' sh '7'`, Output: []string{"${1}-exit=0"},
		Write: writeCanned(notifications, "", 1)}
	reactInhibit = fakeReaction{Typed: `systemd-inhibit --what=sleep .*echo (smoke-\d+)-exit=0' sh$`, Output: []string{"${1}-pid=4242", "${1}-exit=0"},
		Write: writeCanned(powerLocks, "    PARTIAL_WAKE_LOCK              'pocketnix:guest'                          ACQ=-1s2ms (uid=10200 pid=31337)\n", 0)}
	reactUninhibit = fakeReaction{Typed: `kill "\$1"; echo (smoke-\d+)-exit=\$\?' sh '4242'`, Output: []string{"${1}-exit=0"},
		Write: writeCanned(powerLocks, powerHistory, 0)}
	reactKill = fakeReaction{Typed: `echo (smoke-\d+)-killed=\$n; echo smoke-\d+-exit=0' sh '([^']+)'`, Output: []string{"${1}-killed=1", "${1}-exit=0"},
		Remove: []string{"control/windows/${2}"}}
	reactReboot = fakeReaction{Typed: `systemctl reboot`, Reboot: true}
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
	d.can(powerLocks, powerHistory, 0)
	// One uid per Android user, as on a phone with a work profile or Private space.
	d.can("pm list packages -U dev.nickolaj.pocketnix.debug", "package:dev.nickolaj.pocketnix.debug uid:10200,1010200,1110200\n", 0)
	d.can(notifications, "", 1)
	d.can("dumpsys power | grep -m1 mWakefulness=", "  mWakefulness=Awake\n", 0)
	d.can("dumpsys window | grep -E 'mDreamingLockscreen=|isKeyguardShowing=|mShowingLockscreen=' || true", "    isKeyguardShowing=false\n", 0)
	d.can("date +%s", "1762000000\n", 0)
	// An earlier boot's GPU failure, before smoke ran.
	d.write("logcat", []byte("1761999000.000  4321  4400 E crosvm  : ResourceUnmapBlob: ErrUnspec\n"))
	return d
}

var everyTool = []fakeReaction{reactListen, reactPTY, reactNotify, reactUnnotify, reactInhibit, reactUninhibit, reactKill, reactReboot}

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
	if b, _ := os.ReadFile(filepath.Join(d.state, cannedPath(powerLocks))); strings.Contains(string(b), "WAKE_LOCK") {
		left = append(left, "wake lock")
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
		if strings.Contains(lines[0], "systemctl reboot") {
			continue
		}
		if !slices.Contains(lines, "exit") {
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
	console, _ := os.ReadFile(fakeTabPath(d.state, 0))
	if !strings.Contains(string(console), rebootLine) {
		t.Error("the guest never rebooted")
	}
	for _, e := range []string{"uninstall", "am force-stop"} {
		if hasEvent(d.events(), e) {
			t.Errorf("smoke ran %s:\n%s", e, d.events())
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
	// Interrupted once the PTY step's tab is open: the console, the listener's and its own.
	go func() {
		for len(fakeTabs(d.state)) < 3 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	steps := runSmoke(ctx, adb.Device{}, debugPackage, smokeApps, io.Discard)
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
	if console, _ := os.ReadFile(fakeTabPath(d.state, 0)); strings.Contains(string(console), rebootLine) {
		t.Error("an interrupted smoke rebooted the guest")
	}
}

func TestSmokeSkipsWithoutInputs(t *testing.T) {
	none := func(re string) fakeReaction {
		return fakeReaction{Typed: re, Output: []string{"${1}-tool=none", "${1}-exit=0"}}
	}
	toolless := []fakeReaction{
		none(`echo (smoke-\d+)-tool=socat;`), none(`notify-send -p .*echo (smoke-\d+)-exit=\$s'`), none(`systemd-inhibit --what=sleep .*echo (smoke-\d+)-exit=0' sh$`),
		reactPTY, reactKill, reactReboot,
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
			d.can("dumpsys power | grep -m1 mWakefulness=", "  mWakefulness=Asleep\n", 0)
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
			r, _, _ := doctorJSON(t, append([]string{"--smoke"}, tc.args...)...)
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
	if err != nil {
		t.Errorf("doctor --smoke on a stopped VM: %v", err)
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

// Without a GPU or guest RAM there's nothing to judge, but the launches still run.
func TestSmokeNoGPUStillLaunches(t *testing.T) {
	d := smokeDevice(t, everyTool...)
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
		if !strings.Contains(d.events(), "DEBUG_LAUNCH --es app '"+app+"'") {
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
