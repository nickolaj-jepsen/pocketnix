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

func TestInstallSwapsRunningVM(t *testing.T) {
	t.Skip("pending: A1")
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
	if b := d.backups(); len(b) != 1 {
		t.Errorf("backups = %v, want one", b)
	}
}

func TestInstallRestore(t *testing.T) {
	t.Skip("pending: A1")
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
	if !d.running() {
		t.Error("the VM isn't running after --restore")
	}
}
