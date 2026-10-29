package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as a fake adb: provision tests put a symlink named
// adb to it first on PATH, and it simulates a device plus the app's provision
// provider in the directory $FAKE_ADB_STATE.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "adb" {
		os.Exit(fakeADB(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeChunkCap models the USB drop: one push may not exceed it.
const fakeChunkCap = 64 << 20

func fakeADB(args []string) int {
	state := os.Getenv("FAKE_ADB_STATE")
	logEvent := func(format string, a ...any) {
		f, _ := os.OpenFile(filepath.Join(state, "events"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	if len(args) >= 2 && args[0] == "-s" {
		args = args[2:]
	}
	devPath := func(p string) string { return filepath.Join(state, "device", p) }
	switch args[0] {
	case "get-state":
		fmt.Println("device")
	case "install":
		logEvent("install %s", filepath.Base(args[len(args)-1]))
		// As on a phone: replacing the app kills its process, and so a running VM.
		os.WriteFile(filepath.Join(state, "installed"), nil, 0o644)
		os.WriteFile(filepath.Join(state, "vm-killed"), nil, 0o644)
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

func fakeShell(state, line string, logEvent func(string, ...any)) (string, int) {
	f := strings.Fields(line)
	files := filepath.Join(state, "app", "files")
	disk, part := filepath.Join(files, "disk.img"), filepath.Join(files, "disk.img.part")
	unquote := func(s string) string { return strings.Trim(s, "'") }
	switch {
	case strings.Contains(line, ".control"):
		return fakeControl(state, line)
	case len(f) == 4 && f[0] == "pm" && f[1] == "grant":
		logEvent("grant %s %s", f[2], f[3])
	case len(f) == 3 && f[0] == "pm" && f[1] == "path":
		// A running VM means the app is there.
		if _, err := os.Stat(filepath.Join(state, "installed")); err != nil && os.Getenv("FAKE_ADB_RUNNING") != "1" {
			return "", 1
		}
		return "package:/data/app/base.apk\n", 0
	case len(f) >= 6 && f[0] == "content" && f[1] == "call":
		method := f[5]
		arg := ""
		if len(f) >= 8 {
			arg = unquote(f[7])
		}
		switch method {
		case "status":
			_, err := os.Stat(disk)
			_, killed := os.Stat(filepath.Join(state, "vm-killed"))
			running := os.Getenv("FAKE_ADB_RUNNING") == "1" && killed != nil
			return fmt.Sprintf("Result: Bundle[{ok=true, disk=%t, running=%t}]\n", err == nil, running), 0
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
		b, err := os.ReadFile(filepath.Join(state, "device", f[5]))
		if err != nil {
			return err.Error(), 1
		}
		w, _ := os.OpenFile(part, os.O_WRONLY, 0)
		w.WriteAt(b, n)
		w.Close()
	case len(f) >= 1 && f[0] == "am":
		logEvent("%s", line)
	case len(f) == 3 && f[0] == "rm":
		os.Remove(filepath.Join(state, "device", f[2]))
	default:
		return "fake adb: unknown shell command " + line, 127
	}
	return "", 0
}

type fakeDevice struct {
	state string
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
	d := &fakeDevice{state: t.TempDir()}
	t.Setenv("FAKE_ADB_STATE", d.state)
	return d
}

func (d *fakeDevice) events() string {
	b, _ := os.ReadFile(filepath.Join(d.state, "events"))
	return string(b)
}

func (d *fakeDevice) disk() []byte {
	b, _ := os.ReadFile(filepath.Join(d.state, "app", "files", "disk.img"))
	return b
}

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

func TestProvision(t *testing.T) {
	d := newFakeDevice(t)
	img, want := testImage(t)
	apk := filepath.Join(t.TempDir(), "app-debug.apk")
	os.WriteFile(apk, []byte("apk"), 0o644)

	err := provisionFor(t, "--image", img, "--apk", apk, "--debug", "--no-network", "--memory", "1024", "--cpus", "one", "--disk", "8")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.disk(), want) {
		t.Error("the phone's disk image differs from the pushed one")
	}
	ev := d.events()
	for _, line := range []string{
		"install app-debug.apk",
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
		"network": "--ez false", "memoryMb": "--ei 1024", "cpus": "--ei 1", "diskGb": "--ei 8", "minDiskGb": "--ei 1",
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
	apk := filepath.Join(t.TempDir(), "app.apk")
	os.WriteFile(apk, []byte("apk"), 0o644)
	if err := provisionFor(t, "--image", img, "--apk", apk, "--no-start"); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "other.raw")
	os.WriteFile(other, []byte("a different image"), 0o644)
	if err := provisionFor(t, "--image", other, "--apk", apk, "--no-start"); err == nil {
		t.Error("provision replaced an existing disk without --replace")
	}
	if !bytes.Equal(d.disk(), want) {
		t.Error("the existing disk changed")
	}
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
	apk := filepath.Join(t.TempDir(), "app.apk")
	os.WriteFile(apk, []byte("apk"), 0o644)
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
	apk := filepath.Join(t.TempDir(), "app.apk")
	os.WriteFile(apk, []byte("apk"), 0o644)
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
