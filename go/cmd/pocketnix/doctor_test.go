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
	smokeTimes.ptyPoll = controlPoll
	// outputState waits this long once, between its two reads.
	smokeTimes.exit = 50 * time.Millisecond
	smokeTimes.forward, smokeTimes.reboot, smokeTimes.boot = s, s, s
}

// cannedPath is the state path of the shell command's canned answer, for a reaction to write.
func cannedPath(cmdline string) string { return filepath.Join("canned", fakeCannedKey(cmdline)) }

const (
	powerLocks = "dumpsys power | grep -F pocketnix:guest"
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
	reactPTY = fakeReaction{Typed: `head -c \d+ /dev/urandom \| base64; .*echo (smoke-\d+)-exit=`, Output: []string{"AAAAAAAAAAAA", "${1}-exit=0"},
		Write: map[string]string{"control/guest-pty": "1000\n"}}
	reactNotify = fakeReaction{Typed: `notify-send -p .*echo (smoke-\d+)-exit=\$s' sh '[^']*' '([^']+)'`, Output: []string{"${1}-id=7", "${1}-exit=0"},
		Append: map[string]string{"notifications": "  NotificationRecord(0x0abc1234: pkg=dev.nickolaj.pocketnix.debug user=UserHandle{0} id=7 tag=guest importance=3 key=0|dev.nickolaj.pocketnix.debug|7|guest|10200: Notification(channel=guest))\n        android.text=String (${2})\n"}}
	reactUnnotify = fakeReaction{Typed: `CloseNotification u "\$1"; echo (smoke-\d+)-exit=\$\?' sh '7'`, Output: []string{"${1}-exit=0"},
		Write: map[string]string{"notifications": ""}}
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
	d.can("dumpsys power | grep -m1 mWakefulness=", "  mWakefulness=Awake\n", 0)
	d.can("dumpsys window | grep -E 'mDreamingLockscreen=|isKeyguardShowing=|mShowingLockscreen=' || true", "    isKeyguardShowing=false\n", 0)
	d.can("date +%s", "1762000000\n", 0)
	// An earlier boot's GPU failure, before smoke ran.
	d.write("logcat", []byte("1761999000.000  4321  4400 E crosvm  : ResourceUnmapBlob: ErrUnspec\n"))
	return d
}

// The guest's PTY timing file: base64 ran for 100 s, read 130 s after it started.
var (
	reactPTYTime = fakeReaction{Typed: `set -- \$\(cat .*echo (smoke-\d+)-exit=0`,
		Output: []string{"${1}-start=1000", "${1}-end=1100", "${1}-now=1130", "${1}-exit=0"}}
	// base64 hasn't exited by the time the file is read.
	reactPTYRunning = fakeReaction{Typed: reactPTYTime.Typed,
		Output: []string{"${1}-start=1000", "${1}-end=", "${1}-now=1600", "${1}-exit=0"}}
	reactPTYClean = fakeReaction{Typed: `rm -f [^;]*; echo (smoke-\d+)-exit=\$\?`, Output: []string{"${1}-exit=0"},
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
	if b, _ := os.ReadFile(filepath.Join(d.state, cannedPath(powerLocks))); strings.Contains(string(b), "WAKE_LOCK") {
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
		if strings.Contains(lines[0], "systemctl reboot") {
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

// ptyTab is the tab the PTY command was typed into, -1 if none.
func ptyTab(d *fakeDevice) int {
	for tab, lines := range d.typed() {
		if strings.Contains(lines[0], "base64") {
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
	steps := runSmoke(context.Background(), dev, debugPackage, nil, io.Discard)
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

func isReboot(line string) bool { return strings.Contains(line, "systemctl reboot") }

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
	steps := runSmoke(ctx, adb.Device{}, debugPackage, nil, io.Discard)
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
	go func() { done <- runSmoke(ctx, adb.Device{}, debugPackage, nil, io.Discard) }()
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
	typed := reactReboot.Typed + `.*echo (smoke-\d+)-exit=`
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
		{name: "no-console-line", react: &fakeReaction{Typed: typed, Write: map[string]string{"control/booting": "1"}}},
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
			steps := runSmoke(ctx, adb.Device{}, debugPackage, nil, io.Discard)
			if st, _ := stepNamed(steps, "reboot"); st.OK == nil || *st.OK {
				t.Errorf("reboot: %+v, want a failure", st)
			}
			if left := leftovers(d); len(left) > 0 {
				t.Errorf("smoke left %q", left)
			}
		})
	}
}

func TestSmokeSkipsWithoutInputs(t *testing.T) {
	none := func(re string) fakeReaction {
		return fakeReaction{Typed: re, Output: []string{"${1}-tool=none", "${1}-exit=0"}}
	}
	toolless := []fakeReaction{
		none(`echo (smoke-\d+)-tool=socat;`), none(`notify-send -p .*echo (smoke-\d+)-exit=\$s'`), none(`systemd-inhibit --what=sleep .*echo (smoke-\d+)-exit=0' sh$`),
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

// With its transcript's ring wrapped, the terminal joins the marker to the row
// before or after it (TerminalRow.clear keeps mLineWrap), as a phone's 64 MiB does.
func TestSmokePTYFindsAJoinedMarker(t *testing.T) {
	for name, tc := range map[string]struct{ line, want string }{
		"own-row":           {"${1}-exit=0", "on its own row"},
		"to-the-row-before": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAA            ${1}-exit=0", "joined to the row before"},
		"to-the-row-after":  {"${1}-exit=0                          ~ > ", "joined to the row after"},
		"to-both":           {"AAAA    ${1}-exit=0    ~ > ", "joined to the rows before and after"},
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
