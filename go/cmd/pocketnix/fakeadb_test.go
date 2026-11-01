package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as a fake adb: tests put a symlink named adb to it
// first on PATH, and it simulates a device plus the app's providers in the
// directory $FAKE_ADB_STATE.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "adb" {
		os.Exit(fakeADB(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeChunkCap models the USB drop: one push or pull may not exceed it.
const fakeChunkCap = 64 << 20

// fakeAPKPath is where the fake device keeps the installed app's APK.
const fakeAPKPath = "/data/app/base.apk"

// fakeSerial is what get-serialno answers when no -s is given.
const fakeSerial = "FAKESERIAL0001"

func fakeADB(args []string) int {
	state := os.Getenv("FAKE_ADB_STATE")
	logEvent := func(format string, a ...any) {
		f, _ := os.OpenFile(filepath.Join(state, "events"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	serial := fakeSerial
	if len(args) >= 2 && args[0] == "-s" {
		serial, args = args[1], args[2:]
	}
	devPath := func(p string) string { return filepath.Join(state, "device", p) }
	switch args[0] {
	case "get-state":
		fmt.Println("device")
	case "get-serialno":
		fmt.Println(serial)
	case "install":
		local := args[len(args)-1]
		logEvent("install %s", strings.Join(append(args[1:len(args)-1:len(args)-1], filepath.Base(local)), " "))
		if b, err := os.ReadFile(filepath.Join(state, "install-fails")); err == nil {
			fmt.Println("Performing Streamed Install")
			fmt.Println(strings.TrimSpace(string(b)))
			return 1
		}
		b, err := os.ReadFile(local)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		os.MkdirAll(filepath.Dir(devPath(fakeAPKPath)), 0o755)
		os.WriteFile(devPath(fakeAPKPath), b, 0o644)
		os.WriteFile(filepath.Join(state, "installed"), nil, 0o644)
		// As on a phone: replacing the app kills its process, and so a running VM.
		setFakeRunning(state, false)
		fmt.Println("Success")
	case "uninstall":
		logEvent("uninstall %s", strings.Join(args[1:], " "))
		os.Remove(filepath.Join(state, "installed"))
		os.Remove(devPath(fakeAPKPath))
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
		for _, a := range args[1:] {
			if uid, ok := strings.CutPrefix(a, "--uid="); ok {
				fmt.Printf("I Vm: a line from uid %s\n", uid)
			}
		}
	case "shell":
		line, ok := strings.CutSuffix(args[1], "; echo pocketnix-exit=$?")
		if !ok {
			fmt.Fprintln(os.Stderr, "fake adb: shell without exit marker")
			return 1
		}
		out, status := fakeShell(state, line, logEvent)
		fmt.Print(out)
		fmt.Printf("pocketnix-exit=%d\n", status)
	default:
		fmt.Fprintln(os.Stderr, "fake adb: unknown command", args)
		return 1
	}
	return 0
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
	os.WriteFile(filepath.Join(state, "running"), []byte(strconv.FormatBool(running)), 0o644)
}

func fakeInstalled(state string) bool {
	_, err := os.Stat(filepath.Join(state, "installed"))
	// A running VM means the app is there.
	return err == nil || fakeRunning(state)
}

// fakeCannedKey names a canned answer's file: the command's words, as sh
// splits them, so quoting doesn't matter.
func fakeCannedKey(line string) string {
	return url.PathEscape(strings.Join(shellWords(line), " "))
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
	files := filepath.Join(state, "app", "files")
	disk, part := filepath.Join(files, "disk.img"), filepath.Join(files, "disk.img.part")
	unquote := func(s string) string { return strings.Trim(s, "'") }
	devPath := func(p string) string { return filepath.Join(state, "device", p) }
	switch {
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
	case len(f) == 4 && f[0] == "pm" && f[1] == "grant":
		logEvent("grant %s %s", f[2], f[3])
	case len(w) == 3 && w[0] == "pm" && w[1] == "path":
		if w[2] == releasePackage || w[2] == debugPackage {
			if !fakeInstalled(state) {
				return "", 1
			}
			if b, err := os.ReadFile(filepath.Join(state, "pm-path")); err == nil {
				return string(b), 0
			}
			return "package:" + fakeAPKPath + "\n", 0
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
		switch method {
		case "status":
			_, err := os.Stat(disk)
			return fmt.Sprintf("Result: Bundle[{ok=true, disk=%t, running=%t}]\n", err == nil, fakeRunning(state)), 0
		case "caps":
			b, err := os.ReadFile(filepath.Join(state, "caps"))
			if err != nil {
				return "Result: Bundle[{error=unknown method caps}]\n", 0
			}
			return "Result: Bundle[{" + string(b) + ", ok=true}]\n", 0
		case "begin":
			n, _ := strconv.ParseInt(arg, 10, 64)
			os.MkdirAll(files, 0o755)
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
		// The app's ControlActivity; the action names mirror ControlActivity.ACTION_START and ACTION_STOP.
		switch {
		case strings.Contains(line, "-a dev.nickolaj.pocketnix.action.STOP"):
			setFakeRunning(state, false)
		case strings.Contains(line, "-a dev.nickolaj.pocketnix.action.START"):
			setFakeRunning(state, true)
		}
	case len(f) == 3 && f[0] == "rm":
		os.Remove(devPath(f[2]))
	default:
		return "fake adb: unknown shell command " + line, 127
	}
	return "", 0
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
	return d
}

func (d *fakeDevice) write(name string, b []byte) {
	p := filepath.Join(d.state, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, b, 0o644)
}

func (d *fakeDevice) events() string {
	b, _ := os.ReadFile(filepath.Join(d.state, "events"))
	return string(b)
}

func (d *fakeDevice) disk() []byte {
	b, _ := os.ReadFile(filepath.Join(d.state, "app", "files", "disk.img"))
	return b
}

func (d *fakeDevice) running() bool { return fakeRunning(d.state) }

func (d *fakeDevice) setRunning(running bool) { setFakeRunning(d.state, running) }

// installAPK puts an app build on the device as if installed before the test.
func (d *fakeDevice) installAPK(b []byte) {
	d.write(filepath.Join("device", fakeAPKPath), b)
	d.write("installed", nil)
}

// installedAPK is the installed app's APK, nil when there's none.
func (d *fakeDevice) installedAPK() []byte {
	b, _ := os.ReadFile(filepath.Join(d.state, "device", fakeAPKPath))
	return b
}

// failInstalls makes every adb install fail with msg (pm's "Failure [...]" line).
func (d *fakeDevice) failInstalls(msg string) { d.write("install-fails", []byte(msg)) }

// setPMPath overrides `pm path` for the app, e.g. with a split install's lines.
func (d *fakeDevice) setPMPath(out string) { d.write("pm-path", []byte(out)) }

// setAPKSize overrides every `stat -c %s` answer.
func (d *fakeDevice) setAPKSize(n int64) { d.write("stat-size", []byte(strconv.FormatInt(n, 10))) }

// setProp sets a system property; unset ones read empty.
func (d *fakeDevice) setProp(name, value string) {
	d.write(filepath.Join("props", name), []byte(value))
}

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
