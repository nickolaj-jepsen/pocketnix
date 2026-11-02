package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeAPKFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
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
	if _, err := os.Stat(filepath.Join(d.cache, "pocketnix", "backup", fakeSerial)); err == nil {
		t.Error("the device's empty backup directory is left after --restore")
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

// D4b: a failed swap stays recoverable with one command.
func TestInstallFailureKeepsBackup(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	d.failInstalls("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]")

	err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
	if err == nil || !strings.Contains(err.Error(), "--restore") {
		t.Fatalf("got %v, want the --restore hint", err)
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

// Backups are per device: another phone's restore doesn't see this one's.
func TestInstallBacksUpPerSerial(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	if err := runInstall([]string{"--serial", "PHONE-A", "--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
		t.Fatal(err)
	}
	if b := d.backups(); len(b) != 1 || filepath.Base(filepath.Dir(b[0])) != "PHONE-A" {
		t.Errorf("backups = %v, want one under PHONE-A", b)
	}
	if err := runInstall([]string{"--serial", "PHONE-B", "--debug", "--restore"}); err == nil {
		t.Error("restored PHONE-A's backup onto PHONE-B")
	}
}
