package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testImage is larger than the fake's push limit and mostly zeros.
func testImage(t *testing.T) (string, []byte) {
	t.Helper()
	img := make([]byte, 3*chunkSize+4096)
	copy(img, "header")
	copy(img[2*chunkSize+100:], "data in the third chunk")
	copy(img[len(img)-4:], "tail")
	p := filepath.Join(t.TempDir(), "disk.raw")
	os.WriteFile(p, img, 0o644)
	return p, img
}

func provisionFor(t *testing.T, args ...string) error {
	t.Helper()
	o, err := parseProvision(args)
	if err != nil {
		return err
	}
	o.log = io.Discard
	o.downloadRelease = func(string, string) error { return fmt.Errorf("no network in tests") }
	return o.run()
}

// provision shares install's word on what getting past a signature mismatch costs.
func TestProvisionFailedInstallSaysWhatAnUninstallCosts(t *testing.T) {
	d := newFakeDevice(t)
	d.failInstalls("Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: signatures do not match]")
	img, _ := testImage(t)
	err := provisionFor(t, "--image", img, "--apk", fakeAPKFile(t, "app-debug.apk", "apk"), "--debug")
	if err == nil || !strings.Contains(err.Error(), uninstallCost) {
		t.Errorf("got %v, want it to say what an uninstall costs", err)
	}
}

func TestProvision(t *testing.T) {
	d := newFakeDevice(t)
	img, want := testImage(t)
	apk := fakeAPKFile(t, "app-debug.apk", "apk")

	err := provisionFor(t, "--image", img, "--apk", apk, "--debug", "--no-network", "--memory", "1024", "--cpus", "one")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.disk(), want) {
		t.Error("the phone's disk image differs from the pushed one")
	}
	ev := d.events()
	for _, line := range []string{
		"install -r -g app-debug.apk",
		"grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE",
		"grant dev.nickolaj.pocketnix.debug android.permission.USE_CUSTOM_VIRTUAL_MACHINE",
	} {
		if !strings.Contains(ev, line+"\n") {
			t.Errorf("device never saw %q; events:\n%s", line, ev)
		}
	}
	extras := map[string]string{}
	for _, line := range strings.Split(ev, "\n") {
		rest, ok := strings.CutPrefix(line, "am start -n dev.nickolaj.pocketnix.debug/dev.nickolaj.pocketnix.ControlActivity -a dev.nickolaj.pocketnix.action.START ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f)%3 != 0 {
			t.Fatalf("start extras aren't flag, name, value: %q", rest)
		}
		for i := 0; i < len(f); i += 3 {
			extras[f[i+1]] = f[i] + " " + f[i+2]
		}
	}
	for name, want := range map[string]string{
		"network": "--ez false", "memoryMb": "--ei 1024", "cpus": "--ei 1",
	} {
		if extras[name] != want {
			t.Errorf("start extra %s = %q, want %q; events:\n%s", name, extras[name], want, ev)
		}
	}
	if _, err := os.Stat(filepath.Join(d.state, "device", deviceChunk)); err == nil {
		t.Error("chunk left behind on the device")
	}
}

func TestProvisionKeepsExistingDisk(t *testing.T) {
	d := newFakeDevice(t)
	img, want := testImage(t)
	apk := fakeAPKFile(t, "app.apk", "apk")
	if err := provisionFor(t, "--image", img, "--apk", apk, "--no-start"); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "other.raw")
	os.WriteFile(other, []byte("a different image"), 0o644)
	before := len(d.events())
	if err := provisionFor(t, "--image", other, "--apk", apk, "--no-start"); err == nil {
		t.Error("provision replaced an existing disk without --replace")
	}
	if !bytes.Equal(d.disk(), want) {
		t.Error("the existing disk changed")
	}
	refused := d.events()[before:]
	t.Run("refused before installing", func(t *testing.T) {
		if hasEvent(refused, "install ") {
			t.Errorf("the refused run installed the app; its events:\n%s", refused)
		}
	})
	if err := provisionFor(t, "--image", other, "--apk", apk, "--no-start", "--replace"); err != nil {
		t.Fatal(err)
	}
	if string(d.disk()) != "a different image" {
		t.Error("--replace didn't replace the disk")
	}
}

func TestProvisionRefusesWhileRunning(t *testing.T) {
	d := newFakeDevice(t)
	t.Setenv("FAKE_ADB_RUNNING", "1")
	img, _ := testImage(t)
	apk := fakeAPKFile(t, "app.apk", "apk")
	if err := provisionFor(t, "--image", img, "--apk", apk, "--replace"); err == nil {
		t.Error("provision pushed while the VM runs")
	}
	if d.disk() != nil {
		t.Error("disk written while the VM runs")
	}
	// Installing would have killed it.
	if strings.Contains(d.events(), "install ") {
		t.Errorf("installed the app over a running VM; events:\n%s", d.events())
	}
}

// The phone's refusals come before building an image, so a refused run
// costs no build.
func TestProvisionChecksThePhoneFirst(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		setup       func(d *fakeDevice)
	}{
		{"running", "running", func(d *fakeDevice) { d.setRunning(true) }},
		{"disk", "--replace", func(d *fakeDevice) { d.write("app/files/disk.img", []byte("the guest")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.installAPK([]byte("apk"))
			tc.setup(d)
			o, err := parseProvision([]string{"--image", ".#nixosConfigurations.phone", "--apk", fakeAPKFile(t, "app.apk", "apk")})
			if err != nil {
				t.Fatal(err)
			}
			o.log = io.Discard
			o.resolveInstallable = func(string) (string, error) { return "", fmt.Errorf("built the image") }
			if err := o.run(); err == nil || !strings.Contains(err.Error(), tc.token) {
				t.Errorf("got %v, want a refusal naming %s", err, tc.token)
			}
			if hasEvent(d.events(), "install ") {
				t.Errorf("installed; events:\n%s", d.events())
			}
		})
	}
}

func TestProvisionRefusesTheOtherPackage(t *testing.T) {
	d := newFakeDevice(t)
	d.installAPKAs(debugPackage, []byte("the owner's build"))
	d.setRunning(true)
	img, _ := testImage(t)
	err := provisionFor(t, "--image", img, "--apk", fakeAPKFile(t, "app-debug.apk", "a dev build"), "--replace")
	if err == nil || !strings.Contains(err.Error(), "--debug") {
		t.Errorf("got %v, want a refusal naming --debug", err)
	}
	if ev := d.events(); hasEvent(ev, "install ") || hasEvent(ev, "vm killed") {
		t.Errorf("acted on the device; events:\n%s", ev)
	}
}

// A device that definitely can't run a VM stops provision before the
// install, unless --force.
func TestProvisionRefusesFailingDevice(t *testing.T) {
	d := newFakeDevice(t)
	d.can("pm has-feature android.software.virtualization_framework", "false\n", 1)
	img, _ := testImage(t)
	apk := fakeAPKFile(t, "app.apk", "apk")
	if err := provisionFor(t, "--image", img, "--apk", apk, "--no-start"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("got %v, want a refusal naming --force", err)
	}
	if hasEvent(d.events(), "install ") {
		t.Errorf("installed onto a failing device; events:\n%s", d.events())
	}
	if err := provisionFor(t, "--image", img, "--apk", apk, "--no-start", "--force"); err != nil {
		t.Errorf("--force: %v", err)
	}
	if d.disk() == nil {
		t.Error("--force didn't push the image")
	}
}

func TestProvisionNeedsAnAPKWithoutARelease(t *testing.T) {
	newFakeDevice(t)
	img, _ := testImage(t)
	if err := provisionFor(t, "--image", img); err == nil || !strings.Contains(err.Error(), "--apk") {
		t.Errorf("got %v, want a hint to pass --apk", err)
	}
	// A release is never the debug app.
	if err := provisionFor(t, "--image", img, "--debug"); err == nil || !strings.Contains(err.Error(), "--apk") {
		t.Errorf("--debug without --apk: %v", err)
	}
}

// A failed push leaves nothing behind, here or on the phone.
func TestProvisionCleansUpAfterAFailure(t *testing.T) {
	d := newFakeDevice(t)
	img, _ := testImage(t)
	apk := fakeAPKFile(t, "app.apk", "apk")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("FAKE_ADB_FULL", "1")
	if err := provisionFor(t, "--image", img, "--apk", apk); err == nil {
		t.Fatal("provision succeeded onto a full phone")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in TMPDIR: %v", left)
	}
	if _, err := os.Stat(filepath.Join(d.state, "device", deviceChunk)); err == nil {
		t.Error("chunk left behind on the device")
	}
}

func TestParseProvisionModes(t *testing.T) {
	o, err := parseProvision([]string{"--image", "x", "--inject", "/a=f", "--mode", "0644", "--inject", "/b=-"})
	if err != nil {
		t.Fatal(err)
	}
	if o.injects[0].Mode != 0o644 || o.injects[1].Mode != 0o600 {
		t.Errorf("modes = %o, %o", o.injects[0].Mode, o.injects[1].Mode)
	}
	for _, args := range [][]string{
		{"--image", "x", "--mode", "0644"},
		{"--image", "x", "--inject", "/a"},
		{"--image", "x", "--inject", "/a=-", "--inject", "/b=-"},
	} {
		if _, err := parseProvision(args); err == nil {
			t.Errorf("parseProvision(%q) accepted", args)
		}
	}
}
