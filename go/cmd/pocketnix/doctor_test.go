package main

import (
	"encoding/json"
	"errors"
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
			d.can("/apex/com.android.virt/bin/vm info", "Only protected VMs are supported.\nHypervisor version: pkvm-1.0\n", 0)
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
Hypervisor version: pkvm-1.0
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
