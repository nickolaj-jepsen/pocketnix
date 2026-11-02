package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const (
	// serialPlaceholder stands for a serial anywhere in a bundle.
	serialPlaceholder = "<serial>"
	recordTimeFormat  = "2006-01-02T15:04:05.000Z"
	// recordStopGrace is how long a stopped child gets to finish: scrcpy writes
	// the index that makes its mp4 playable only then.
	recordStopGrace = 15 * time.Second
	// recordTailBytes is how much of scrcpy's stderr a note about it quotes.
	recordTailBytes = 300
	// recordModelLen caps the device's model in a bundle's name.
	recordModelLen = 64

	// A screen of mostly still text. The rate keeps a rented session's whole
	// video under the volume that drops a bulk adb transfer (AGENTS.md's USB
	// trap); the size and frame rate cut what the phone spends encoding.
	recordBitRate = "1M"
	recordMaxSize = "1280"
	recordMaxFPS  = "15"
)

// shellExit is a shell command's status at the end of its stdout.
var shellExit = regexp.MustCompile(regexp.QuoteMeta(shellExitMarker) + `(\d+)\s*$`)

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// newRedactor replaces each secret with serialPlaceholder, the longest first: a
// network serial can hold the hardware one. Empty and "unknown" (some builds'
// ro.serialno, and a word doctor.json uses) are no secrets.
func newRedactor(secrets []string) *strings.Replacer {
	secrets = slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return s == "" || s == "unknown" })
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	var pairs []string
	for _, s := range secrets {
		pairs = append(pairs, s, serialPlaceholder)
	}
	return strings.NewReplacer(pairs...)
}

// appendLog adds a line to the commands.log at path. Each line is one O_APPEND
// write, so the run and its children's goroutines can't interleave within one.
func appendLog(path string, redact *strings.Replacer, at time.Time, kind, text string) error {
	text = strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(redact.Replace(text))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s %s\n", at.UTC().Format(recordTimeFormat), kind, text)
	return err
}

// redactLines writes whole lines to w with secrets replaced, so a secret
// split across two writes is still found. Close writes what's left.
type redactLines struct {
	w      io.WriteCloser
	redact *strings.Replacer
	buf    []byte
}

func (l *redactLines) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	i := bytes.LastIndexByte(l.buf, '\n')
	if i < 0 {
		return len(p), nil
	}
	_, err := io.WriteString(l.w, l.redact.Replace(string(l.buf[:i+1])))
	l.buf = append(l.buf[:0], l.buf[i+1:]...)
	return len(p), err
}

func (l *redactLines) Close() error {
	_, err := io.WriteString(l.w, l.redact.Replace(string(l.buf)))
	l.buf = nil
	return errors.Join(err, l.w.Close())
}

// recording is one doctor run's bundle (C4), and the children filling it.
type recording struct {
	// dev is the device to record through: its commands go into commands.log.
	dev      adb.Device
	dir, log string
	secrets  []string
	redact   *strings.Replacer
	// pending is what's logged before the bundle exists: its name needs the model.
	pending []logLine
	// ctx ends at the first signal, for what can clean up after itself.
	ctx     context.Context
	cancel  context.CancelFunc
	sigs    chan os.Signal
	stopped chan struct{}
	once    sync.Once
	logcat  *recordChild
	scrcpy  *recordChild
	logFile io.Closer
	smoke   io.WriteCloser
}

// logLine is a line of commands.log; see appendLog.
type logLine struct {
	at         time.Time
	kind, text string
}

// recordChild is a long-running process of a recording, in its own process
// group: a terminal's Ctrl-C leaves it running, so that it covers smoke's cleanup.
type recordChild struct {
	cmd    *exec.Cmd
	stop   context.CancelFunc
	done   chan struct{}
	stderr tailWriter
	// status is its exit status, -1 for a signal; result is the same for the log.
	status int
	result string
}

// startRecording creates a bundle under parent and starts recording dev into
// it, once dev answers: without a device there's no bundle, and an error. Its
// dev is the one to run commands on, and its stop ends the recording, and must
// run; signals end it too (see watchSignals).
func startRecording(parent string, dev adb.Device, smoke bool) (_ *recording, err error) {
	realADB, err := exec.LookPath("adb")
	if err != nil {
		return nil, err
	}
	r := &recording{stopped: make(chan struct{}), sigs: make(chan os.Signal, 2)}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.addSecrets(dev.Serial)
	dev.Trace = r.trace
	r.dev = dev
	if err := usableDevice(dev); err != nil {
		return nil, err
	}
	serial := dev.Serial
	if serial == "" {
		out, err := dev.Run("get-serialno")
		if err != nil {
			return nil, err
		}
		serial = strings.TrimSpace(out)
		r.addSecrets(serial)
	}
	// Over the network the adb serial is an address, and logcat can still hold this one.
	r.addSecrets(deref(getprop(dev, "ro.serialno")))
	name := time.Now().UTC().Format("20060102T150405Z") + "-" + bundleModel(getprop(dev, "ro.product.model"))

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	final := filepath.Join(parent, name)
	// Only its owner may read it (B2).
	if err := os.Mkdir(final, 0o700); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			r.stop()
			os.RemoveAll(final)
		}
	}()
	r.dir, r.log = final, filepath.Join(final, "commands.log")
	for _, l := range r.pending {
		r.logLine(l.at, l.kind, l.text)
	}
	r.pending = nil
	r.watchSignals()

	if err := os.WriteFile(filepath.Join(final, "README.txt"), []byte(recordReadme(smoke, r.revision())), 0o600); err != nil {
		return nil, err
	}
	if smoke {
		f, err := os.OpenFile(filepath.Join(final, "smoke.txt"), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		r.smoke = &redactLines{w: f, redact: r.redact}
	}
	if err := r.startLogcat(realADB, serial); err != nil {
		return nil, err
	}
	r.startScrcpy(realADB, serial)
	return r, nil
}

// trace logs a command of dev's, with the shell command's own status when its
// stdout ends in the marker: adb itself exits 0 then.
func (r *recording) trace(argv []string, start time.Time, status int, stdout string) {
	kind := "exit=" + strconv.Itoa(status)
	if m := shellExit.FindStringSubmatch(stdout[max(0, len(stdout)-len(shellExitMarker)-16):]); m != nil && status == 0 {
		kind = "exit=" + m[1]
	}
	r.logLine(start, kind, strings.Join(argv, " "))
}

// logLine adds a line to commands.log, or keeps it until the bundle exists.
func (r *recording) logLine(at time.Time, kind, text string) {
	if r.log == "" {
		r.pending = append(r.pending, logLine{at, kind, text})
		return
	}
	if err := appendLog(r.log, r.redact, at, kind, text); err != nil {
		fmt.Fprintln(os.Stderr, "doctor: commands.log:", err)
	}
}

// usableDevice errors unless adb reaches dev.
func usableDevice(dev adb.Device) error {
	if state, err := dev.State(); err != nil || state != "device" {
		return fmt.Errorf("no usable adb device (state %q): %v", state, err)
	}
	return nil
}

// watchSignals makes the first signal end ctx, while the children keep
// recording, and a second one stop them and exit at once.
func (r *recording) watchSignals() {
	signal.Notify(r.sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-r.sigs:
			r.cancel()
		case <-r.stopped:
			return
		}
		fmt.Fprintln(os.Stderr, "doctor: interrupted; finishing the recording (interrupt again to stop now)")
		select {
		case <-r.sigs:
			r.note("interrupted twice: stopping now")
			r.stop()
			os.Exit(130)
		case <-r.stopped:
		}
	}()
}

// addSecrets has the bundle's files replace s from now on.
func (r *recording) addSecrets(s ...string) {
	r.secrets = append(r.secrets, s...)
	r.redact = newRedactor(r.secrets)
}

// note says something about the recording in commands.log and on stderr.
func (r *recording) note(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "doctor: "+r.redact.Replace(text))
	r.logLine(time.Now(), "note", text)
}

// start starts a child with these streams and logs it; it logs its end itself.
func (r *recording) start(name string, env []string, stdout io.Writer, path string, args ...string) (*recordChild, error) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &recordChild{stop: cancel, done: make(chan struct{}), stderr: tailWriter{max: recordTailBytes}}
	c.cmd = exec.CommandContext(ctx, path, args...)
	c.cmd.Cancel = func() error { return c.cmd.Process.Signal(os.Interrupt) }
	c.cmd.WaitDelay = recordStopGrace
	c.cmd.Env = env
	c.cmd.Stdout, c.cmd.Stderr = stdout, &c.stderr
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The bundle's own path can name the desktop's user.
	command := strings.ReplaceAll(name+" "+strings.Join(args, " "), r.dir, "<bundle>")
	if err := c.cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	r.logLine(time.Now(), "start", command)
	go func() {
		defer close(c.done)
		c.cmd.Wait()
		cancel()
		c.status, c.result = c.cmd.ProcessState.ExitCode(), "exit="+strconv.Itoa(c.cmd.ProcessState.ExitCode())
		if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			c.result = "signal=" + strings.ReplaceAll(ws.Signal().String(), " ", "-")
		}
		r.logLine(time.Now(), c.result, command)
	}()
	return c, nil
}

// startLogcat streams the phone's whole log into logcat.txt: what the buffers
// hold, so that a failure from before the run is in the bundle, and then what comes.
func (r *recording) startLogcat(realADB, serial string) error {
	f, err := os.OpenFile(filepath.Join(r.dir, "logcat.txt"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := &redactLines{w: f, redact: r.redact}
	r.logFile = w
	r.logcat, err = r.start("adb", nil, w, realADB, "-s", serial, "logcat", "-b", "main,system,crash")
	if err != nil {
		r.note("no logcat.txt lines: adb logcat didn't start: %v", err)
	}
	return nil
}

// scrcpyArgs is scrcpy's command line recording to out, with the flags the
// help it printed has. No control, as that would sync the clipboards; no audio,
// which the bundle has no use for and could hold a call.
func scrcpyArgs(help, serial, out string) []string {
	has := func(flag string) bool {
		// A whole word: --no-window-aspect-ratio-lock is older than --no-window.
		return regexp.MustCompile(`(?m)(^|[\s,])` + regexp.QuoteMeta(flag) + `(=|\s|$)`).MatchString(help)
	}
	args := []string{"--serial=" + serial, "--no-control", "--max-size=" + recordMaxSize, "--max-fps=" + recordMaxFPS}
	if has("--no-playback") {
		args = append(args, "--no-playback")
	} else {
		args = append(args, "--no-display")
	}
	// With --no-playback alone, a scrcpy that has this still opens a window.
	if has("--no-window") {
		args = append(args, "--no-window")
	}
	if has("--no-audio") {
		args = append(args, "--no-audio")
	}
	if has("--video-bit-rate") {
		args = append(args, "--video-bit-rate="+recordBitRate)
	} else {
		args = append(args, "--bit-rate="+recordBitRate)
	}
	return append(args, "--record="+out)
}

// startScrcpy starts the screen recording; without it the run goes on, with a note.
func (r *recording) startScrcpy(realADB, serial string) {
	path, err := exec.LookPath("scrcpy")
	if err != nil {
		r.note("no screen.mp4: scrcpy isn't on PATH")
		return
	}
	at := time.Now()
	probe := exec.Command(path, "--help")
	help, err := probe.CombinedOutput()
	r.logLine(at, "exit="+strconv.Itoa(probe.ProcessState.ExitCode()), "scrcpy --help")
	if err != nil {
		r.note("no screen.mp4: scrcpy --help: %v: %s", err, lastBytes(help, recordTailBytes))
		return
	}
	// Its wrapper may put another adb first on PATH, and a client of another
	// version restarts the server, which would end this run's other adb streams.
	env := append(os.Environ(), "ADB="+realADB)
	args := scrcpyArgs(string(help), serial, filepath.Join(r.dir, "screen.mp4"))
	if r.scrcpy, err = r.start("scrcpy", env, nil, path, args...); err != nil {
		r.note("no screen.mp4: scrcpy didn't start: %v", err)
	}
}

func lastBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

// stop ends the children, scrcpy first so that the log covers its end, and
// settles screen.mp4. Later calls wait for the first.
func (r *recording) stop() {
	r.once.Do(func() {
		signal.Stop(r.sigs)
		close(r.stopped)
		r.cancel()
		if r.scrcpy != nil {
			r.scrcpy.stop()
			<-r.scrcpy.done
			r.settleVideo()
		}
		if r.logcat != nil {
			r.logcat.stop()
			<-r.logcat.done
		}
		for _, c := range []io.Closer{r.logFile, r.smoke} {
			if c != nil {
				c.Close()
			}
		}
	})
}

// settleVideo leaves screen.mp4 only if scrcpy got as far as recording, and
// notes what's wrong with it. A status of 2 is a phone gone mid-run: scrcpy
// still closes the file then, so the video up to there stays.
func (r *recording) settleVideo() {
	c := r.scrcpy
	video := filepath.Join(r.dir, "screen.mp4")
	stderr := lastBytes(c.stderr.b, recordTailBytes)
	fi, err := os.Stat(video)
	switch empty := err != nil || fi.Size() == 0; {
	case c.status == 1 || empty:
		os.Remove(video)
		r.note("no screen.mp4: scrcpy ended with %s: %s", c.result, stderr)
	case c.status != 0:
		r.note("screen.mp4 may be cut short or not play: scrcpy ended with %s: %s", c.result, stderr)
	}
}

// bundleModel is a device's model made safe for a file name: it's the
// device's word, and may hold anything.
func bundleModel(model *string) string {
	s := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(deref(model), "-")
	if len(s) > recordModelLen {
		s = s[:recordModelLen]
	}
	if s = strings.Trim(s, "-."); s != "" {
		return s
	}
	return "unknown"
}

// writeReport adds the report to the bundle: doctor.json, and smoke's results
// to smoke.txt. A failure is noted: the run's result stands without them.
func (r *recording) writeReport(rep doctorReport) {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(r.dir, "doctor.json"), []byte(r.redact.Replace(string(b))+"\n"), 0o600)
	}
	if err != nil {
		r.note("no doctor.json: %v", err)
	}
	if r.smoke != nil {
		printSmoke(r.smoke, rep.Smoke)
	}
}

// smokeProgress is where smoke's progress goes: to stderr as usual, and smoke.txt.
func (r *recording) smokeProgress() io.Writer { return io.MultiWriter(os.Stderr, r.smoke) }

// revision is the git revision this CLI was built from. Go stamps none in a
// linked worktree or a Nix build, so then it's the run's directory's, and says so.
func (r *recording) revision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		rev, modified := "", ""
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision":
				rev = s.Value
			case s.Key == "vcs.modified" && s.Value == "true":
				modified = " (with uncommitted changes)"
			}
		}
		if rev != "" {
			return rev + modified
		}
	}
	at := time.Now()
	c := exec.Command("git", "rev-parse", "HEAD")
	out, err := c.Output()
	r.logLine(at, "exit="+strconv.Itoa(c.ProcessState.ExitCode()), "git rev-parse HEAD")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out)) + " (of the directory the run started in)"
}

// recordReadme is the bundle's README.txt.
func recordReadme(smoke bool, revision string) string {
	smokeLine := ""
	if smoke {
		smokeLine = "smoke.txt     --smoke's progress and results\n"
	}
	return `A recording of a pocketnix doctor run.

Review this bundle before sharing it. It stays on this computer unless you
send it somewhere. screen.mp4 can show notifications, whatever was on screen
and the guest's terminal. logcat.txt is the phone's whole log, from every app,
and can hold account names, a phone number and message text. Only the device's
serial is taken out of the files, and the video isn't filtered.

commands.log  every command the run sent, in order: start time (UTC), exit
              status, command. A shell command's status is its own, not adb's.
              A serial is written as ` + serialPlaceholder + `. scrcpy's own adb calls
              aren't in it.
doctor.json   the doctor report
` + smokeLine + `logcat.txt    adb logcat -b main,system,crash: the buffers as they were at the
              start, then every line until the run ended
screen.mp4    scrcpy's video of the screen; if it's absent, commands.log's
              notes say why

git revision: ` + revision + `
CLI version:  ` + version + `
`
}
