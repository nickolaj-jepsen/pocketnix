package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// The test binary doubles as a fake adb: tests put a symlink named adb to it
// first on PATH, and it simulates a device plus the app's providers in the
// directory $FAKE_ADB_STATE.
func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "adb":
		os.Exit(fakeADB(os.Args[1:]))
	case "scrcpy":
		os.Exit(fakeScrcpy(os.Args[1:]))
	case fakeDoctorName:
		os.Exit(fakeDoctorProcess(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeChunkCap models the USB drop: one push or pull may not exceed it.
const fakeChunkCap = 64 << 20

// fakeStopLag is how many status calls still answer running=true after a
// STOP: as on a phone, only polling shows the VM is down.
const fakeStopLag = 2

// fakeAPKPath is where the installed app's APK is. Like a phone's `pm path`,
// it changes on every install.
func fakeAPKPath(state string) string {
	if b, err := os.ReadFile(filepath.Join(state, "apk-path")); err == nil {
		return string(b)
	}
	return "/data/app/~~preinstalled==/base.apk"
}

const (
	// fakeCrashes is the file that marks an APK as a build whose process dies at start.
	fakeCrashes = "crashes"
	// fakeVersionCode and fakeDebuggable mark an APK with what the device reads of
	// it, independently of the CLI's manifest parser.
	fakeVersionCode = "version-code"
	fakeDebuggable  = "debuggable"
	// fakeRuntimeGrants is the state file for the app's runtime permissions being
	// granted, as `adb install -g` does; an install without it leaves them as they were.
	fakeRuntimeGrants = "runtime-grants"
)

// fakeDiskFile is the app's disk image, under the state directory.
const fakeDiskFile = "app/files/disk.img"

// fakeSerial is what get-serialno answers when no -s is given.
const fakeSerial = "FAKESERIAL0001"

// fakeADBCalled is the state file that marks any adb command having run.
const fakeADBCalled = "adb-called"

func fakeADB(args []string) int {
	state := os.Getenv("FAKE_ADB_STATE")
	os.WriteFile(filepath.Join(state, fakeADBCalled), nil, 0o644)
	logEvent := func(format string, a ...any) {
		f, _ := os.OpenFile(filepath.Join(state, "events"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	serial := fakeSerial
	if env := os.Getenv("ANDROID_SERIAL"); env != "" {
		serial = env
	}
	if len(args) >= 2 && args[0] == "-s" {
		serial, args = args[1], args[2:]
	}
	devPath := func(p string) string { return filepath.Join(state, "device", p) }
	if _, err := os.Stat(filepath.Join(state, "offline")); err == nil {
		fmt.Fprintln(os.Stderr, "adb: device offline")
		return 1
	}
	switch args[0] {
	case "get-state":
		if _, err := os.Stat(filepath.Join(state, "get-state-fails")); err == nil {
			fmt.Fprintln(os.Stderr, "adb: no devices/emulators found")
			return 1
		}
		fmt.Println("device")
	case "get-serialno":
		fmt.Println(serial)
	case "install":
		local := args[len(args)-1]
		flags := args[1 : len(args)-1]
		logEvent("install %s", filepath.Base(local))
		if b, err := os.ReadFile(filepath.Join(state, "install-fails")); err == nil {
			fmt.Println("Performing Streamed Install")
			fmt.Println(strings.TrimSpace(string(b)))
			if _, err := os.Stat(filepath.Join(state, "install-drops")); err == nil {
				os.WriteFile(filepath.Join(state, "offline"), nil, 0o644)
			}
			return 1
		}
		b, err := os.ReadFile(local)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !slices.Contains(flags, "-r") && fakeHasApp(state) {
			fmt.Println("Performing Streamed Install")
			fmt.Println("Failure [INSTALL_FAILED_ALREADY_EXISTS: Attempt to re-install without first uninstalling.]")
			return 1
		}
		if fakeRefusesDowngrade(state, b, slices.Contains(flags, "-d")) {
			fmt.Println("Performing Streamed Install")
			fmt.Println("Failure [INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected]")
			return 1
		}
		os.RemoveAll(filepath.Dir(devPath(fakeAPKPath(state))))
		events, _ := os.ReadFile(filepath.Join(state, "events"))
		apk := fmt.Sprintf("/data/app/~~install%d==/base.apk", strings.Count(string(events), "\n"))
		os.MkdirAll(filepath.Dir(devPath(apk)), 0o755)
		os.WriteFile(devPath(apk), b, 0o644)
		os.WriteFile(filepath.Join(state, "apk-path"), []byte(apk), 0o644)
		os.WriteFile(filepath.Join(state, "installed"), nil, 0o644)
		if slices.Contains(flags, "-g") {
			os.WriteFile(filepath.Join(state, fakeRuntimeGrants), nil, 0o644)
		}
		// As on a phone: replacing the app kills its process, and so a running VM.
		if fakeRunning(state) {
			logEvent("vm killed")
		}
		setFakeRunning(state, false)
		if b, err := os.ReadFile(filepath.Join(state, "install-fails-late")); err == nil {
			fmt.Println(strings.TrimSpace(string(b)))
			return 1
		}
		fmt.Println("Success")
	case "uninstall":
		logEvent("uninstall %s", strings.Join(args[1:], " "))
		os.Remove(filepath.Join(state, "installed"))
		os.Remove(filepath.Join(state, fakeRuntimeGrants))
		os.RemoveAll(filepath.Dir(devPath(fakeAPKPath(state))))
		if fakeRunning(state) {
			logEvent("vm killed")
		}
		setFakeRunning(state, false)
	case "push":
		local, remote := args[len(args)-2], args[len(args)-1]
		b, err := os.ReadFile(local)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(b) > fakeChunkCap {
			fmt.Fprintln(os.Stderr, "adb: device offline (pushed too much at once)")
			return 1
		}
		os.MkdirAll(filepath.Dir(devPath(remote)), 0o755)
		os.WriteFile(devPath(remote), b, 0o644)
	case "pull":
		remote, local := args[len(args)-2], args[len(args)-1]
		logEvent("pull %s", remote)
		b, err := os.ReadFile(devPath(remote))
		if err != nil {
			fmt.Fprintf(os.Stderr, "adb: error: failed to stat remote object '%s': No such file or directory\n", remote)
			return 1
		}
		if len(b) > fakeChunkCap {
			fmt.Fprintln(os.Stderr, "adb: device offline (pulled too much at once)")
			return 1
		}
		if err := os.WriteFile(local, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "logcat":
		fmt.Print(fakeLogcat(state, args[1:]))
		for _, a := range args[1:] {
			if uid, ok := strings.CutPrefix(a, "--uid="); ok {
				fmt.Printf("I Vm: a line from uid %s\n", uid)
			}
		}
		// As on a phone, logcat without -d follows the log until it's killed.
		if !slices.Contains(args, "-d") {
			writePID(state, "logcat.pid")
			time.Sleep(time.Hour)
		}
	case "shell":
		// With shell-hangs, the phone stops answering for as long as the file stays.
		if _, err := os.Stat(filepath.Join(state, "shell-hangs")); err == nil {
			writePID(state, "shell-hung.pid")
			for err == nil {
				time.Sleep(5 * time.Millisecond)
				_, err = os.Stat(filepath.Join(state, "shell-hangs"))
			}
			return 1
		}
		line, ok := strings.CutSuffix(args[1], "; echo "+adb.ShellExitMarker+"$?")
		if !ok {
			fmt.Fprintln(os.Stderr, "fake adb: shell without exit marker")
			return 1
		}
		for _, drop := range fakeDrops(state) {
			if strings.Contains(line, drop.Match) {
				fmt.Print(drop.Stdout)
				fmt.Fprint(os.Stderr, drop.Stderr)
				return 1
			}
		}
		out, status := fakeShell(state, line, logEvent)
		fmt.Print(out)
		fmt.Printf("%s%d\n", adb.ShellExitMarker, status)
	default:
		fmt.Fprintln(os.Stderr, "fake adb: unknown command", args)
		return 1
	}
	return 0
}

// fakeDrop is adb itself failing a shell command: it printed Stdout of the
// command's output, and Stderr is its own complaint.
type fakeDrop struct {
	// Match is a substring of the command.
	Match, Stdout, Stderr string
}

func fakeDrops(state string) []fakeDrop {
	b, _ := os.ReadFile(filepath.Join(state, "drops.json"))
	var drops []fakeDrop
	json.Unmarshal(b, &drops)
	return drops
}

// fakeRunning is the VM's state: the "running" state file once anything
// started or stopped it, else $FAKE_ADB_RUNNING.
func fakeRunning(state string) bool {
	if b, err := os.ReadFile(filepath.Join(state, "running")); err == nil {
		return string(b) == "true"
	}
	return os.Getenv("FAKE_ADB_RUNNING") == "1"
}

func setFakeRunning(state string, running bool) {
	os.Remove(filepath.Join(state, "stopping"))
	os.WriteFile(filepath.Join(state, "running"), []byte(strconv.FormatBool(running)), 0o644)
}

// fakeStatusRunning is what a status call sees, and it counts down a STOP's lag.
func fakeStatusRunning(state string) bool {
	p := filepath.Join(state, "stopping")
	if b, err := os.ReadFile(p); err == nil {
		n, _ := strconv.Atoi(string(b))
		if n <= 1 {
			setFakeRunning(state, false)
		} else {
			os.WriteFile(p, []byte(strconv.Itoa(n-1)), 0o644)
		}
	}
	return fakeRunning(state)
}

// fakeInstalled reports whether pkg is installed: the "installed" state file
// names one package, or is empty for either. Without it, a running VM means
// the app is there, as either package.
func fakeInstalled(state, pkg string) bool {
	b, err := os.ReadFile(filepath.Join(state, "installed"))
	if err != nil {
		return fakeRunning(state)
	}
	return len(b) == 0 || string(b) == pkg
}

// fakeHasApp reports whether the app is installed, as either package: the model
// holds one build.
func fakeHasApp(state string) bool {
	return exists(filepath.Join(state, "installed")) || fakeRunning(state)
}

// fakeAPKEntry is a file in the installed app's APK, the way fakeAPK variants
// mark what a build does on a device; false if the APK has no such file.
func fakeAPKEntry(state, name string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(state, "device", fakeAPKPath(state)))
	if err != nil {
		return nil, false
	}
	return zipEntry(b, name)
}

// fakeRefusesDowngrade is Android's rule for installing apk: a lower version
// code needs -d (allow), and then passes only over a debuggable build or on a
// debuggable OS. APKs without the fakeVersionCode mark aren't compared.
func fakeRefusesDowngrade(state string, apk []byte, allow bool) bool {
	newer, ok := zipEntry(apk, fakeVersionCode)
	if !ok {
		return false
	}
	older, ok := fakeAPKEntry(state, fakeVersionCode)
	if !ok {
		return false
	}
	n, _ := strconv.ParseInt(string(newer), 10, 64)
	o, _ := strconv.ParseInt(string(older), 10, 64)
	if n >= o {
		return false
	}
	if !allow {
		return true
	}
	if _, debuggable := fakeAPKEntry(state, fakeDebuggable); debuggable {
		return false
	}
	osDebuggable, _ := os.ReadFile(filepath.Join(state, "props", "ro.debuggable"))
	return string(osDebuggable) != "1"
}

// fakeDumpsysPackage is dumpsys package for a build with the fakeVersionCode
// mark, in a phone's shape; a resolver table comes first, whose lines must not count.
func fakeDumpsysPackage(state, pkg string) (string, bool) {
	code, ok := fakeAPKEntry(state, fakeVersionCode)
	if !ok {
		return "", false
	}
	flags := "HAS_CODE ALLOW_CLEAR_USER_DATA"
	if _, debuggable := fakeAPKEntry(state, fakeDebuggable); debuggable {
		flags = "DEBUGGABLE " + flags
	}
	return "Activity Resolver Table:\n  versionCode=1\n  flags=[ DEBUGGABLE ]\n\nPackages:\n" +
		"  Package [" + pkg + "] (a1b2c3):\n    appId=10200\n" +
		"    versionCode=" + string(code) + " minSdk=34 targetSdk=36\n    versionName=0.1.0\n" +
		"    flags=[ " + flags + " ]\n    privateFlags=[ PRIVATE_FLAG_ACTIVITIES_RESIZE_MODE_RESIZEABLE ]\n" +
		"    pkgFlags=[ " + flags + " ]\n", true
}

// zipEntry is a file in the zip b, false if it has none or b is no zip.
func zipEntry(b []byte, name string) ([]byte, bool) {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, false
	}
	f, err := z.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	v, err := io.ReadAll(f)
	return v, err == nil
}

// fakeAppUID is the app's uid, as `pm list packages -U` answers for it: the
// release app's when only that is installed.
func fakeAppUID(state string) string {
	if b, _ := os.ReadFile(filepath.Join(state, "installed")); string(b) == releasePackage {
		return "10100"
	}
	return "10200"
}

// fakeProviderPackage is the app package of a content call's provider URI.
func fakeProviderPackage(line string) string {
	_, rest, _ := strings.Cut(line, "content://")
	rest, _, _ = strings.Cut(rest, " ")
	rest = strings.Trim(rest, "'")
	if i := strings.LastIndex(rest, "."); i > 0 {
		return rest[:i]
	}
	return rest
}

// fakeCannedKey names a canned answer's file: the command's words, as sh
// splits them, so quoting doesn't matter.
func fakeCannedKey(line string) string {
	// Hashed, as a long script would overrun a file name.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(shellWords(line), " "))))
}

func fakeShell(state, line string, logEvent func(string, ...any)) (string, int) {
	if b, err := os.ReadFile(filepath.Join(state, "canned", fakeCannedKey(line))); err == nil {
		status := 0
		if s, err := os.ReadFile(filepath.Join(state, "canned", fakeCannedKey(line)+".status")); err == nil {
			status, _ = strconv.Atoi(string(s))
		}
		return string(b), status
	}
	f := strings.Fields(line)
	w := shellWords(line)
	disk := filepath.Join(state, fakeDiskFile)
	part := disk + ".part"
	unquote := func(s string) string { return strings.Trim(s, "'") }
	devPath := func(p string) string { return filepath.Join(state, "device", p) }
	if _, err := os.Stat(filepath.Join(state, "limiter-high")); err == nil {
		if out, status, ok := fakeLimiter(state, line, w); ok {
			return out, status
		}
	}
	switch {
	case strings.HasPrefix(line, "dumpsys notification --noredact | grep "):
		return fakeNotifications(state, w)
	case strings.Contains(line, ".control"):
		return fakeControl(state, line)
	case len(w) == 2 && w[0] == "getprop":
		// Like getprop, an unset prop reads empty.
		b, _ := os.ReadFile(filepath.Join(state, "props", w[1]))
		return string(b) + "\n", 0
	case len(w) == 4 && w[0] == "stat" && w[1] == "-c" && w[2] == "%s":
		if b, err := os.ReadFile(filepath.Join(state, "stat-size")); err == nil {
			return string(b) + "\n", 0
		}
		fi, err := os.Stat(devPath(w[3]))
		if err != nil {
			return "stat: '" + w[3] + "': No such file or directory\n", 1
		}
		return strconv.FormatInt(fi.Size(), 10) + "\n", 0
	// A running VM's crosvm goes by the app's uid, and shows in ps as long as the VM runs.
	case line == vmProcessesCommand:
		out := "0 init\n1000 system_server\n2000 adbd\n"
		if fakeRunning(state) {
			out += fakeAppUID(state) + " crosvm_pocketnix\n"
		}
		return out, 0
	case len(w) == 3 && w[0] == "dumpsys" && w[1] == "package" && (w[2] == releasePackage || w[2] == debugPackage):
		if out, ok := fakeDumpsysPackage(state, w[2]); ok {
			return out, 0
		}
		return "fake adb: unknown shell command " + line, 127
	case len(w) == 2 && w[0] == "ls":
		if _, err := os.Stat(devPath(w[1])); err != nil {
			return "ls: " + w[1] + ": No such file or directory\n", 1
		}
		return w[1] + "\n", 0
	case len(f) == 4 && f[0] == "pm" && f[1] == "grant":
		logEvent("grant %s %s", f[2], f[3])
	case len(w) == 3 && w[0] == "pm" && w[1] == "path":
		if w[2] == releasePackage || w[2] == debugPackage {
			if !fakeInstalled(state, w[2]) {
				return "", 1
			}
			if b, err := os.ReadFile(filepath.Join(state, "pm-path")); err == nil {
				return string(b), 0
			}
			return "package:" + fakeAPKPath(state) + "\n", 0
		}
		if fakeHasPackage(state, w[2]) {
			return "package:/system/priv-app/" + w[2] + "/base.apk\n", 0
		}
		return "", 1
	case len(w) == 4 && w[0] == "pm" && w[1] == "list" && w[2] == "packages" && !strings.HasPrefix(w[3], "-"):
		var out strings.Builder
		for _, p := range fakePackages(state) {
			if strings.Contains(p, w[3]) {
				out.WriteString("package:" + p + "\n")
			}
		}
		return out.String(), 0
	case len(f) >= 6 && f[0] == "content" && f[1] == "call":
		method := f[5]
		arg := ""
		if len(f) >= 8 {
			arg = unquote(f[7])
		}
		if pkg := fakeProviderPackage(line); !fakeInstalled(state, pkg) {
			return "Error while accessing provider:" + pkg + ".provision\n", 1
		}
		if _, broken := fakeAPKEntry(state, fakeCrashes); broken {
			return "Exception occurred while executing 'call':\njava.lang.RuntimeException: Unable to get provider\n", 1
		}
		switch method {
		case "status":
			_, err := os.Stat(disk)
			return fmt.Sprintf("Result: Bundle[{ok=true, disk=%t, running=%t}]\n", err == nil, fakeStatusRunning(state)), 0
		case "caps":
			b, err := os.ReadFile(filepath.Join(state, "caps"))
			if err != nil {
				return "Result: Bundle[{error=unknown method caps}]\n", 0
			}
			return "Result: Bundle[{" + string(b) + ", ok=true}]\n", 0
		case "begin":
			n, _ := strconv.ParseInt(arg, 10, 64)
			os.MkdirAll(filepath.Dir(disk), 0o755)
			os.WriteFile(part, nil, 0o600)
			os.Truncate(part, n)
		case "commit":
			if _, err := os.Stat(disk); err == nil && arg != "replace" {
				return "Result: Bundle[{error=a disk image exists, pass replace}]\n", 0
			}
			for i, w := range f {
				if v, ok := strings.CutPrefix(unquote(w), "sha256:s:"); ok && i > 0 && f[i-1] == "--extra" {
					if got, _ := fileSHA256(part); got != v {
						return "Result: Bundle[{error=the image doesn't match (sha256 " + got + ")}]\n", 0
					}
				}
			}
			os.Rename(part, disk)
		}
		return "Result: Bundle[{ok=true}]\n", 0
	case len(f) == 6 && f[0] == "content" && f[1] == "write" && f[4] == "<":
		if os.Getenv("FAKE_ADB_FULL") == "1" {
			return "Error: No space left on device", 1
		}
		uri := unquote(f[3])
		_, off, _ := strings.Cut(uri, "?offset=")
		n, _ := strconv.ParseInt(off, 10, 64)
		b, err := os.ReadFile(devPath(f[5]))
		if err != nil {
			return err.Error(), 1
		}
		w, _ := os.OpenFile(part, os.O_WRONLY, 0)
		w.WriteAt(b, n)
		w.Close()
	case len(f) == 5 && f[0] == "pm" && f[1] == "list" && f[2] == "packages" && f[3] == "-U":
		// pm's filter matches substrings, so the release app's name lists both.
		var out strings.Builder
		for _, p := range []string{releasePackage + " uid:10100", debugPackage + " uid:10200"} {
			if strings.Contains(p, unquote(f[4])) {
				out.WriteString("package:" + p + "\n")
			}
		}
		return out.String(), 0
	case len(f) >= 1 && f[0] == "am":
		logEvent("%s", line)
		switch {
		case strings.Contains(line, "-a "+actionStop):
			if fakeRunning(state) {
				os.WriteFile(filepath.Join(state, "stopping"), []byte(strconv.Itoa(fakeStopLag)), 0o644)
			}
		case strings.Contains(line, "-a "+actionStart):
			setFakeRunning(state, true)
		case strings.Contains(line, "-a "+actionLaunch):
			fakeLaunch(state, w)
		}
	// The phone side of a forward: toybox nc to a phone port.
	case len(w) == 6 && w[0] == "timeout" && w[2] == "nc" && w[3] == "127.0.0.1":
		return fakePhoneConnect(state, w[4])
	case len(f) == 3 && f[0] == "rm":
		os.Remove(devPath(f[2]))
	default:
		return "fake adb: unknown shell command " + line, 127
	}
	return "", 0
}

// fakeNotifications is the phone's grep of the state's "notifications" text,
// which stands for dumpsys notification: the lines matching any -e of w,
// exiting 1 when there are none.
func fakeNotifications(state string, w []string) (string, int) {
	b, _ := os.ReadFile(filepath.Join(state, "notifications"))
	var res []*regexp.Regexp
	for i, a := range w {
		if a == "-e" && i+1 < len(w) {
			res = append(res, regexp.MustCompile(w[i+1]))
		}
	}
	var out strings.Builder
	for _, l := range strings.Split(string(b), "\n") {
		if slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(l) }) {
			out.WriteString(l + "\n")
		}
	}
	if out.Len() == 0 {
		return "", 1
	}
	return out.String(), 0
}

// fakePID is the app's process in a fake phone with a limiter.
const fakePID = "4242"

// fakeLimiter is MemoryLimiter with one running process: ok says it took line.
// As on a phone, an ignored uid's manual limit is dropped, and `manual none`
// changes nothing.
func fakeLimiter(state, line string, w []string) (out string, status int, ok bool) {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(state, name))
		return strings.TrimSpace(string(b))
	}
	write := func(name, value string) { os.WriteFile(filepath.Join(state, name), []byte(value), 0o644) }
	if strings.HasPrefix(line, "pidof ") {
		return fakePID + "\n", 0, true
	}
	if line == "stat -c %u /proc/"+fakePID {
		return fakeAppUID(state) + "\n", 0, true
	}
	if strings.HasPrefix(line, "p=$(pidof ") {
		or := func(name, fallback string) string {
			if v := read(name); v != "" {
				return v
			}
			return fallback
		}
		return "state=" + or("limiter-state", "4 (FOREGROUND_SERVICE)") + "\nhigh=" + read("limiter-high") + "\nswapMax=max\ncurrent=" + or("limiter-current", "1") +
			"\npeak=1\nhighEvents=" + or("limiter-events", "0") + "\navail=" + or("limiter-avail", "2000000") + "\nscan=0\nsteal=0\nsys=10\n", 0, true
	}
	if !strings.HasPrefix(line, "cmd activity memory-limiter ") {
		return "", 0, false
	}
	ignored := read("limiter-ignored")
	if ignored == "" {
		ignored = "none"
	}
	switch args := w[3:]; args[0] {
	case "status":
		return "Memory limiter\n  enabled limits=true monitoring=true killing=true ignored=" + ignored +
			"\n  visibleMem=6144MB visibleSwap=3072MB notVisibleMem=3072MB notVisibleSwap=3072MB\n", 0, true
	case "ignore":
		write("limiter-ignored", args[1])
		return "", 0, true
	case "manual":
		if args[2] == "none" || ignored == fakeAppUID(state) || ignored == "all" {
			return "", 0, true
		}
		// With limiter-stuck, a limit never reaches the cgroup.
		if _, err := os.Stat(filepath.Join(state, "limiter-stuck")); err == nil {
			return "", 0, true
		}
		mib, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return "Invalid limit\n", 255, true
		}
		write("limiter-high", strconv.FormatInt(mib<<20, 10))
		return "", 0, true
	}
	return "Error: unknown sub-command\n", 255, true
}

// fakeLogcat is the state's "logcat" lines; with -T <epoch>, only those since
// then, each starting with its epoch time as `-v epoch` prints it.
func fakeLogcat(state string, args []string) string {
	b, _ := os.ReadFile(filepath.Join(state, "logcat"))
	since := -1.0
	for i, a := range args {
		if a == "-T" && i+1 < len(args) {
			since, _ = strconv.ParseFloat(args[i+1], 64)
		}
	}
	var out strings.Builder
	for _, l := range strings.SplitAfter(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if t, err := strconv.ParseFloat(f[0], 64); since < 0 || (err == nil && t >= since) {
			out.WriteString(l)
		}
	}
	return out.String()
}

// fakePackages lists the packages other than the app that the device has.
func fakePackages(state string) []string {
	b, _ := os.ReadFile(filepath.Join(state, "packages"))
	return strings.Fields(string(b))
}

func fakeHasPackage(state, pkg string) bool {
	for _, p := range fakePackages(state) {
		if p == pkg {
			return true
		}
	}
	return false
}

type fakeDevice struct {
	state string
	// cache is os.UserCacheDir() for the test, where install keeps its backups.
	cache string
}

func newFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, "adb")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := &fakeDevice{state: t.TempDir(), cache: t.TempDir()}
	t.Setenv("FAKE_ADB_STATE", d.state)
	t.Setenv("XDG_CACHE_HOME", d.cache)
	t.Setenv("ANDROID_SERIAL", "")
	return d
}

func (d *fakeDevice) write(name string, b []byte) {
	p := filepath.Join(d.state, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, b, 0o644)
}

// adbCalled is whether any adb command has run against the device.
func (d *fakeDevice) adbCalled() bool { return exists(filepath.Join(d.state, fakeADBCalled)) }

func (d *fakeDevice) events() string {
	b, _ := os.ReadFile(filepath.Join(d.state, "events"))
	return string(b)
}

// hasEvent reports whether any line of events starts with prefix.
func hasEvent(events, prefix string) bool {
	for _, line := range strings.Split(events, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func (d *fakeDevice) disk() []byte {
	b, _ := os.ReadFile(filepath.Join(d.state, fakeDiskFile))
	return b
}

// setDisk gives the phone a disk image, as if provisioned before the test.
func (d *fakeDevice) setDisk(b []byte) { d.write(fakeDiskFile, b) }

func (d *fakeDevice) running() bool { return fakeRunning(d.state) }

func (d *fakeDevice) setRunning(running bool) { setFakeRunning(d.state, running) }

// installAPK puts an app build on the device as if installed before the
// test, as either package.
func (d *fakeDevice) installAPK(b []byte) { d.installAPKAs("", b) }

// installAPKAs installs b as pkg only; the other package isn't installed.
func (d *fakeDevice) installAPKAs(pkg string, b []byte) {
	d.write(filepath.Join("device", fakeAPKPath(d.state)), b)
	d.write("installed", []byte(pkg))
}

// installedAPK is the installed app's APK, nil when there's none, and what
// fakeAPK put in it for one of those.
func (d *fakeDevice) installedAPK() []byte {
	b, _ := os.ReadFile(filepath.Join(d.state, "device", fakeAPKPath(d.state)))
	if z, err := zip.NewReader(bytes.NewReader(b), int64(len(b))); err == nil {
		if f, err := z.Open("build"); err == nil {
			defer f.Close()
			build, _ := io.ReadAll(f)
			return build
		}
	}
	return b
}

// runtimePermissionsGranted is whether an install granted the app's runtime permissions.
func (d *fakeDevice) runtimePermissionsGranted() bool {
	return exists(filepath.Join(d.state, fakeRuntimeGrants))
}

// failInstalls makes every adb install fail with msg (pm's "Failure [...]" line).
func (d *fakeDevice) failInstalls(msg string) { d.write("install-fails", []byte(msg)) }

// failInstallsLate makes every adb install replace the app and then fail with msg,
// as when the connection goes after pm committed.
func (d *fakeDevice) failInstallsLate(msg string) { d.write("install-fails-late", []byte(msg)) }

// dropAtInstall makes the next adb install fail with msg and take the phone off adb.
func (d *fakeDevice) dropAtInstall(msg string) {
	d.failInstalls(msg)
	d.write("install-drops", nil)
}

// reconnect brings the phone back on adb after dropAtInstall, with installs working.
func (d *fakeDevice) reconnect() {
	for _, f := range []string{"offline", "install-drops"} {
		os.Remove(filepath.Join(d.state, f))
	}
	d.allowInstalls()
}

// allowInstalls undoes failInstalls and failInstallsLate.
func (d *fakeDevice) allowInstalls() {
	os.Remove(filepath.Join(d.state, "install-fails"))
	os.Remove(filepath.Join(d.state, "install-fails-late"))
}

// addFile puts a file on the device, e.g. /apex/com.android.virt/etc/u-boot.bin.
func (d *fakeDevice) addFile(path string, b []byte) { d.write(filepath.Join("device", path), b) }

// setPMPath overrides `pm path` for the app, e.g. with a split install's lines.
func (d *fakeDevice) setPMPath(out string) { d.write("pm-path", []byte(out)) }

// setAPKSize overrides every `stat -c %s` answer.
func (d *fakeDevice) setAPKSize(n int64) { d.write("stat-size", []byte(strconv.FormatInt(n, 10))) }

// setProp sets a system property; unset ones read empty.
func (d *fakeDevice) setProp(name, value string) {
	d.write(filepath.Join("props", name), []byte(value))
}

// denyAVF makes the phone say it lacks AVF's system feature.
func (d *fakeDevice) denyAVF() { d.can(avfFeatureCommand, "false\n", 1) }

// addPackages installs packages other than the app (e.g. app.grapheneos.apps).
func (d *fakeDevice) addPackages(pkgs ...string) {
	d.write("packages", []byte(strings.Join(append(fakePackages(d.state), pkgs...), "\n")))
}

// setCaps makes ProvisionProvider answer caps with these key=value pairs
// (comma-separated, as in a Bundle); without it the app predates caps.
func (d *fakeDevice) setCaps(pairs string) { d.write("caps", []byte(pairs)) }

// can makes the shell command answer out with status, whatever its quoting.
func (d *fakeDevice) can(cmdline, out string, status int) {
	d.write(filepath.Join("canned", fakeCannedKey(cmdline)), []byte(out))
	d.write(filepath.Join("canned", fakeCannedKey(cmdline)+".status"), []byte(strconv.Itoa(status)))
}

// drop makes adb fail every shell command holding match, with this output and complaint.
func (d *fakeDevice) drop(match, stdout, stderr string) {
	b, _ := json.Marshal(append(fakeDrops(d.state), fakeDrop{match, stdout, stderr}))
	d.write("drops.json", b)
}

// stdoutOf runs f and returns what it wrote to os.Stdout.
func stdoutOf(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.Bytes()
	}()
	ferr := f()
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return string(out), ferr
}
