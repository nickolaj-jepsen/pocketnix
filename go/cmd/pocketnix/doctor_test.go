package main

import (
	"encoding/json"
	"testing"
)

// doctorJSON runs doctor --json with args and decodes its report.
func doctorJSON(t *testing.T, args ...string) (doctorReport, error) {
	t.Helper()
	out, err := stdoutOf(t, func() error { return runDoctor(append([]string{"--json"}, args...)) })
	var r doctorReport
	if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
		t.Fatalf("doctor --json printed no report (%v; run: %v):\n%s", jerr, err, out)
	}
	return r, err
}

// cannedPixel makes the fake device answer like the maintainer's Pixel 8 Pro
// on GrapheneOS.
func cannedPixel(d *fakeDevice) {
	for name, v := range map[string]string{
		"ro.product.model":                          "Pixel 8 Pro",
		"ro.product.manufacturer":                   "Google",
		"ro.build.version.sdk":                      "37",
		"ro.build.version.release":                  "17",
		"ro.boot.hypervisor.vm.supported":           "1",
		"ro.boot.hypervisor.protected_vm.supported": "1",
	} {
		d.setProp(name, v)
	}
	d.addPackages("app.grapheneos.apps")
	d.can("pm has-feature android.software.virtualization_framework", "true\n", 0)
	d.can("uname -r", "6.1.145-android14-11-g0c1d2e3f4a5b\n", 0)
	d.can("ls /apex/com.android.virt/etc/u-boot.bin", "/apex/com.android.virt/etc/u-boot.bin\n", 0)
}

func TestDoctorJSON(t *testing.T) {
	t.Skip("pending: A3a")
	d := newFakeDevice(t)
	cannedPixel(d)
	r, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
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

func TestDoctorCaps(t *testing.T) {
	t.Skip("pending: A3b")
	d := newFakeDevice(t)
	cannedPixel(d)
	d.installAPK([]byte("apk"))
	d.setCaps("capabilities=1, manager=true, gpuConfig=boxed")
	r, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.App.Caps["gpuConfig"] != "boxed" || r.App.Caps["capabilities"] != "1" {
		t.Errorf("app.caps = %v", r.App.Caps)
	}

	// An app from before caps.
	d = newFakeDevice(t)
	cannedPixel(d)
	d.installAPK([]byte("apk"))
	r, err = doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.App.CapsError == "" || r.Verdict.Level != levelWarn {
		t.Errorf("an app without caps: capsError %q, verdict %+v", r.App.CapsError, r.Verdict)
	}
}
