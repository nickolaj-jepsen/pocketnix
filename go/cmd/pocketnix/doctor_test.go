package main

import (
	"encoding/json"
	"strings"
	"testing"
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
// on GrapheneOS.
func cannedPixel(d *fakeDevice) {
	for name, v := range map[string]string{
		"ro.build.fingerprint":                      "google/husky/husky:17/BP4A.251205.006/2025120600:user/release-keys",
		"ro.product.model":                          "Pixel 8 Pro",
		"ro.product.manufacturer":                   "Google",
		"ro.soc.manufacturer":                       "Google",
		"ro.soc.model":                              "Tensor G3",
		"ro.build.version.sdk":                      "37",
		"ro.build.version.release":                  "17",
		"ro.boot.hypervisor.vm.supported":           "1",
		"ro.boot.hypervisor.protected_vm.supported": "1",
		"ro.boot.hypervisor.version":                "pkvm-1.0",
	} {
		d.setProp(name, v)
	}
	d.addPackages("app.grapheneos.apps")
	d.addFile("/apex/com.android.virt/etc/u-boot.bin", []byte("u-boot"))
	d.can("pm has-feature android.software.virtualization_framework", "true\n", 0)
	d.can("uname -r", "6.1.145-android14-11-g0c1d2e3f4a5b\n", 0)
	d.can("/apex/com.android.virt/bin/vm info", `Both protected and non-protected VMs are supported.
Hypervisor version: pkvm-1.0
/dev/kvm exists.
/dev/vfio/vfio does not exist.
VFIO-platform is not supported.
Assignable devices: []
Available OS list: ["microdroid"]
Debug policy: none
`, 0)
	d.can("pm list packages --apex-only --show-versioncode", "package:com.android.virt versionCode:360000000\n", 0)
}

func TestDoctorJSON(t *testing.T) {
	t.Skip("pending: A3a")
	d := newFakeDevice(t)
	cannedPixel(d)
	r, raw, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"schema": float64(1), "device.os": "grapheneos", "avf.vmSupported": true,
		"avf.protectedVmSupported": true, "verdict.level": "ok",
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

func TestDoctorCaps(t *testing.T) {
	t.Skip("pending: A3b")
	d := newFakeDevice(t)
	cannedPixel(d)
	d.installAPK([]byte("apk"))
	d.setCaps("capabilities=1, manager=true, gpuConfig=boxed")
	r, _, err := doctorJSON(t)
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
	r, _, err = doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.App.CapsError == "" || r.Verdict.Level != levelWarn {
		t.Errorf("an app without caps: capsError %q, verdict %+v", r.App.CapsError, r.Verdict)
	}
}
