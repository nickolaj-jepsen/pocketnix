package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const installUsage = `usage: pocketnix install [--serial <s>] [--debug] [--no-start] <apk>
       pocketnix install [--serial <s>] [--debug] [--no-start] --restore

Swaps the app's build and never touches the VM's disk: stops a running VM,
backs up the installed APK unless a backup exists, installs the new one over
it, re-grants the AVF permissions and starts the VM again if it was running.
Later installs keep the first backup, so --restore returns the phone to the
build it had before the first swap.

flags:
  --restore          reinstall the backed-up APK and delete the backup; starts the VM
  --debug            target the debug build (dev.nickolaj.pocketnix.debug)
  --no-start         leave the VM stopped afterwards
  --serial <serial>  adb device to use (default: adb's only device)
`

const (
	// actionStop mirrors ControlActivity.ACTION_STOP.
	actionStop = "dev.nickolaj.pocketnix.action.STOP"
	// maxPull bounds the backup's one adb pull: bulk USB transfers drop the device after ~100–200 MB.
	maxPull = 64 << 20
	// stopDeadline outlasts Vm.GRACEFUL_STOP_MS plus AvfEngine.forceStop's 10 s wait for
	// onStopped, after which the app reports the VM stopped either way.
	stopDeadline = 60 * time.Second
	// startDeadline bounds the wait for a START to reach the app; status counts Starting as running.
	startDeadline = 30 * time.Second
	statusPoll    = 500 * time.Millisecond
)

// signatureHint follows a failed `adb install -r`: neither provision nor install ever uninstalls.
const signatureHint = "A signature mismatch needs an uninstall first, which deletes the VM's disk image; do that by hand if you mean it"

type installOpts struct {
	apk, serial             string
	debug, noStart, restore bool
	log                     io.Writer
}

func parseInstall(args []string) (*installOpts, error) {
	o := &installOpts{log: os.Stderr}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, installUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.noStart, "no-start", false, "")
	fs.BoolVar(&o.restore, "restore", false, "")
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return nil, errUsage
	}
	switch {
	case o.restore && fs.NArg() == 0:
	case !o.restore && fs.NArg() == 1:
		o.apk = fs.Arg(0)
	default:
		fs.Usage()
		return nil, errUsage
	}
	return o, nil
}

func runInstall(args []string) error {
	o, err := parseInstall(args)
	if err != nil {
		return err
	}
	return o.run()
}

func (o *installOpts) pkg() string {
	if o.debug {
		return debugPackage
	}
	return releasePackage
}

func (o *installOpts) logf(format string, args ...any) {
	fmt.Fprintf(o.log, "install: "+format+"\n", args...)
}

// restoreCommand is the command line that puts the backup back.
func (o *installOpts) restoreCommand() string {
	cmd := "pocketnix install"
	if o.serial != "" {
		cmd += " --serial " + o.serial
	}
	if o.debug {
		cmd += " --debug"
	}
	return cmd + " --restore"
}

// backupPath is where the APK that an install replaced on serial's pkg is
// kept, outliving the run.
func backupPath(serial, pkg string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	serial = strings.ReplaceAll(serial, string(os.PathSeparator), "_")
	return filepath.Join(dir, "pocketnix", "backup", serial, pkg+".apk"), nil
}

// run never uninstalls the app: that deletes the VM's disk. A failure after
// the VM was stopped leaves it stopped and keeps the backup.
func (o *installOpts) run() error {
	if !o.restore {
		if err := checkAPKPackage(o.apk, o.pkg()); err != nil {
			return err
		}
	}
	dev := adb.Device{Serial: o.serial}
	if state, err := dev.State(); err != nil || state != "device" {
		return fmt.Errorf("no usable adb device (state %q): %v", state, err)
	}
	serial := o.serial
	if serial == "" {
		out, err := dev.Run("get-serialno")
		if err != nil {
			return err
		}
		serial = strings.TrimSpace(out)
	}
	backup, err := backupPath(serial, o.pkg())
	if err != nil {
		return err
	}
	apk := o.apk
	if o.restore {
		if _, err := os.Stat(backup); err != nil {
			return fmt.Errorf("no backup of %s for %s at %s", o.pkg(), serial, backup)
		}
		apk = backup
	}

	prov := provider{dev: dev, authority: o.pkg() + ".provision"}
	wasRunning := false
	// pm exits 1 for a package that isn't installed; anything else could hide a running VM.
	out, status, err := shellStatus(dev, "pm path "+o.pkg())
	if err == nil && status > 1 {
		err = fmt.Errorf("pm path exited %d", status)
	}
	if err != nil {
		return fmt.Errorf("can't tell whether %s is installed, so not installing: %w", o.pkg(), err)
	}
	if status == 0 {
		// Before the stop: a refused or failed pull leaves the VM as it was.
		if !o.restore {
			if err := o.backUp(dev, out, backup); err != nil {
				return err
			}
		}
		st, err := prov.call("status", "")
		if err != nil {
			return fmt.Errorf("can't tell whether the VM is running, so not installing: %w", err)
		}
		wasRunning = st["running"] == "true"
	}
	if wasRunning {
		if err := o.stopVM(dev, prov); err != nil {
			return err
		}
	}

	fail := func(err error) error {
		if _, serr := os.Stat(backup); serr == nil {
			return fmt.Errorf("%w\nThe VM is stopped, and the replaced build is backed up: `%s` puts it back and starts the VM", err, o.restoreCommand())
		}
		return err
	}
	o.logf("installing %s", apk)
	// -d: the backup, or an older dev build, may have a lower versionCode than the build it replaces.
	if _, err := dev.Run("install", "-r", "-d", apk); err != nil {
		return fail(fmt.Errorf("%w\n%s", err, signatureHint))
	}
	for _, p := range avfPermissions {
		if _, err := dev.Shell("pm grant " + o.pkg() + " " + p); err != nil {
			return fail(err)
		}
	}
	o.logf("granted %s", strings.Join(avfPermissions, ", "))
	if o.restore {
		if err := os.Remove(backup); err != nil {
			return err
		}
		// The device's directory goes too, once it holds no other package's backup.
		os.Remove(filepath.Dir(backup))
		o.logf("restored the backup and removed it")
	}

	if o.noStart || !(wasRunning || o.restore) {
		return nil
	}
	return o.startVM(dev, prov)
}

// backUp pulls the installed APK, which pmPath (pm path's answer) names, to
// backup unless a backup exists: the first one is the owner's build.
func (o *installOpts) backUp(dev adb.Device, pmPath, backup string) error {
	if _, err := os.Stat(backup); err == nil {
		o.logf("keeping the earlier backup %s", backup)
		return nil
	}
	var paths []string
	for _, line := range strings.Split(pmPath, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok {
			paths = append(paths, p)
		}
	}
	switch len(paths) {
	case 0:
		return fmt.Errorf("pm path %s named no APK:\n%s", o.pkg(), pmPath)
	case 1:
	default:
		return fmt.Errorf("%s is a split install (%d APKs), and install backs up only one; not installing", o.pkg(), len(paths))
	}
	remote := paths[0]
	out, err := dev.Shell("stat -c %s " + adb.ShellQuote(remote))
	if err != nil {
		return err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return fmt.Errorf("size of %s: %q", remote, out)
	}
	if size > maxPull {
		return fmt.Errorf("the installed APK is %d MiB, more than one adb pull can be trusted with (maxPull, %d MiB); not backing it up or installing", size>>20, maxPull>>20)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	part := backup + ".part"
	defer os.Remove(part)
	if _, err := dev.Run("pull", remote, part); err != nil {
		return err
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != size {
		return fmt.Errorf("pulled %s incompletely: want %d bytes", remote, size)
	}
	if err := os.Rename(part, backup); err != nil {
		return err
	}
	o.logf("backed up the installed build to %s", backup)
	return nil
}

// stopVM asks the app to stop the VM and waits until status says it has:
// the STOP intent returns before the guest is down.
func (o *installOpts) stopVM(dev adb.Device, prov provider) error {
	o.logf("stopping the VM")
	if _, err := dev.Shell(fmt.Sprintf("am start -n %s/%s -a %s", o.pkg(), activityClass, actionStop)); err != nil {
		return err
	}
	if err := waitRunning(prov, false, stopDeadline); err != nil {
		return fmt.Errorf("the VM didn't stop, so not installing (that would kill it): %w", err)
	}
	o.logf("the VM is stopped")
	return nil
}

// startVM sends a bare START: its extras would overwrite the saved
// settings, which survive the install.
func (o *installOpts) startVM(dev adb.Device, prov provider) error {
	if _, err := dev.Shell(fmt.Sprintf("am start -n %s/%s -a %s", o.pkg(), activityClass, actionStart)); err != nil {
		return err
	}
	if err := waitRunning(prov, true, startDeadline); err != nil {
		return fmt.Errorf("the VM didn't start (pocketnix control logcat may say why): %w", err)
	}
	o.logf("started the VM")
	return nil
}

// waitRunning polls status until its running is want, or deadline passes.
func waitRunning(prov provider, want bool, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for {
		st, err := prov.call("status", "")
		if err == nil && (st["running"] == "true") == want {
			return nil
		}
		if time.Now().After(end) {
			if err == nil {
				err = fmt.Errorf("status still says running=%t after %s", !want, deadline)
			}
			return err
		}
		time.Sleep(statusPoll)
	}
}
