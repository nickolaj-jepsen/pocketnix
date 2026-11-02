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

// crashingAPKFile writes a debug APK whose process dies at start, and so whose
// providers never answer.
func crashingAPKFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app-debug.apk")
	apk := zipOf(map[string][]byte{
		"AndroidManifest.xml": axmlManifest(false, "package", debugPackage),
		"build":               []byte("a crashing build"),
		fakeCrashes:           nil,
	})
	if err := os.WriteFile(p, apk, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// versionedAPKFile writes fakeVersionedAPK to a file named as gradle's are.
func versionedAPKFile(t *testing.T, pkg, build string, code uint32, debuggable bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.apk")
	if err := os.WriteFile(p, fakeVersionedAPK(pkg, build, code, debuggable), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// installLogged runs install, and returns what it logged too.
func installLogged(t *testing.T, args ...string) (string, error) {
	t.Helper()
	o, err := parseInstall(args)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	o.log = &log
	err = o.run()
	return log.String(), err
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
	if strings.Contains(ev, "uninstall") {
		t.Errorf("uninstalled the app; events:\n%s", ev)
	}
	if strings.Contains(ev, "vm killed\n") {
		t.Errorf("installed over a running VM instead of stopping it; events:\n%s", ev)
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
	steps := [][]string{
		{debugStop},
		{"install -r -d app-debug.apk"},
		{
			"grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE",
			"grant dev.nickolaj.pocketnix.debug android.permission.USE_CUSTOM_VIRTUAL_MACHINE",
		},
		{debugStart},
	}
	after := -1
	for _, step := range steps {
		latest := after
		for _, line := range step {
			i := strings.Index(ev, line+"\n")
			if i <= after {
				t.Fatalf("want %q after the steps before it in %q; events:\n%s", line, steps, ev)
			}
			latest = max(latest, i)
		}
		after = latest
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

// A failed swap that did replace the app stays recoverable with one command.
func TestInstallFailureKeepsBackup(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	d.failInstallsLate("Failure [INSTALL_FAILED_INTERNAL_ERROR: the connection went]")

	err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
	if err == nil || !strings.Contains(err.Error(), "--restore") {
		t.Fatalf("got %v, want the --restore hint", err)
	}
	if !strings.Contains(err.Error(), "INSTALL_FAILED_INTERNAL_ERROR") {
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

// A failed install that left the app as it was changes nothing: the old build
// goes on as if the install was never tried.
func TestInstallFailureThatReplacedNothing(t *testing.T) {
	const reason = "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]"

	t.Run("restarts a running VM and drops this run's backup", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "INSTALL_FAILED_UPDATE_INCOMPATIBLE") {
			t.Fatalf("got %v, want pm's reason", err)
		}
		// Nobody may take the way past a signature mismatch lightly.
		if !strings.Contains(err.Error(), "uninstall") || !strings.Contains(err.Error(), "everything in the guest") {
			t.Errorf("got %v, want it to say an uninstall deletes everything in the guest", err)
		}
		if !strings.Contains(err.Error(), "untouched") || strings.Contains(err.Error(), "--restore") {
			t.Errorf("got %v, want it to say the old build is untouched, with no --restore advice", err)
		}
		if got := string(d.installedAPK()); got != "the owner's build" {
			t.Errorf("installed APK = %q", got)
		}
		if !d.running() {
			t.Error("the VM isn't running again")
		}
		if b := d.backups(); len(b) != 0 {
			t.Errorf("backups = %v, want this run's gone", b)
		}
		ev := d.events()
		if stop, start := eventIndex(ev, debugStop), eventIndex(ev, debugStart); stop < 0 || start < stop {
			t.Errorf("want a STOP, then a START; events:\n%s", ev)
		}
		if strings.Contains(ev, "uninstall") || strings.Contains(ev, "vm killed\n") {
			t.Errorf("events:\n%s", ev)
		}
	})

	t.Run("keeps a backup from an earlier run", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "dev build 1")}); err != nil {
			t.Fatal(err)
		}
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "dev build 2")})
		if err == nil || strings.Contains(err.Error(), "--restore") {
			t.Errorf("got %v, want a failure with no --restore advice", err)
		}
		if got := string(d.installedAPK()); got != "dev build 1" {
			t.Errorf("installed APK = %q", got)
		}
		if !d.running() {
			t.Error("the VM isn't running again")
		}
		b := d.backups()
		if len(b) != 1 {
			t.Fatalf("backups = %v, want the earlier one", b)
		}
		if got, _ := os.ReadFile(b[0]); string(got) != "the owner's build" {
			t.Errorf("backup = %q", got)
		}
	})

	t.Run("doesn't start a VM that wasn't running", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "untouched") || strings.Contains(err.Error(), "VM is stopped") {
			t.Errorf("got %v, want it to say the old build is untouched, and nothing of a stopped VM", err)
		}
		if ev := d.events(); eventIndex(ev, debugStart) >= 0 || d.running() {
			t.Errorf("started the VM; events:\n%s", ev)
		}
		if b := d.backups(); len(b) != 0 {
			t.Errorf("backups = %v, want this run's gone", b)
		}
	})

	t.Run("leaves the VM down with --no-start", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", "--no-start", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "--no-start") {
			t.Errorf("got %v, want it to say --no-start kept the VM down", err)
		}
		if d.running() {
			t.Error("the VM runs despite --no-start")
		}
	})

	t.Run("a phone without the app stays without it", func(t *testing.T) {
		d := newFakeDevice(t)
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || strings.Contains(err.Error(), "--restore") || strings.Contains(err.Error(), "VM is stopped") {
			t.Errorf("got %v, want a failure with no advice about a backup or a stopped VM", err)
		}
	})

	t.Run("a failed restore keeps the backup and the build it would replace", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err != nil {
			t.Fatal(err)
		}
		d.failInstalls(reason)
		err := runInstall([]string{"--debug", "--restore"})
		if err == nil || !strings.Contains(err.Error(), "untouched") {
			t.Errorf("got %v, want it to say the installed build is untouched", err)
		}
		if got := string(d.installedAPK()); got != "a dev build" {
			t.Errorf("installed APK = %q", got)
		}
		if !d.running() {
			t.Error("the VM isn't running again")
		}
		if b := d.backups(); len(b) != 1 {
			t.Errorf("backups = %v, want the one it failed to restore", b)
		}
	})
}

// When the phone can't say what the install did, or it did replace the app, the
// VM stays down and the backup stays, and the message says only what's known.
func TestInstallFailureThatMayHaveReplaced(t *testing.T) {
	const reason = "Failure [INSTALL_FAILED_INTERNAL_ERROR: the connection went]"

	t.Run("the phone drops off adb", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		d.dropAtInstall(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "--restore") || !strings.Contains(err.Error(), "VM is stopped") {
			t.Fatalf("got %v, want the stopped VM and the --restore hint", err)
		}
		if strings.Contains(err.Error(), "was replaced") {
			t.Errorf("got %v, which claims to know the app was replaced", err)
		}
		if d.running() {
			t.Error("the VM runs after an install that may have replaced the app")
		}
		if b := d.backups(); len(b) != 1 {
			t.Errorf("backups = %v, want one", b)
		}
		d.reconnect()
		if err := runInstall([]string{"--debug", "--restore"}); err != nil {
			t.Fatal(err)
		}
		if !d.running() {
			t.Error("--restore didn't start the VM")
		}
	})

	t.Run("a VM that wasn't running isn't called stopped", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.failInstallsLate(reason)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "--restore") || strings.Contains(err.Error(), "VM is stopped") {
			t.Errorf("got %v, want the --restore hint, and nothing of a stopped VM", err)
		}
	})

	t.Run("a failed grant after the install counts as replaced", func(t *testing.T) {
		d := newFakeDevice(t)
		d.installAPK([]byte("the owner's build"))
		d.setRunning(true)
		d.can("pm grant "+debugPackage+" android.permission.MANAGE_VIRTUAL_MACHINE", "Exception occurred while executing 'grant'\n", 255)
		err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")})
		if err == nil || !strings.Contains(err.Error(), "--restore") || !strings.Contains(err.Error(), "VM is stopped") {
			t.Errorf("got %v, want the stopped VM and the --restore hint", err)
		}
		if d.running() {
			t.Error("the VM runs after a failed grant")
		}
		if got := string(d.installedAPK()); got != "a dev build" {
			t.Errorf("installed APK = %q", got)
		}
		if b := d.backups(); len(b) != 1 {
			t.Errorf("backups = %v, want one", b)
		}
	})
}

// The way back from a dev build that crashes at start can't depend on that build.
func TestInstallRestoreRecoversACrashingBuild(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setRunning(true)
	if err := runInstall([]string{"--debug", "--no-start", crashingAPKFile(t)}); err != nil {
		t.Fatal(err)
	}
	if d.running() {
		t.Fatal("the VM runs over a build that can't")
	}

	// Anything but --restore keeps failing closed: this could be any provider failure.
	if err := runInstall([]string{"--debug", fakeAPKFile(t, "app-debug.apk", "a dev build")}); err == nil {
		t.Error("installed over an app that doesn't say whether a VM runs")
	}
	if got := string(d.installedAPK()); got != "a crashing build" {
		t.Errorf("a normal install went ahead: installed APK = %q", got)
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
	if !d.running() {
		t.Error("the VM isn't running after --restore")
	}
	if ev := d.events(); strings.Contains(ev, "vm killed\n") || strings.Contains(ev, "uninstall") {
		t.Errorf("events:\n%s", ev)
	}
}

// With no status to go by, --restore looks for the VM's process: replacing the
// app under a real VM is a hard power-off.
func TestInstallRestoreOverAnAppThatDoesntAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(d *fakeDevice)
		refused bool
		// want is the refusal's token.
		want string
	}{
		{name: "a-vm-still-runs", setup: func(d *fakeDevice) {
			d.setRunning(true)
		}, refused: true, want: "crosvm"},
		{name: "a-vm-runs-under-another-android-user", setup: func(d *fakeDevice) {
			d.can("pm list packages -U "+debugPackage, "package:"+debugPackage+" uid:10200,1010200\n", 0)
			d.can(vmProcessesCommand, "1010200 crosvm_pocketnix\n", 0)
		}, refused: true, want: "crosvm"},
		{name: "a-vm-and-no-uid-to-tell-whose", setup: func(d *fakeDevice) {
			d.can("pm list packages -U "+debugPackage, "", 0)
			d.can(vmProcessesCommand, "10300 crosvm_pocketnix\n", 0)
		}, refused: true, want: "crosvm"},
		{name: "adb-loses-the-phone", setup: func(d *fakeDevice) {
			d.drop(vmProcessesCommand, "", "adb: device offline\n")
		}, refused: true, want: "offline"},
		{name: "ps-fails", setup: func(d *fakeDevice) {
			d.can(vmProcessesCommand, "ps: bad -o\n", 1)
		}, refused: true, want: "can't tell"},
		{name: "ps-lists-nothing", setup: func(d *fakeDevice) {
			d.can(vmProcessesCommand, "", 0)
		}, refused: true, want: "can't tell"},
		{name: "another-apps-vm", setup: func(d *fakeDevice) {
			d.can(vmProcessesCommand, "10100 crosvm_pocketnix\n", 0)
		}},
		{name: "no-vm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.installAPK([]byte("the owner's build"))
			if err := runInstall([]string{"--debug", crashingAPKFile(t)}); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(d)
			}
			wasRunning := d.running()
			err := runInstall([]string{"--debug", "--no-start", "--restore"})
			if !tc.refused {
				if err != nil {
					t.Fatal(err)
				}
				if got := string(d.installedAPK()); got != "the owner's build" {
					t.Errorf("installed APK = %q", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want a refusal naming %q", err, tc.want)
			}
			if got := string(d.installedAPK()); got != "a crashing build" {
				t.Errorf("installed APK = %q", got)
			}
			if b := d.backups(); len(b) != 1 {
				t.Errorf("backups = %v, want it left in place", b)
			}
			if ev := d.events(); eventIndex(ev, debugStop) >= 0 || strings.Contains(ev, "vm killed\n") {
				t.Errorf("acted on the VM; events:\n%s", ev)
			}
			if d.running() != wasRunning {
				t.Errorf("the VM's running state became %t", d.running())
			}
		})
	}
}

// A release build on a user build's phone: Android won't take an older one over
// it with -d, so install says so before the VM goes down, not after.
func TestInstallRefusesADowngradeAndroidWouldRefuse(t *testing.T) {
	d := newFakeDevice(t)
	d.setProp("ro.debuggable", "0")
	d.installAPKAs(releasePackage, fakeVersionedAPK(releasePackage, "version 2", 20000, false))
	d.setRunning(true)
	err := runInstall([]string{versionedAPKFile(t, releasePackage, "version 1", 10000, false)})
	if err == nil || !strings.Contains(err.Error(), "10000") || !strings.Contains(err.Error(), "20000") {
		t.Errorf("got %v, want a refusal naming both versionCodes", err)
	}
	ev := d.events()
	if hasEvent(ev, "install ") || hasEvent(ev, "am start") || hasEvent(ev, "pull ") {
		t.Errorf("acted on the device; events:\n%s", ev)
	}
	if !d.running() {
		t.Error("the VM stopped")
	}
	if b := d.backups(); len(b) != 0 {
		t.Errorf("backups = %v, want none from a refused run", b)
	}
}

// A newer release build closes --restore's way back: the log says so when it
// goes on, and the restore itself refuses before anything changes.
func TestInstallRestoreRefusesADowngrade(t *testing.T) {
	d := newFakeDevice(t)
	d.setProp("ro.debuggable", "0")
	d.installAPKAs(releasePackage, fakeVersionedAPK(releasePackage, "version 1", 10000, false))
	d.setRunning(true)
	log, err := installLogged(t, versionedAPKFile(t, releasePackage, "version 2", 20000, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, "--restore") {
		t.Errorf("no word of --restore going away in the log:\n%s", log)
	}
	if !d.running() {
		t.Fatal("the VM isn't running again")
	}
	stops := strings.Count(d.events(), "action.STOP")

	err = runInstall([]string{"--restore"})
	if err == nil || !strings.Contains(err.Error(), "10000") || !strings.Contains(err.Error(), "20000") {
		t.Errorf("got %v, want a refusal naming both versionCodes", err)
	}
	if got := string(d.installedAPK()); got != "version 2" {
		t.Errorf("installed APK = %q", got)
	}
	if !d.running() {
		t.Error("the VM stopped")
	}
	if got := strings.Count(d.events(), "action.STOP"); got != stops {
		t.Errorf("sent another STOP; events:\n%s", d.events())
	}
	if b := d.backups(); len(b) != 1 {
		t.Errorf("backups = %v, want it left in place", b)
	}
}

// Nothing refuses a downgrade over a debuggable build, or on a debuggable OS.
func TestInstallAllowsADowngradeAndroidAllows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pkg          string
		installedDbg bool
		osDebuggable string
	}{
		{name: "debuggable-build", pkg: debugPackage, installedDbg: true, osDebuggable: "0"},
		{name: "debuggable-os", pkg: releasePackage, osDebuggable: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.setProp("ro.debuggable", tc.osDebuggable)
			d.installAPKAs(tc.pkg, fakeVersionedAPK(tc.pkg, "version 2", 20000, tc.installedDbg))
			d.setRunning(true)
			args := []string{versionedAPKFile(t, tc.pkg, "version 1", 10000, tc.installedDbg)}
			if tc.pkg == debugPackage {
				args = append([]string{"--debug"}, args...)
			}
			if err := runInstall(args); err != nil {
				t.Fatal(err)
			}
			if got := string(d.installedAPK()); got != "version 1" {
				t.Errorf("installed APK = %q", got)
			}
			if !d.running() {
				t.Error("the VM isn't running again")
			}
		})
	}
}

// What can't be read is no reason to refuse: adb decides, and when Android
// refuses after all, nothing was replaced and the VM comes back.
func TestInstallLetsAdbDecideWithoutTheFacts(t *testing.T) {
	const dumpsys = "dumpsys package " + releasePackage
	older := func(t *testing.T) string { return versionedAPKFile(t, releasePackage, "version 1", 10000, false) }
	for _, tc := range []struct {
		name  string
		setup func(d *fakeDevice)
		apk   func(t *testing.T) string
		// refused is Android's own verdict on what install sends it.
		refused bool
	}{
		{name: "os-debuggable-unreadable", setup: func(d *fakeDevice) {
			d.setProp("ro.debuggable", "")
		}, apk: older, refused: true},
		{name: "installed-build-unreadable", setup: func(d *fakeDevice) {
			d.can(dumpsys, "Can't find service: package\n", 1)
		}, apk: older, refused: true},
		{name: "installed-version-missing", setup: func(d *fakeDevice) {
			d.can(dumpsys, "Packages:\n  Package ["+releasePackage+"] (a1b2c3):\n    flags=[ HAS_CODE ]\n", 0)
		}, apk: older, refused: true},
		{name: "installed-flags-missing", setup: func(d *fakeDevice) {
			d.can(dumpsys, "Packages:\n  Package ["+releasePackage+"] (a1b2c3):\n    versionCode=20000 minSdk=34\n", 0)
		}, apk: older, refused: true},
		{name: "apk-version-unreadable", apk: func(t *testing.T) string {
			return fakeAPKFile(t, "app-release.apk", "no version to read")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.setProp("ro.debuggable", "0")
			d.installAPKAs(releasePackage, fakeVersionedAPK(releasePackage, "version 2", 20000, false))
			d.setRunning(true)
			if tc.setup != nil {
				tc.setup(d)
			}
			err := runInstall([]string{tc.apk(t)})
			if !hasEvent(d.events(), "install ") {
				t.Errorf("refused on a guess: %v", err)
			}
			if tc.refused != (err != nil) {
				t.Errorf("got %v, want refused by Android = %t", err, tc.refused)
			}
			if tc.refused && (err == nil || !strings.Contains(err.Error(), "INSTALL_FAILED_VERSION_DOWNGRADE")) {
				t.Errorf("got %v, want Android's refusal", err)
			}
			if !d.running() {
				t.Error("the VM didn't come back")
			}
		})
	}
}

// The warning about --restore is for the one case where it'll really be refused.
func TestInstallWarnsOnlyWhenARestoreWouldBeRefused(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pkg          string
		code         uint32
		debuggable   bool
		osDebuggable string
	}{
		{name: "same-version", pkg: releasePackage, code: 10000, osDebuggable: "0"},
		{name: "debuggable-apk", pkg: debugPackage, code: 20000, debuggable: true, osDebuggable: "0"},
		{name: "debuggable-os", pkg: releasePackage, code: 20000, osDebuggable: "1"},
		{name: "os-unreadable", pkg: releasePackage, code: 20000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.setProp("ro.debuggable", tc.osDebuggable)
			d.installAPKAs(tc.pkg, fakeVersionedAPK(tc.pkg, "version 1", 10000, tc.debuggable))
			args := []string{versionedAPKFile(t, tc.pkg, "another", tc.code, tc.debuggable)}
			if tc.pkg == debugPackage {
				args = append([]string{"--debug"}, args...)
			}
			log, err := installLogged(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(log, "--restore") {
				t.Errorf("warned about --restore:\n%s", log)
			}
		})
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
	d.setProp("ro.serialno", "PHONE-A")
	const usb, wifi = "--serial=PHONE-A", "--serial=192.0.2.1:5555"
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
	const adbSerial, hwSerial = "TRANSPORT-A", "PHONE-A"
	d := newFakeDevice(t)
	d.installAPK([]byte("the owner's build"))
	d.setProp("ro.serialno", hwSerial)
	var log strings.Builder
	install := func(args ...string) error {
		t.Helper()
		o, err := parseInstall(append([]string{"--serial", adbSerial, "--debug"}, args...))
		if err != nil {
			t.Fatal(err)
		}
		o.log = &log
		return o.run()
	}
	if err := install("--restore"); err == nil || strings.Contains(err.Error(), hwSerial) {
		t.Errorf("--restore without a backup: %v", err)
	}
	d.failInstallsLate("Failure [INSTALL_FAILED_INTERNAL_ERROR: the connection went]")
	err := install(fakeAPKFile(t, "app-debug.apk", "a dev build"))
	if err == nil || !strings.Contains(err.Error(), "--serial "+adbSerial) || strings.Contains(err.Error(), hwSerial) {
		t.Errorf("a failed install: %v", err)
	}
	if err := install("--restore"); err == nil || strings.Contains(err.Error(), hwSerial) {
		t.Errorf("a failed restore: %v", err)
	}
	if strings.Contains(log.String(), hwSerial) {
		t.Errorf("the log holds the serial:\n%s", log.String())
	}
	if b := d.backups(); len(b) != 1 || strings.Contains(b[0], hwSerial) {
		t.Errorf("backups = %v, want one, whose path doesn't hold the serial", b)
	}
}
