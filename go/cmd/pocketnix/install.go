package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	// maxPull bounds the backup's one adb pull: bulk USB transfers drop the device after ~100–200 MB.
	maxPull = 64 << 20
	// stopDeadline outlasts Vm.GRACEFUL_STOP_MS plus AvfEngine.forceStop's 10 s wait for
	// onStopped, after which the app reports the VM stopped either way.
	stopDeadline = 60 * time.Second
	// startDeadline bounds the wait for a START to reach the app; status counts Starting as running.
	startDeadline = 30 * time.Second
	statusPoll    = 500 * time.Millisecond
	// backupKeyBytes is how much of a hash names a phone's backup directory.
	backupKeyBytes = 8
)

const (
	// vmProcessesCommand lists every process's uid and name; shell may see them all.
	vmProcessesCommand = "ps -A -o UID=,NAME="
	// vmProcessPrefix starts the VM's process's name, crosvm_ and AvfEngine.VM_NAME
	// on a Pixel: virtmgr chooses it, so anything after this may differ elsewhere.
	vmProcessPrefix = "crosvm"
)

// uninstallCost follows any mention of an uninstall: neither provision nor install ever does one.
const uninstallCost = "only an uninstall gets past it, and an uninstall deletes the VM's disk image and everything in the guest, for good. pocketnix won't do that: do it by hand only if you mean to lose them"

// signatureHint follows a failed `adb install -r`, whatever failed it.
const signatureHint = "If that's a signature mismatch, " + uninstallCost

// What a package's entry in `dumpsys package` holds: its long version code, and
// its flags, among them DEBUGGABLE.
var (
	dumpsysVersionCode = regexp.MustCompile(`(?m)^\s*versionCode=(\d+)`)
	dumpsysFlags       = regexp.MustCompile(`(?m)^\s*(?:pkgF|f)lags=\[([^\]]*)\]`)
)

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

// backupPath is where the APK that an install replaced on dev's pkg is kept,
// outliving the run. It goes by a hash of the phone's ro.serialno: adb's serial
// changes between USB and Wi-Fi, and between Wi-Fi sessions, which would strand a
// backup, and the path turns up in logs and adb's errors.
func (o *installOpts) backupPath(dev adb.Device) (string, error) {
	out, err := dev.Shell("getprop ro.serialno")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" || id == "unknown" {
		o.logf("the phone doesn't show its ro.serialno, so its backup goes by adb's serial, which changes with the connection")
		if id = o.serial; id == "" {
			out, err := dev.Run("get-serialno")
			if err != nil {
				return "", err
			}
			id = strings.TrimSpace(out)
		}
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(dir, "pocketnix", "backup", hex.EncodeToString(sum[:backupKeyBytes]), o.pkg()+".apk"), nil
}

// swap is what a run knows before it installs: what a failure has to say and undo.
type swap struct {
	// backup is this phone's backup path; newBackup says this run wrote it.
	backup    string
	newBackup bool
	// pmPath is pm path's answer about the build to replace, empty without one.
	pmPath     string
	wasRunning bool
}

// run never uninstalls the app: that deletes the VM's disk. An install that
// fails having replaced nothing puts things back (installFailed); one that
// replaced the app, or may have, leaves the VM stopped and keeps the backup.
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
	backup, err := o.backupPath(dev)
	if err != nil {
		return err
	}
	apk := o.apk
	if o.restore {
		if _, err := os.Stat(backup); err != nil {
			return fmt.Errorf("no backup of %s for this phone at %s", o.pkg(), backup)
		}
		apk = backup
	}

	prov := provider{dev: dev, authority: o.pkg() + ".provision"}
	s := swap{backup: backup}
	// pm exits 1 for a package that isn't installed; anything else could hide a running VM.
	out, status, err := dev.ShellStatus("pm path " + o.pkg())
	if err == nil && status > 1 {
		err = fmt.Errorf("pm path exited %d", status)
	}
	if err != nil {
		return fmt.Errorf("can't tell whether %s is installed, so not installing: %w", o.pkg(), err)
	}
	if status == 0 {
		s.pmPath = out
		st, err := prov.call("status", "")
		switch {
		case err == nil:
			s.wasRunning = st["running"] == "true"
		case o.restore:
			// The build being replaced may be why it doesn't answer, so that can't block the way back.
			if err := o.checkNoVM(dev); err != nil {
				return err
			}
			o.logf("the installed build doesn't answer and no VM runs for it, so going ahead")
		default:
			return fmt.Errorf("can't tell whether the VM is running, so not installing: %w", err)
		}
		// Before the backup and the stop: a refusal changes nothing.
		osDebuggable := propBool(getprop(dev, "ro.debuggable"))
		incoming, _ := readAPKManifest(apk)
		installedCode, installedDebuggable := installedBuild(dev, o.pkg())
		if downgradeRefused(incoming.versionCode, installedCode, installedDebuggable, osDebuggable) {
			return o.refuseDowngrade(*incoming.versionCode, *installedCode, backup)
		}
		// Before the stop: a refused or failed pull leaves the VM as it was.
		if !o.restore {
			if s.newBackup, err = o.backUp(dev, out, backup); err != nil {
				return err
			}
			if kept, err := readAPKManifest(backup); err == nil && downgradeRefused(kept.versionCode, incoming.versionCode, incoming.debuggable, osDebuggable) {
				o.logf("warning: after this install --restore can't go back: Android won't take the backup's versionCode %d over this build's %d, which isn't debuggable, on this phone", *kept.versionCode, *incoming.versionCode)
			}
		}
	}
	if s.wasRunning {
		if err := o.stopVM(dev, prov); err != nil {
			return err
		}
	}

	o.logf("installing %s", apk)
	// -d: the backup, or an older dev build, may have a lower versionCode than the build it replaces.
	if _, err := dev.Run("install", "-r", "-d", apk); err != nil {
		return o.installFailed(dev, prov, fmt.Errorf("%w\n%s", err, signatureHint), s)
	}
	for _, p := range avfPermissions {
		if _, err := dev.Shell("pm grant " + o.pkg() + " " + p); err != nil {
			return o.leftChanged(err, s, "The build was replaced, but not all its permissions are granted.")
		}
	}
	o.logf("granted %s", strings.Join(avfPermissions, ", "))
	if o.restore {
		if err := removeBackup(backup); err != nil {
			return err
		}
		o.logf("restored the backup and removed it")
	}

	if o.noStart || !(s.wasRunning || o.restore) {
		return nil
	}
	return o.startVM(dev, prov)
}

// installedBuild reads pkg's version code and debuggable flag from dumpsys
// package, nil where it doesn't show one: only its entry for pkg counts.
func installedBuild(dev adb.Device, pkg string) (code *int64, debuggable *bool) {
	out := shellOK(dev, "dumpsys package "+pkg)
	if out == nil {
		return nil, nil
	}
	_, entry, ok := strings.Cut(*out, "Package ["+pkg+"]")
	if !ok {
		return nil, nil
	}
	if m := dumpsysVersionCode.FindStringSubmatch(entry); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			code = &n
		}
	}
	if m := dumpsysFlags.FindStringSubmatch(entry); m != nil {
		debuggable = new(contains(strings.Fields(m[1]), "DEBUGGABLE"))
	}
	return code, debuggable
}

// downgradeRefused reports whether Android definitely refuses an install of
// version incoming with -d over a build at installed: a lower version passes
// only over a debuggable build or on a debuggable OS (ro.debuggable). A nil,
// which is something unreadable, makes it false: adb decides then.
func downgradeRefused(incoming, installed *int64, installedDebuggable, osDebuggable *bool) bool {
	return incoming != nil && installed != nil && installedDebuggable != nil && osDebuggable != nil &&
		*incoming < *installed && !*installedDebuggable && !*osDebuggable
}

// refuseDowngrade is the error for a run downgradeRefused stops.
func (o *installOpts) refuseDowngrade(incoming, installed int64, backup string) error {
	what, kept := "that APK", ""
	if o.restore {
		what, kept = "the backup", ", and the backup stays at "+backup
	}
	return fmt.Errorf("%s has versionCode %d, the installed build %d, and Android refuses to downgrade a build that isn't debuggable on a phone that isn't either. Nothing changed%s. As it stands, %s", what, incoming, installed, kept, uninstallCost)
}

// checkNoVM errors if a VM process runs for the app, or adb can't show the
// processes. With none, an install has no VM to kill.
func (o *installOpts) checkNoVM(dev adb.Device) error {
	out, status, err := dev.ShellStatus(vmProcessesCommand)
	if err == nil && (status != 0 || strings.TrimSpace(out) == "") {
		err = fmt.Errorf("%s exited %d, printing %q", vmProcessesCommand, status, out)
	}
	if err != nil {
		return fmt.Errorf("can't tell whether a VM is running, so not restoring: %w", err)
	}
	// Without the app's uid, any VM on the phone might be its own.
	var uids []string
	if uid, err := appUID(dev, o.pkg()); err == nil {
		uids = strings.Split(uid, ",")
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[1], vmProcessPrefix) && (uids == nil || contains(uids, f[0])) {
			return fmt.Errorf("the installed build doesn't answer, yet a VM process (%s) runs for it, so not restoring: that would kill the VM. Shut the guest down, then run this again", f[1])
		}
	}
	return nil
}

// removeBackup deletes a backup, and the device's directory once it holds no
// other package's.
func removeBackup(backup string) error {
	if err := os.Remove(backup); err != nil {
		return err
	}
	os.Remove(filepath.Dir(backup))
	return nil
}

// installFailed is the error for an adb install that failed, having first put
// things back if it replaced nothing: pm path changes with every install, so an
// unchanged one means the old build is still there, and the stop was all this
// run did to it. Otherwise it's leftChanged's.
func (o *installOpts) installFailed(dev adb.Device, prov provider, err error, s swap) error {
	after, status, perr := dev.ShellStatus("pm path " + o.pkg())
	if perr != nil || status > 1 {
		return o.leftChanged(err, s, "The phone doesn't say whether the build changed.")
	}
	if status != 0 {
		after = ""
	}
	if strings.TrimSpace(after) != strings.TrimSpace(s.pmPath) {
		return o.leftChanged(err, s, "The build was replaced.")
	}
	if s.pmPath == "" {
		return fmt.Errorf("%w\nNothing was installed.", err)
	}
	if s.newBackup {
		if rerr := removeBackup(s.backup); rerr != nil {
			o.logf("couldn't remove this run's backup: %v", rerr)
		}
	}
	const untouched = "Nothing was replaced: the installed build is untouched"
	switch {
	case !s.wasRunning:
		return fmt.Errorf("%w\n%s.", err, untouched)
	case o.noStart:
		return fmt.Errorf("%w\n%s, and its VM stays stopped, as --no-start asks.", err, untouched)
	}
	if serr := o.startVM(dev, prov); serr != nil {
		return fmt.Errorf("%w\n%s, but its VM was stopped and didn't start again: %v", err, untouched, serr)
	}
	return fmt.Errorf("%w\n%s, and its VM is running again.", err, untouched)
}

// leftChanged is the error for a run that leaves things as the failure did: the
// VM down if it ran, and the backup, the way back. what says what's known of the build.
func (o *installOpts) leftChanged(err error, s swap, what string) error {
	vm := "The VM wasn't running"
	if s.wasRunning {
		vm = "The VM is stopped"
	}
	switch _, serr := os.Stat(s.backup); {
	case serr != nil:
		return fmt.Errorf("%w\n%s %s.", err, what, vm)
	case o.restore:
		return fmt.Errorf("%w\n%s %s, and the backup is kept: `%s` tries again and starts the VM.", err, what, vm, o.restoreCommand())
	}
	return fmt.Errorf("%w\n%s %s, and the build the phone had is backed up: `%s` puts it back and starts the VM.", err, what, vm, o.restoreCommand())
}

// backUp pulls the installed APK, which pmPath (pm path's answer) names, to
// backup unless a backup exists: the first one is the owner's build. It says
// whether it wrote one.
func (o *installOpts) backUp(dev adb.Device, pmPath, backup string) (bool, error) {
	if _, err := os.Stat(backup); err == nil {
		o.logf("keeping the earlier backup %s", backup)
		return false, nil
	}
	var paths []string
	for _, line := range strings.Split(pmPath, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok {
			paths = append(paths, p)
		}
	}
	switch len(paths) {
	case 0:
		return false, fmt.Errorf("pm path %s named no APK:\n%s", o.pkg(), pmPath)
	case 1:
	default:
		return false, fmt.Errorf("%s is a split install (%d APKs), and install backs up only one; not installing", o.pkg(), len(paths))
	}
	remote := paths[0]
	out, err := dev.Shell("stat -c %s " + adb.ShellQuote(remote))
	if err != nil {
		return false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return false, fmt.Errorf("size of %s: %q", remote, out)
	}
	if size > maxPull {
		return false, fmt.Errorf("the installed APK is %d MiB, more than one adb pull can be trusted with (maxPull, %d MiB); not backing it up or installing", size>>20, maxPull>>20)
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return false, err
	}
	part := backup + ".part"
	defer os.Remove(part)
	if _, err := dev.Run("pull", remote, part); err != nil {
		return false, err
	}
	if fi, err := os.Stat(part); err != nil || fi.Size() != size {
		return false, fmt.Errorf("pulled %s incompletely: want %d bytes", remote, size)
	}
	if err := os.Rename(part, backup); err != nil {
		return false, err
	}
	o.logf("backed up the installed build to %s", backup)
	return true, nil
}

// stopVM asks the app to stop the VM and waits until status says it has:
// the STOP intent returns before the guest is down.
func (o *installOpts) stopVM(dev adb.Device, prov provider) error {
	o.logf("stopping the VM")
	if _, err := amStart(dev, o.pkg(), actionStop); err != nil {
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
	if _, err := amStart(dev, o.pkg(), actionStart); err != nil {
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
