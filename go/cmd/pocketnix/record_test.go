package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const (
	// fakeDoctorName is the test binary's name when it plays pocketnix doctor.
	fakeDoctorName = "pocketnix-fake-doctor"

	// A serial of each kind must stay out of a bundle: the one adb knows, and the phone's own.
	recordSerial   = "TESTSERIAL123"
	recordHWSerial = "HARDWARESERIAL9"

	// scrcpy 4.1's help for the flags scrcpyArgs looks at.
	scrcpyHelpNew = "    -b, --video-bit-rate=value\n    -N, --no-playback\n    --no-audio\n    --no-window\n    --no-window-aspect-ratio-lock\n"
	// A scrcpy older than --no-window, whose help has the longer flag.
	scrcpyHelpOld = "    -b, --bit-rate=value\n    -N, --no-display\n    --no-window-aspect-ratio-lock\n"
)

// fakeScrcpy models scrcpy: it writes its video until SIGINT, and only then
// the end that makes it playable. Its state files are under $FAKE_ADB_STATE:
// scrcpy-help is its --help, and scrcpy-fails makes it exit with that status at
// once, having written scrcpy-video's bytes (nothing at all without that file).
func fakeScrcpy(args []string) int {
	state := os.Getenv("FAKE_ADB_STATE")
	if slices.Contains(args, "--help") {
		help, err := os.ReadFile(filepath.Join(state, "scrcpy-help"))
		if err != nil {
			help = []byte(scrcpyHelpNew)
		}
		fmt.Print(string(help))
		return 0
	}
	os.WriteFile(filepath.Join(state, "scrcpy-args"), []byte(strings.Join(args, "\n")), 0o644)
	os.WriteFile(filepath.Join(state, "scrcpy-adb"), []byte(os.Getenv("ADB")), 0o644)
	var video string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--record="); ok {
			video = v
		}
	}
	if b, err := os.ReadFile(filepath.Join(state, "scrcpy-fails")); err == nil {
		if frames, err := os.ReadFile(filepath.Join(state, "scrcpy-video")); err == nil {
			os.WriteFile(video, frames, 0o644)
		}
		fmt.Fprintln(os.Stderr, "ERROR: Server connection failed")
		status, _ := strconv.Atoi(string(b))
		return status
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	os.WriteFile(video, []byte("frames"), 0o644)
	writePID(state, "scrcpy.pid")
	<-sig
	f, _ := os.OpenFile(video, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("+index")
	f.Close()
	return 0
}

// writePID publishes this process's pid in one step, for a test waiting on the file.
func writePID(state, name string) {
	tmp := filepath.Join(state, name+".tmp")
	os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o644)
	os.Rename(tmp, filepath.Join(state, name))
}

// fakeDoctorProcess is `pocketnix doctor`, for tests that signal the process.
// Its smoke runs with short waits, except for the PTY step, which they interrupt.
func fakeDoctorProcess(args []string) int {
	controlPoll = 5 * time.Millisecond
	s := time.Second
	smokeTimes.cmd, smokeTimes.offer, smokeTimes.notify, smokeTimes.wake = s, s, s, s
	smokeTimes.window, smokeTimes.frames, smokeTimes.gone, smokeTimes.forward = s, s, s, s
	smokeTimes.exit, smokeTimes.ptyPoll = 50*time.Millisecond, 5*time.Millisecond
	err := runDoctor(args)
	var status exitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &status):
		return int(status)
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

// recordDevice is a fake Pixel, with a fake scrcpy on PATH and its serial in a
// logcat line, the way a phone's own log can carry it.
func recordDevice(t *testing.T) *fakeDevice {
	t.Helper()
	d := newFakeDevice(t)
	cannedPixel(d)
	d.setProp("ro.serialno", recordHWSerial)
	addFakeBin(t, "scrcpy")
	d.write("logcat", []byte("1761999000.000  1234  1234 I Build   : serial "+recordHWSerial+" on "+recordSerial+"\n1761999001.000  1234  1234 I Vm      : a line\n"))
	return d
}

// addFakeBin puts the test binary first on PATH under name; TestMain picks the fake by it.
func addFakeBin(t *testing.T, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(bin, name)
}

// onlyBundle is the one thing record left in dir, which must be a bundle.
func onlyBundle(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("record left %v in its directory, want one bundle", entries)
	}
	return filepath.Join(dir, entries[0].Name())
}

func readBundle(t *testing.T, bundle, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// logIndex is the first commands.log line holding text, failing the test without one.
func logIndex(t *testing.T, lines []string, text string) int {
	t.Helper()
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, text) })
	if i < 0 {
		t.Errorf("commands.log has no line with %q:\n%s", text, strings.Join(lines, "\n"))
	}
	return i
}

// assertNoSerial checks every file of the bundle for both serials.
func assertNoSerial(t *testing.T, bundle string) {
	t.Helper()
	entries, err := os.ReadDir(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b := readBundle(t, bundle, e.Name())
		for _, serial := range []string{recordSerial, recordHWSerial, fakeSerial} {
			if strings.Contains(b, serial) {
				t.Errorf("%s holds the serial %s", e.Name(), serial)
			}
		}
	}
	if strings.Contains(bundle, recordSerial) {
		t.Errorf("the bundle's name holds the serial: %s", bundle)
	}
}

// assertGone waits for the pid in state's file to be gone.
func assertGone(t *testing.T, state, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, name))
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Errorf("%s: process %d is still there", name, pid)
			syscall.Kill(pid, syscall.SIGKILL)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecordBundle(t *testing.T) {
	d := recordDevice(t)
	dir := filepath.Join(t.TempDir(), "new", "records")
	if _, err := stdoutOf(t, func() error {
		return runDoctor([]string{"--serial", recordSerial, "--record", dir})
	}); err != nil {
		t.Fatal(err)
	}

	bundle := onlyBundle(t, dir)
	if !regexp.MustCompile(`^\d{8}T\d{6}Z-Pixel-8-Pro$`).MatchString(filepath.Base(bundle)) {
		t.Errorf("bundle name %q", filepath.Base(bundle))
	}
	for _, name := range []string{"commands.log", "doctor.json", "logcat.txt", "screen.mp4", "README.txt"} {
		if !exists(filepath.Join(bundle, name)) {
			t.Errorf("the bundle has no %s", name)
		}
	}
	if exists(filepath.Join(bundle, "smoke.txt")) {
		t.Error("a run without --smoke wrote smoke.txt")
	}
	assertNoSerial(t, bundle)
	readme := strings.ToLower(readBundle(t, bundle, "README.txt"))
	for _, token := range []string{"review", "git revision:", "cli version:"} {
		if !strings.Contains(readme, token) {
			t.Errorf("the README has no %q", token)
		}
	}
	var r doctorReport
	if err := json.Unmarshal([]byte(readBundle(t, bundle, "doctor.json")), &r); err != nil {
		t.Fatal(err)
	}
	if r.Device.Model == nil || *r.Device.Model != "Pixel 8 Pro" || r.Smoke != nil {
		t.Errorf("doctor.json: model %v, smoke %v", r.Device.Model, r.Smoke)
	}
	if v := readBundle(t, bundle, "screen.mp4"); v != "frames+index" {
		t.Errorf("screen.mp4 = %q: scrcpy wasn't stopped with SIGINT", v)
	}
	if got := readBundle(t, bundle, "logcat.txt"); !strings.Contains(got, "a line") || !strings.Contains(got, serialPlaceholder) {
		t.Errorf("logcat.txt:\n%s", got)
	}
	assertGone(t, d.state, "scrcpy.pid")
	assertGone(t, d.state, "logcat.pid")

}

func TestRecordCommandsLog(t *testing.T) {
	recordDevice(t)
	dir := t.TempDir()
	if _, err := stdoutOf(t, func() error {
		return runDoctor([]string{"--serial", recordSerial, "--record", dir})
	}); err != nil {
		t.Fatal(err)
	}
	bundle := onlyBundle(t, dir)
	log := readBundle(t, bundle, "commands.log")
	lines := strings.Split(strings.TrimSpace(log), "\n")
	assertLine := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z (exit=-?\d+|signal=\S+|start|note) \S`)
	for _, l := range lines {
		if !assertLine.MatchString(l) {
			t.Errorf("a line isn't <time> <status> <command>: %q", l)
		}
	}
	state := logIndex(t, lines, "get-state")
	model := logIndex(t, lines, "getprop ro.product.model")
	logcat := logIndex(t, lines, "start adb -s "+serialPlaceholder+" logcat -b main,system,crash")
	scrcpy := logIndex(t, lines, "start scrcpy --serial="+serialPlaceholder)
	uname := logIndex(t, lines, "uname -r")
	end := logIndex(t, lines, "exit=0 scrcpy --serial=")
	if !(state < model && model < logcat && logcat < scrcpy && scrcpy < uname && uname < end) {
		t.Errorf("commands.log is out of order:\n%s", log)
	}
	// The app isn't installed: the shell command's status, under an adb that exits 0.
	if !strings.Contains(lines[logIndex(t, lines, "pm path dev.nickolaj.pocketnix;")], " exit=1 adb ") {
		t.Errorf("the app's pm path isn't logged with the shell's status:\n%s", log)
	}
	if !strings.Contains(lines[state], "adb -s "+serialPlaceholder+" get-state") {
		t.Errorf("get-state's line: %q", lines[state])
	}
	if strings.Contains(log, recordSerial) || strings.Contains(log, recordHWSerial) {
		t.Errorf("commands.log holds a serial:\n%s", log)
	}
}

func TestRecordTraceSeesEveryCall(t *testing.T) {
	recordDevice(t)
	type call struct {
		argv   string
		status int
		stdout string
	}
	var calls []call
	dev := adb.Device{Serial: recordSerial, Trace: func(argv []string, start time.Time, status int, stdout string) {
		if start.IsZero() {
			t.Errorf("%q has no start time", argv)
		}
		calls = append(calls, call{strings.Join(argv, " "), status, stdout})
	}}
	dev.Run("get-state")
	dev.Run("no-such-command")
	dev.Stream(io.Discard, "logcat", "-d")
	want := []call{
		{"adb -s " + recordSerial + " get-state", 0, "device\n"},
		{"adb -s " + recordSerial + " no-such-command", 1, ""},
		{"adb -s " + recordSerial + " logcat -d", 0, ""},
	}
	if !slices.Equal(calls, want) {
		t.Errorf("traced %+v, want %+v", calls, want)
	}
}

func TestRecordFindsTheSerialItself(t *testing.T) {
	d := recordDevice(t)
	d.write("logcat", []byte("1761999000.000  1234  1234 I Build   : serial "+recordHWSerial+" on "+fakeSerial+"\n"))
	dir := t.TempDir()
	if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) }); err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(filepath.Join(d.state, "scrcpy-args")); !strings.Contains(string(args), "--serial="+fakeSerial) {
		t.Errorf("scrcpy's args:\n%s", args)
	}
	assertNoSerial(t, onlyBundle(t, dir))
}

func TestRecordRedaction(t *testing.T) {
	r := newRedactor([]string{"", "unknown", "3A11", "adb-3A11-x._adb-tls-connect._tcp"})
	if got, want := r.Replace("os unknown on adb-3A11-x._adb-tls-connect._tcp, 3A11"), "os unknown on <serial>, <serial>"; got != want {
		t.Errorf("Replace = %q, want %q", got, want)
	}

	// A secret split across two writes, as a stream can split it.
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &redactLines{w: f, redact: r}
	w.Write([]byte("first 3A"))
	w.Write([]byte("11\nlast 3A11"))
	w.Close()
	if b, _ := os.ReadFile(path); string(b) != "first <serial>\nlast <serial>" {
		t.Errorf("wrote %q", b)
	}
}

func TestRecordSmoke(t *testing.T) {
	d := smokeDevice(t, everyTool...)
	d.setProp("ro.serialno", recordHWSerial)
	addFakeBin(t, "scrcpy")
	dir := t.TempDir()
	if _, err := stdoutOf(t, func() error {
		return runDoctor([]string{"--serial", recordSerial, "--smoke", "--gpu-apps", strings.Join(smokeApps, ","), "--record", dir})
	}); err != nil {
		t.Fatal(err)
	}
	bundle := onlyBundle(t, dir)
	smoke := readBundle(t, bundle, "smoke.txt")
	if !strings.Contains(smoke, "smoke: pty") || !regexp.MustCompile(`(?m)^  reboot +ok `).MatchString(smoke) {
		t.Errorf("smoke.txt:\n%s", smoke)
	}
	var r doctorReport
	if err := json.Unmarshal([]byte(readBundle(t, bundle, "doctor.json")), &r); err != nil {
		t.Fatal(err)
	}
	if st, ok := stepNamed(r.Smoke, "reboot"); !ok || st.OK == nil || !*st.OK {
		t.Errorf("doctor.json's reboot step: %+v", st)
	}
	if !exists(filepath.Join(bundle, "screen.mp4")) {
		t.Error("the bundle has no screen.mp4")
	}
	assertNoSerial(t, bundle)
	log := readBundle(t, bundle, "commands.log")
	if !strings.Contains(log, "am start") || !strings.Contains(log, "logcat -b main,system,crash --uid=") {
		t.Errorf("commands.log lacks smoke's commands:\n%s", log)
	}
}

func TestRecordWithoutScrcpy(t *testing.T) {
	d := newFakeDevice(t)
	cannedPixel(d)
	// The fake adb's directory only: a real scrcpy on PATH would otherwise run.
	t.Setenv("PATH", strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0])
	dir := t.TempDir()
	if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) }); err != nil {
		t.Errorf("a missing scrcpy failed the run: %v", err)
	}
	bundle := onlyBundle(t, dir)
	if exists(filepath.Join(bundle, "screen.mp4")) {
		t.Error("a screen.mp4 without scrcpy")
	}
	if log := readBundle(t, bundle, "commands.log"); !regexp.MustCompile(`(?m) note .*scrcpy`).MatchString(log) {
		t.Errorf("no note about scrcpy in commands.log:\n%s", log)
	}
	if !exists(filepath.Join(bundle, "doctor.json")) {
		t.Error("no doctor.json")
	}
	assertGone(t, d.state, "logcat.pid")
}

// A video exists only where scrcpy wrote some of it, whatever status it ended with.
func TestRecordScrcpyFails(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		video        *string
		kept         bool
	}{
		{name: "before-the-file", status: "1"},
		{name: "with-an-empty-file", status: "1", video: new("")},
		{name: "after-some-frames", status: "1", video: new("frames"), kept: true},
		{name: "phone-gone-after-some-frames", status: "2", video: new("frames"), kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := recordDevice(t)
			d.write("scrcpy-fails", []byte(tc.status))
			if tc.video != nil {
				d.write("scrcpy-video", []byte(*tc.video))
			}
			dir := t.TempDir()
			if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) }); err != nil {
				t.Errorf("a failing scrcpy failed the run: %v", err)
			}
			bundle := onlyBundle(t, dir)
			if got := exists(filepath.Join(bundle, "screen.mp4")); got != tc.kept {
				t.Errorf("screen.mp4 exists = %t, want %t", got, tc.kept)
			}
			log := readBundle(t, bundle, "commands.log")
			if !regexp.MustCompile(`(?m) exit=` + tc.status + ` scrcpy `).MatchString(log) || !regexp.MustCompile(`(?m) note .*Server connection failed`).MatchString(log) {
				t.Errorf("no note about scrcpy's failure in commands.log:\n%s", log)
			}
			if tc.kept && !regexp.MustCompile(`(?m) note .*cut short`).MatchString(log) {
				t.Errorf("no note that the kept video may be cut short:\n%s", log)
			}
		})
	}
}

// A smoke run that couldn't start exits 1 and still leaves its bundle, with the report.
func TestRecordSmokeThatCouldntRun(t *testing.T) {
	d := recordDevice(t)
	dir := t.TempDir()
	_, err := stdoutOf(t, func() error { return runDoctor([]string{"--smoke", "--record", dir}) })
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("run returned %v, want exit status 1", err)
	}
	var r doctorReport
	if err := json.Unmarshal([]byte(readBundle(t, onlyBundle(t, dir), "doctor.json")), &r); err != nil {
		t.Fatal(err)
	}
	if st, ok := stepNamed(r.Smoke, "pty"); !ok || st.OK != nil {
		t.Errorf("doctor.json's pty step: %+v, want ok null", st)
	}
	if ev := d.events(); hasEvent(ev, "am start") {
		t.Errorf("smoke acted on the device; events:\n%s", ev)
	}
}

func TestRecordScrcpyFlags(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		want, not  []string
	}{
		{"new", scrcpyHelpNew, []string{"--no-playback", "--no-window", "--no-audio", "--no-control", "--video-bit-rate=" + recordBitRate}, []string{"--no-display", "--bit-rate=" + recordBitRate}},
		{"old", scrcpyHelpOld, []string{"--no-display", "--no-control", "--bit-rate=" + recordBitRate}, []string{"--no-playback", "--no-window", "--no-audio", "--video-bit-rate=" + recordBitRate}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := recordDevice(t)
			d.write("scrcpy-help", []byte(tc.help))
			dir := t.TempDir()
			if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) }); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(d.state, "scrcpy-args"))
			args := strings.Split(string(b), "\n")
			for _, f := range tc.want {
				if !slices.Contains(args, f) {
					t.Errorf("no %s in %q", f, args)
				}
			}
			for _, f := range tc.not {
				if slices.Contains(args, f) {
					t.Errorf("%s in %q", f, args)
				}
			}
			if !slices.Contains(args, "--record="+filepath.Join(onlyBundle(t, dir), "screen.mp4")) {
				t.Errorf("no --record into the bundle in %q", args)
			}
		})
	}
}

func TestRecordScrcpyUsesTheRunsADB(t *testing.T) {
	d := recordDevice(t)
	want, err := exec.LookPath("adb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", t.TempDir()}) }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d.state, "scrcpy-adb")); string(b) != want {
		t.Errorf("scrcpy's ADB = %q, want %q", b, want)
	}
}

func TestRecordUnsafeModel(t *testing.T) {
	d := recordDevice(t)
	d.setProp("ro.product.model", "../../evil/.. x")
	dir := t.TempDir()
	if _, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) }); err != nil {
		t.Fatal(err)
	}
	if name := filepath.Base(onlyBundle(t, dir)); !regexp.MustCompile(`^\d{8}T\d{6}Z-evil-\.\.-x$`).MatchString(name) {
		t.Errorf("bundle name %q", name)
	}
}

func TestRecordNoDevice(t *testing.T) {
	d := recordDevice(t)
	d.write("get-state-fails", nil)
	dir := t.TempDir()
	_, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) })
	if err == nil {
		t.Fatal("a run without a device succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a run without a device left %v", entries)
	}
}

func TestRecordFailVerdictStillWritesTheBundle(t *testing.T) {
	d := recordDevice(t)
	d.can("pm has-feature android.software.virtualization_framework", "false\n", 1)
	dir := t.TempDir()
	_, err := stdoutOf(t, func() error { return runDoctor([]string{"--record", dir}) })
	var status exitStatus
	if !errors.As(err, &status) || status != 1 {
		t.Errorf("run returned %v, want exit status 1", err)
	}
	bundle := onlyBundle(t, dir)
	if !strings.Contains(readBundle(t, bundle, "doctor.json"), `"fail"`) {
		t.Error("doctor.json doesn't hold the fail")
	}
	assertGone(t, d.state, "scrcpy.pid")
}

// doctorProcess starts `doctor --smoke` with args in its own process, and returns
// it once smoke sits in its PTY step and ready holds.
func doctorProcess(t *testing.T, d *fakeDevice, ready func() bool, args ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command(addFakeBin(t, fakeDoctorName), append([]string{"--smoke", "--serial", recordSerial}, args...)...)
	c.Env = os.Environ()
	// A file: a hung fake adb outliving the run would hold a pipe open.
	stderr, err := os.Create(filepath.Join(d.state, "doctor-stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	c.Stderr = stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill() })
	deadline := time.Now().Add(20 * time.Second)
	// The console, the listener's tab and the PTY step's.
	for len(fakeTabs(d.state)) < 3 || !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("smoke didn't reach its PTY step: tabs %v", fakeTabs(d.state))
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

// interruptedRun is doctorProcess with --record into dir, once both children record.
func interruptedRun(t *testing.T, d *fakeDevice, dir string) *exec.Cmd {
	t.Helper()
	return doctorProcess(t, d, func() bool {
		return exists(filepath.Join(d.state, "scrcpy.pid")) && exists(filepath.Join(d.state, "logcat.pid"))
	}, "--record", dir)
}

// Without --record too, smoke cleans up after whatever signal ends a run.
func TestSmokeSignalsCleanUp(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			d := smokeDevice(t, without(everyTool, reactPTY)...)
			c := doctorProcess(t, d, func() bool { return true })
			c.Process.Signal(sig)
			c.Wait()
			if !c.ProcessState.Exited() {
				t.Errorf("the run: %v, want it to finish", c.ProcessState)
			}
			if left := leftovers(d); len(left) > 0 {
				t.Errorf("smoke left %q", left)
			}
		})
	}
}

func TestRecordInterruptFinishesTheBundle(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	addFakeBin(t, "scrcpy")
	dir := t.TempDir()
	c := interruptedRun(t, d, dir)
	c.Process.Signal(os.Interrupt)
	// Its status is 1 for the PTY step it cut short; a signal would mean it never finished.
	c.Wait()
	if !c.ProcessState.Exited() {
		t.Errorf("the interrupted run: %v", c.ProcessState)
	}
	assertGone(t, d.state, "scrcpy.pid")
	assertGone(t, d.state, "logcat.pid")
	if left := leftovers(d); len(left) > 0 {
		t.Errorf("smoke left %q", left)
	}
	bundle := onlyBundle(t, dir)
	if v := readBundle(t, bundle, "screen.mp4"); v != "frames+index" {
		t.Errorf("screen.mp4 = %q: scrcpy wasn't stopped with SIGINT", v)
	}
	var r doctorReport
	if err := json.Unmarshal([]byte(readBundle(t, bundle, "doctor.json")), &r); err != nil {
		t.Fatal(err)
	}
	if st, _ := stepNamed(r.Smoke, "reboot"); st.OK != nil || !strings.Contains(st.Detail, "interrupted") {
		t.Errorf("doctor.json's reboot step after the interrupt: %+v", st)
	}
	if !strings.Contains(readBundle(t, bundle, "smoke.txt"), "interrupted") {
		t.Error("smoke.txt doesn't say it was interrupted")
	}
	// The recording outlived the first signal, so that it covers smoke's cleanup.
	log := strings.Split(readBundle(t, bundle, "commands.log"), "\n")
	if logIndex(t, log, "--method close") > logIndex(t, log, "exit=0 scrcpy --serial=") {
		t.Error("scrcpy ended before smoke's cleanup")
	}
}

func TestRecordSecondInterruptStopsAtOnce(t *testing.T) {
	d := smokeDevice(t, without(everyTool, reactPTY)...)
	addFakeBin(t, "scrcpy")
	dir := t.TempDir()
	c := interruptedRun(t, d, dir)
	stderr := filepath.Join(d.state, "doctor-stderr")
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("no %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// The phone stops answering, so smoke's cleanup can't end: only the second signal can.
	hang := filepath.Join(d.state, "shell-hangs")
	d.write("shell-hangs", nil)
	t.Cleanup(func() {
		os.Remove(hang)
		assertGone(t, d.state, "shell-hung.pid")
	})
	waitFor("hung shell", func() bool { return exists(filepath.Join(d.state, "shell-hung.pid")) })
	c.Process.Signal(os.Interrupt)
	waitFor("message after the first signal", func() bool {
		b, _ := os.ReadFile(stderr)
		return strings.Contains(string(b), "interrupted")
	})
	if c.ProcessState != nil {
		t.Fatal("the run ended with the first signal")
	}
	c.Process.Signal(os.Interrupt)
	var exit *exec.ExitError
	if err := c.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Errorf("the run: %v, want exit status 130", err)
	}
	assertGone(t, d.state, "scrcpy.pid")
	assertGone(t, d.state, "logcat.pid")
	if v := readBundle(t, onlyBundle(t, dir), "screen.mp4"); v != "frames+index" {
		t.Errorf("screen.mp4 = %q: scrcpy wasn't stopped with SIGINT", v)
	}
}
