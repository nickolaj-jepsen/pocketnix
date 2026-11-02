package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPKFile writes a fakeAPK holding build, of the debug package when name
// says so, as gradle's names do.
func fakeAPKFile(t *testing.T, name, build string) string {
	t.Helper()
	pkg := releasePackage
	if strings.Contains(name, "debug") {
		pkg = debugPackage
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, fakeAPK(pkg, build), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// backups lists install's backup files under the fake device's cache dir.
func (d *fakeDevice) backups() []string {
	m, _ := filepath.Glob(filepath.Join(d.cache, "pocketnix", "backup", "*", "*.apk"))
	return m
}

// eventIndex is the line number of the first event starting with prefix, or -1.
func eventIndex(events, prefix string) int {
	for i, line := range strings.Split(events, "\n") {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

const (
	debugStart = "am start -n dev.nickolaj.pocketnix.debug/dev.nickolaj.pocketnix.ControlActivity -a dev.nickolaj.pocketnix.action.START"
	debugStop  = "am start -n dev.nickolaj.pocketnix.debug/dev.nickolaj.pocketnix.ControlActivity -a dev.nickolaj.pocketnix.action.STOP"
)

func TestInstallSwapsRunningVM(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	apk := fakeAPKFile(t, "app-debug.apk", "a dev build")

	if err := runInstall([]string{"--debug", apk}); err != nil {
		t.Fatal(err)
	}
	if got := string(d.installedAPK()); got != "a dev build" {
		t.Errorf("installed APK = %q", got)
	}
	if !d.running() {
		t.Error("the VM isn't running again")
	}
	ev := d.events()
	for _, line := range []string{
		"install -r -d app-debug.apk",
		"grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE",
		"grant dev.nickolaj.pocketnix.debug android.permission.USE_CUSTOM_VIRTUAL_MACHINE",
	} {
		if !strings.Contains(ev, line+"\n") {
			t.Errorf("device never saw %q; events:\n%s", line, ev)
		}
	}
	if strings.Contains(ev, "uninstall") {
		t.Errorf("uninstalled the app; events:\n%s", ev)
	}
	if strings.Contains(ev, "vm killed\n") {
		t.Errorf("installed over a running VM instead of stopping it; events:\n%s", ev)
	}
	stop := strings.Index(ev, "-a dev.nickolaj.pocketnix.action.STOP")
	if stop < 0 || stop > strings.Index(ev, "install -r -d") {
		t.Errorf("no STOP before the install; events:\n%s", ev)
	}
	if b := d.backups(); len(b) != 1 {
		t.Errorf("backups = %v, want one", b)
	}
}

// The install, the grants and then the restart, which is a bare START: its
// extras would overwrite the app's saved settings.
func TestInstallOrdersTheSwap(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}
	ev := d.events()
	order := []string{
		debugStop + "\n",
		"install -r -d app-debug.apk\n",
		"grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE\n",
		"grant dev.nickolaj.pocketnix.debug android.permission.USE_CUSTOM_VIRTUAL_MACHINE\n",
		debugStart + "\n",
	}
	last := -1
	for _, line := range order {
		i := strings.Index(ev, line)
		if i <= last {
			t.Fatalf("want %q after the events before it in %q; events:\n%s", line, order, ev)
		}
		last = i
	}
}

func TestInstallRestore(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}

	if err := runInstall([]string{"--debug", "--restore"}); err != nil {
		t.Fatal(err)
	}
	if got := string(d.installedAPK()); got != "the owner's build" {
		t.Errorf("installed APK = %q, want the backup", got)
	}
	if b := d.backups(); len(b) != 0 {
		t.Errorf("backup left after --restore: %v", b)
	}
	if left, _ := os.ReadDir(filepath.Join(d.cache, "pocketnix", "backup")); len(left) != 0 {
		t.Errorf("left under the backup directory after --restore: %v", left)
	}
	if !d.running() {
		t.Error("the VM isn't running after --restore")
	}
	if ev := d.events(); strings.Contains(ev, "vm killed\n") || strings.Contains(ev, "uninstall") {
		t.Errorf("events:\n%s", ev)
	}
}

func TestInstallRestoreNeedsABackup(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	if err := runInstall([]string{"--debug", "--restore"}); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Errorf("got %v, want a missing backup", err)
	}
	if hasEvent(d.events(), "install ") {
		t.Errorf("installed without a backup; events:\n%s", d.events())
	}
}

// A failed swap stays recoverable with one command.
func TestInstallFailureKeepsBackup(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	d.failInstalls("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]")

	err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
	if err == nil || !strings.Contains(err.Error(), "--restore") {
		t.Fatalf("got %v, want the --restore hint", err)
	}
	if !strings.Contains(err.Error(), "INSTALL_FAILED_UPDATE_INCOMPATIBLE") {
		t.Errorf("got %v, want pm's reason, which adb prints on stdout", err)
	}
	b := d.backups()
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if got, _ := os.ReadFile(b[0]); string(got) != "the owner's build" {
		t.Errorf("backup = %q", got)
	}
	if d.running() {
		t.Error("the VM runs after a failed install")
	}
	if ev := d.events(); strings.Contains(ev, "uninstall") || strings.Contains(ev, "vm killed\n") {
		t.Errorf("events:\n%s", ev)
	}

	d.allowInstalls()
	if err := runInstall([]string{"--debug", "--restore"}); err != nil {
		t.Fatal(err)
	}
	if got := string(d.installedAPK()); got != "the owner's build" {
		t.Errorf("installed APK = %q after --restore", got)
	}
	if !d.running() {
		t.Error("--restore didn't start the VM")
	}
}

// Only pm's own exit 1 means the app isn't there: anything else could hide a running VM.
func TestInstallRefusesWhenPMPathFails(t *testing.T) {
	const cmdline = "pm path " + debugPackage
	for name, setup := range map[string]func(d *fakeDevice){
		"adb": func(d *fakeDevice) { d.drop(cmdline, "", "error: closed\n") },
		"pm":  func(d *fakeDevice) { d.can(cmdline, "cmd: Failure calling service package\n", 20) },
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.installAPK([]byte("the owner's build"))
			d.setRunning(true)
			setup(d)
			if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err == nil {
				t.Error("installed without knowing whether a VM runs")
			}
			if ev := d.events(); hasEvent(ev, "install ") || hasEvent(ev, "am start") {
				t.Errorf("acted on the device; events:\n%s", ev)
			}
			if !d.running() {
				t.Error("the VM stopped")
			}
		})
	}
}

// A chain of dev installs never overwrites the owner's build.
func TestInstallKeepsFirstBackup(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	for _, b := range []string{"dev build 1", "dev build 2"} {
		if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", b)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(d.installedAPK()); got != "dev build 2" {
		t.Errorf("installed APK = %q", got)
	}
	b := d.backups()
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if got, _ := os.ReadFile(b[0]); string(got) != "the owner's build" {
		t.Errorf("backup = %q, want the first build", got)
	}
}

// An APK of the other package would replace that app and kill its VM, with
// nothing backed up. The refusal names the flag that was probably meant.
func TestInstallRefusesTheOtherPackage(t *testing.T) {
	for _, tc := range []struct {
		name, running, apk string
		args               []string
	}{
		{name: "debug-apk-without-debug", running: debugPackage, apk: "app-debug.apk"},
		{name: "release-apk-with-debug", running: releasePackage, apk: "app-release.apk", args: []string{"--debug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.installAPKAs(tc.running, []byte("the owner's build"))
			d.setRunning(true)
			err := runInstall(append(tc.args, fakeAPKFile(t, tc.apk, "a dev build")))
			if err == nil || !strings.Contains(err.Error(), "--debug") {
				t.Errorf("got %v, want a refusal naming --debug", err)
			}
			if ev := d.events(); hasEvent(ev, "install ") || hasEvent(ev, "vm killed") || hasEvent(ev, "am start") {
				t.Errorf("acted on the device; events:\n%s", ev)
			}
			if !d.running() {
				t.Error("the VM stopped")
			}
			if got := string(d.installedAPK()); got != "the owner's build" {
				t.Errorf("installed APK = %q", got)
			}
		})
	}
}

func TestInstallRefusesAFileThatIsNoAPK(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	p := filepath.Join(t.TempDir(), "app-debug.apk")
	os.WriteFile(p, []byte("not a zip"), 0o644)
	if err := runInstall([]string{"--debug", p}); err == nil {
		t.Error("installed a file that isn't an APK")
	}
	if ev := d.events(); hasEvent(ev, "install ") || hasEvent(ev, "am start") {
		t.Errorf("acted on the device; events:\n%s", ev)
	}
}

func TestInstallRefusesLargeAPK(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	d.setAPKSize(100 << 20)
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err == nil || !strings.Contains(err.Error(), "maxPull") {
		t.Errorf("got %v, want a refusal naming maxPull", err)
	}
	ev := d.events()
	if eventIndex(ev, debugStop) >= 0 || hasEvent(ev, "install ") || hasEvent(ev, "pull ") {
		t.Errorf("acted on the device; events:\n%s", ev)
	}
	if !d.running() {
		t.Error("the VM stopped")
	}
	if b := d.backups(); len(b) != 0 {
		t.Errorf("backups = %v", b)
	}
}

func TestInstallRefusesSplitAPK(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	d.setPMPath("package:/data/app/~~x==/base.apk\npackage:/data/app/~~x==/split_config.arm64_v8a.apk\n")
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err == nil || !strings.Contains(err.Error(), "split") {
		t.Errorf("got %v, want a split-install refusal", err)
	}
	ev := d.events()
	if eventIndex(ev, debugStop) >= 0 || hasEvent(ev, "install ") {
		t.Errorf("acted on the device; events:\n%s", ev)
	}
}

// A stopped VM stays stopped, and --no-start keeps a running one down.
func TestInstallStartsOnlyWhatRan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		args    []string
	}{
		{"stopped", false, nil},
		{"no-start", true, []string{"--no-start"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.installAPK([]byte("the owner's build"))
			d.setRunning(tc.running)
			args := append([]string{"--debug"}, tc.args...)
			if err := runInstall(append(args, fakeAPKFile(t, "app-debug.apk", "a dev build"))); err != nil {
				t.Fatal(err)
			}
			if ev := d.events(); eventIndex(ev, debugStart) >= 0 || d.running() {
				t.Errorf("started the VM; events:\n%s", ev)
			}
			if got := string(d.installedAPK()); got != "a dev build" {
				t.Errorf("installed APK = %q", got)
			}
		})
	}
}

// A phone without the app gets it, with nothing to back up.
func TestInstallOntoAFreshPhone(t *testing.T) {
	d := newFakeDevice(t)
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}
	if got := string(d.installedAPK()); got != "a dev build" {
		t.Errorf("installed APK = %q", got)
	}
	if b := d.backups(); len(b) != 0 {
		t.Errorf("backups = %v", b)
	}
}

// Backups go by the phone: another one's restore doesn't see this one's.
func TestInstallBacksUpPerDevice(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setProp("ro.serialno", "PHONE-A")
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}
	if b := d.backups(); len(b) != 1 {
		t.Errorf("backups = %v, want one", b)
	}
	d.setProp("ro.serialno", "PHONE-B")
	if err := runInstall([]string{"--debug", "--restore"}); err == nil {
		t.Error("restored PHONE-A's backup onto PHONE-B")
	}
	d.setProp("ro.serialno", "PHONE-A")
	if err := runInstall([]string{"--debug", "--restore"}); err != nil {
		t.Errorf("PHONE-A's own restore: %v", err)
	}
}

// Over USB and over Wi-Fi the phone has different adb serials, and the same backup.
func TestInstallFindsItsBackupOverANewTransport(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setProp("ro.serialno", recordHWSerial)
	const usb, wifi = "--serial=" + recordHWSerial, "--serial=192.0.2.1:5555"
	if err := runInstall([]string{usb, "--debug", fakeAPKFile(t, "app-debug.apk", "dev build 1")}); err != nil {
		t.Fatal(err)
	}
	if err := runInstall([]string{wifi, "--debug", fakeAPKFile(t, "app-debug.apk", "dev build 2")}); err != nil {
		t.Fatal(err)
	}
	if b := d.backups(); len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if err := runInstall([]string{wifi, "--debug", "--restore"}); err != nil {
		t.Fatal(err)
	}
	if got := string(d.installedAPK()); got != "the owner's build" {
		t.Errorf("installed APK = %q, want the first build", got)
	}
}

// A phone that hides ro.serialno still keeps its backup apart from another's.
func TestInstallWithoutTheSerialProp(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	if err := runInstall([]string{"--serial", "PHONE-A", "--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}
	if err := runInstall([]string{"--serial", "PHONE-B", "--debug", "--restore"}); err == nil {
		t.Error("restored PHONE-A's backup onto PHONE-B")
	}
	if err := runInstall([]string{"--serial", "PHONE-A", "--debug", "--restore"}); err != nil {
		t.Errorf("PHONE-A's own restore: %v", err)
	}
}

// The phone's own serial keys the backup and goes nowhere else: not its path,
// the log or an error. The restore command names the transport it was given.
func TestInstallKeepsTheSerialToItself(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setProp("ro.serialno", recordHWSerial)
	var log strings.Builder
	install := func(args ...string) error {
		t.Helper()
		o, err := parseInstall(append([]string{"--serial", recordSerial, "--debug"}, args...))
		if err != nil {
			t.Fatal(err)
		}
		o.log = &log
		return o.run()
	}
	if err := install("--restore"); err == nil || strings.Contains(err.Error(), recordHWSerial) {
		t.Errorf("--restore without a backup: %v", err)
	}
	d.failInstalls("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]")
	err := install(fakeAPKFile(t, "app-debug.apk", "a dev build"))
	if err == nil || !strings.Contains(err.Error(), "--serial "+recordSerial) || strings.Contains(err.Error(), recordHWSerial) {
		t.Errorf("a failed install: %v", err)
	}
	if err := install("--restore"); err == nil || strings.Contains(err.Error(), recordHWSerial) {
		t.Errorf("a failed restore: %v", err)
	}
	if strings.Contains(log.String(), recordHWSerial) {
		t.Errorf("the log holds the serial:\n%s", log.String())
	}
	if b := d.backups(); len(b) != 1 || strings.Contains(b[0], recordHWSerial) {
		t.Errorf("backups = %v, want one, whose path doesn't hold the serial", b)
	}
}
