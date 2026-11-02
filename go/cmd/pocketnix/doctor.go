package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const doctorUsage = `usage: pocketnix doctor [--serial <s>] [--debug] [--json] [--smoke] [--gpu-apps <id>,<id>] [--record <dir>]

Probes a phone for what pocketnix needs (AVF, its kernel, u-boot, the app)
and prints a verdict: ok, warn or fail. It exits 1 only on fail, which means
the phone definitely can't run the VM.

flags:
  --json             print the report as JSON, for a device report
  --debug            look at the debug build (dev.nickolaj.pocketnix.debug)
  --smoke            also run the end-to-end checks against the debug build's
                     running VM (implies --debug; the phone unlocked, pocketnix in front)
  --gpu-apps <ids>   two app ids from pocketnix control dump's apps=, for the GPU step
  --record <dir>     record the run (screen, logcat, commands) into a bundle under dir
  --serial <serial>  adb device to use (default: adb's only device)
`

const (
	// ubootPath mirrors AvfEngine.UBOOT: the bootloader the app boots the image with.
	ubootPath = "/apex/com.android.virt/etc/u-boot.bin"
	vmTool    = "/apex/com.android.virt/bin/vm"
	// avfFeature is the system feature an AVF device declares.
	avfFeature = "android.software.virtualization_framework"
	// grapheneOSPackage is GrapheneOS's App Store, present on every GrapheneOS install.
	grapheneOSPackage = "app.grapheneos.apps"
	// testedKernel is the kernel branch pocketnix has booted on.
	testedKernel = "6.1"
)

type doctorOpts struct {
	serial, record     string
	debug, json, smoke bool
	gpuApps            []string
}

func parseDoctor(args []string) (*doctorOpts, error) {
	o := &doctorOpts{}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, doctorUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.smoke, "smoke", false, "")
	fs.StringVar(&o.record, "record", "", "")
	fs.Func("gpu-apps", "", func(v string) error {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.gpuApps = append(o.gpuApps, id)
			}
		}
		return nil
	})
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return nil, errUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, errUsage
	}
	if o.smoke {
		o.debug = true
	}
	return o, nil
}

func (o *doctorOpts) pkg() string {
	if o.debug {
		return debugPackage
	}
	return releasePackage
}

// runDoctor prints the report, and returns exitStatus(1) on a fail verdict
// so that main adds nothing to it.
func runDoctor(args []string) error {
	o, err := parseDoctor(args)
	if err != nil {
		return err
	}
	if o.smoke || o.record != "" {
		return errors.New("doctor: --smoke and --record are not implemented yet")
	}
	dev := adb.Device{Serial: o.serial}
	if state, err := dev.State(); err != nil || state != "device" {
		return fmt.Errorf("no usable adb device (state %q): %v", state, err)
	}
	r := probe(dev, o.pkg())
	if o.json {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", b)
	} else {
		printDoctor(os.Stdout, r)
	}
	if r.Verdict.Level == levelFail {
		return exitStatus(1)
	}
	return nil
}

// probe reads every fact doctor reports from dev's shell, the app's about
// pkg, and judges them. It reads named props only (B1).
func probe(dev adb.Device, pkg string) doctorReport {
	r := doctorReport{
		Schema: doctorSchema,
		Device: probeDevice(dev),
		AVF:    probeAVF(dev),
		App:    probeApp(dev, pkg),
	}
	r.Verdict = shellVerdict(r.Device, r.AVF)
	return r
}

// shellStatus runs cmdline in dev's shell and returns its stdout and exit
// status; err is only adb's own failure. The marker is adb.Device.Shell's.
func shellStatus(dev adb.Device, cmdline string) (string, int, error) {
	const marker = "pocketnix-exit="
	out, err := dev.Run("shell", cmdline+"; echo "+marker+"$?")
	if err != nil {
		return "", -1, err
	}
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return "", -1, fmt.Errorf("adb shell %s: no exit status in output", cmdline)
	}
	status, err := strconv.Atoi(strings.TrimSpace(out[i+len(marker):]))
	if err != nil {
		return "", -1, fmt.Errorf("adb shell %s: exit status %q", cmdline, out[i+len(marker):])
	}
	return out[:i], status, nil
}

// shellOK is a command's trimmed output, nil unless it exited 0 printing some.
func shellOK(dev adb.Device, cmdline string) *string {
	out, status, err := shellStatus(dev, cmdline)
	if out = strings.TrimSpace(out); err != nil || status != 0 || out == "" {
		return nil
	}
	return &out
}

// getprop is a system property, nil when it reads empty: getprop can't tell
// an unset prop from one it may not read.
func getprop(dev adb.Device, name string) *string { return shellOK(dev, "getprop "+name) }

// propBool reads a boolean as Android's GetBoolProperty does; nil for
// anything else.
func propBool(v *string) *bool {
	if v == nil {
		return nil
	}
	var b bool
	switch *v {
	case "1", "true", "y", "yes", "on":
		b = true
	case "0", "false", "n", "no", "off":
		b = false
	default:
		return nil
	}
	return &b
}

func probeDevice(dev adb.Device) doctorDevice {
	d := doctorDevice{
		Fingerprint:  getprop(dev, "ro.build.fingerprint"),
		Model:        getprop(dev, "ro.product.model"),
		Manufacturer: getprop(dev, "ro.product.manufacturer"),
		SDK:          getprop(dev, "ro.build.version.sdk"),
		Release:      getprop(dev, "ro.build.version.release"),
		Kernel:       shellOK(dev, "uname -r"),
		OS:           osUnknown,
	}
	var soc []string
	for _, p := range []string{"ro.soc.manufacturer", "ro.soc.model"} {
		if v := getprop(dev, p); v != nil {
			soc = append(soc, *v)
		}
	}
	if len(soc) > 0 {
		s := strings.Join(soc, " ")
		d.SoC = &s
	}
	// pm path exits 1, printing nothing, for a package that isn't installed.
	switch out, status, err := shellStatus(dev, "pm path "+grapheneOSPackage); {
	case err != nil:
	case status == 0:
		d.OS = osGrapheneOS
	case status == 1 && strings.TrimSpace(out) == "":
		d.OS = osStock
	}
	return d
}

var apexVersion = regexp.MustCompile(`(?m)^package:com\.(?:google\.)?android\.virt versionCode:(\d+)\s*$`)

func probeAVF(dev adb.Device) doctorAVF {
	a := doctorAVF{
		VMSupported:          propBool(getprop(dev, "ro.boot.hypervisor.vm.supported")),
		ProtectedVMSupported: propBool(getprop(dev, "ro.boot.hypervisor.protected_vm.supported")),
		HypervisorVersion:    getprop(dev, "ro.boot.hypervisor.version"),
		VMInfo:               []string{},
	}
	// pm has-feature prints true or false, exiting 0 or 1.
	if out, status, err := shellStatus(dev, "pm has-feature "+avfFeature); err == nil && (status == 0 || status == 1) {
		a.Feature = propBool(ptr(strings.TrimSpace(out)))
	}
	if out := shellOK(dev, "pm list packages --apex-only --show-versioncode"); out != nil {
		if m := apexVersion.FindStringSubmatch(*out); m != nil {
			a.ApexVersion = &m[1]
		}
	}
	_, status, err := shellStatus(dev, "ls "+ubootPath)
	a.UBoot = err == nil && status == 0
	// vm info exits non-zero after its sysprop lines when it can't reach
	// virtmgr; those lines still count. 126 and 127 are the shell's own.
	if out, status, err := shellStatus(dev, vmTool+" info"); err == nil && status != 126 && status != 127 {
		for _, line := range strings.Split(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				a.VMInfo = append(a.VMInfo, line)
			}
		}
	}
	return a
}

var versionName = regexp.MustCompile(`(?m)^\s*versionName=(\S+)`)

func probeApp(dev adb.Device, pkg string) doctorApp {
	a := doctorApp{Package: pkg}
	if _, status, err := shellStatus(dev, "pm path "+pkg); err != nil || status != 0 {
		return a
	}
	a.Installed = true
	if out := shellOK(dev, "dumpsys package "+pkg); out != nil {
		if m := versionName.FindStringSubmatch(*out); m != nil {
			a.VersionName = &m[1]
		}
	}
	return a
}

func ptr[T any](v T) *T { return &v }

var kernelBranch = regexp.MustCompile(`^(\d+\.\d+)[.-]`)

// shellVerdict judges the shell facts: fail only on a definite "can't run
// the VM", warn on anything uncertain. Each reason starts with its level.
func shellVerdict(d doctorDevice, a doctorAVF) doctorVerdict {
	v := doctorVerdict{Level: levelOK, Reasons: []string{}}
	fail := func(format string, args ...any) {
		v.Level = levelFail
		v.Reasons = append(v.Reasons, "fail: "+fmt.Sprintf(format, args...))
	}
	warn := func(format string, args ...any) {
		if v.Level == levelOK {
			v.Level = levelWarn
		}
		v.Reasons = append(v.Reasons, "warn: "+fmt.Sprintf(format, args...))
	}

	switch {
	case a.Feature == nil:
		warn("couldn't read the %s feature", avfFeature)
	case !*a.Feature:
		fail("the phone lacks the %s feature", avfFeature)
	}

	switch {
	case a.VMSupported == nil:
		warn("ro.boot.hypervisor.vm.supported is empty or unreadable")
	case !*a.VMSupported && a.ProtectedVMSupported != nil && *a.ProtectedVMSupported:
		fail("the hypervisor runs only protected VMs (ro.boot.hypervisor.vm.supported is false), and pocketnix's VM isn't one")
	case !*a.VMSupported:
		warn("ro.boot.hypervisor.vm.supported is false")
	}
	if a.ProtectedVMSupported == nil {
		warn("ro.boot.hypervisor.protected_vm.supported is empty or unreadable")
	}

	reachedVirtmgr := false
	for _, line := range a.VMInfo {
		switch {
		case strings.HasPrefix(line, "VMs are not supported."), strings.HasPrefix(line, "Only protected VMs are supported."):
			fail("vm info: %s", line)
		// vm info prints these only after spawning virtmgr.
		case strings.HasPrefix(line, "Assignable devices:"), strings.HasPrefix(line, "Available OS list:"):
			reachedVirtmgr = true
		}
	}
	switch {
	case len(a.VMInfo) == 0:
		warn("couldn't run %s info", vmTool)
	case !reachedVirtmgr:
		warn("vm info stopped before asking virtmgr: the shell couldn't reach it")
	}

	switch m := kernelBranch.FindStringSubmatch(deref(d.Kernel)); {
	case d.Kernel == nil:
		warn("couldn't read the kernel release")
	case m == nil:
		warn("kernel %s is on an unknown branch; pocketnix is tested on %s", *d.Kernel, testedKernel)
	case m[1] != testedKernel:
		warn("kernel %s is on the %s branch; pocketnix is tested on %s", *d.Kernel, m[1], testedKernel)
	}

	if !a.UBoot {
		warn("no %s: the app has no bootloader for the image", ubootPath)
	}
	return v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// printDoctor writes r for a person; an unreadable fact prints as "?".
func printDoctor(w io.Writer, r doctorReport) {
	str := func(s *string) string {
		if s == nil {
			return "?"
		}
		return *s
	}
	yes := func(b *bool) string {
		switch {
		case b == nil:
			return "?"
		case *b:
			return "yes"
		}
		return "no"
	}
	d, a := r.Device, r.AVF
	fmt.Fprintf(w, "device       %s %s (%s), Android %s (SDK %s), %s\n", str(d.Manufacturer), str(d.Model), str(d.SoC), str(d.Release), str(d.SDK), d.OS)
	fmt.Fprintf(w, "fingerprint  %s\n", str(d.Fingerprint))
	fmt.Fprintf(w, "kernel       %s\n", str(d.Kernel))
	fmt.Fprintf(w, "avf          feature %s, VMs %s, protected VMs %s, hypervisor %s, virt APEX %s, u-boot %s\n",
		yes(a.Feature), yes(a.VMSupported), yes(a.ProtectedVMSupported), str(a.HypervisorVersion), str(a.ApexVersion), yes(&a.UBoot))
	for i, line := range a.VMInfo {
		label := ""
		if i == 0 {
			label = "vm info"
		}
		fmt.Fprintf(w, "%-12s %s\n", label, line)
	}
	app := "not installed"
	if r.App.Installed {
		app = "installed, version " + str(r.App.VersionName)
	}
	fmt.Fprintf(w, "app          %s: %s\n", r.App.Package, app)
	fmt.Fprintf(w, "verdict      %s\n", r.Verdict.Level)
	for _, reason := range r.Verdict.Reasons {
		fmt.Fprintf(w, "  %s\n", reason)
	}
}
