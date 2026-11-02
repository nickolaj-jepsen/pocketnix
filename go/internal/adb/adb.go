// Package adb drives one Android device through the adb binary on PATH.
package adb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ShellExitMarker comes before a shell command's exit status in the stdout of
// ShellStatus's adb command, which is the stdout Trace gets.
const ShellExitMarker = "pocketnix-exit="

// Device is one adb device; an empty Serial means adb's only device.
type Device struct {
	Serial string
	// Trace, when set, is called on the caller's goroutine as each Run or Stream
	// ends, also a failed one: the command's argv, when it started, its exit status
	// (-1 if it didn't start or a signal ended it), and the stdout Run kept (Stream
	// keeps none). Start's commands end out of sight, and aren't traced.
	Trace func(argv []string, start time.Time, status int, stdout string)
	// QuietErrors keeps a command's stdout out of the errors Run, Shell and
	// ShellStatus return, for errors that go into a report: it can hold other apps'
	// text. adb's own stderr stays in them.
	QuietErrors bool
}

func (d Device) cmd(args ...string) *exec.Cmd {
	if d.Serial != "" {
		args = append([]string{"-s", d.Serial}, args...)
	}
	return exec.Command("adb", args...)
}

func (d Device) trace(c *exec.Cmd, start time.Time, stdout string) {
	if d.Trace != nil {
		// A nil ProcessState's ExitCode is -1 too.
		d.Trace(c.Args, start, c.ProcessState.ExitCode(), stdout)
	}
}

// Run runs an adb command and returns its stdout.
func (d Device) Run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := d.cmd(args...)
	c.Stdout, c.Stderr = &out, &errb
	start := time.Now()
	err := c.Run()
	d.trace(c, start, out.String())
	if err != nil {
		return out.String(), fmt.Errorf("adb %s: %w\n%s%s", strings.Join(args, " "), err, d.inError(out.String()), errb.String())
	}
	return out.String(), nil
}

// inError is what an error may quote of a command's stdout.
func (d Device) inError(stdout string) string {
	if d.QuietErrors {
		return ""
	}
	return stdout
}

// Stream runs an adb command with its stdout going to w as it comes, until
// it exits.
func (d Device) Stream(w io.Writer, args ...string) error {
	var errb bytes.Buffer
	c := d.cmd(args...)
	c.Stdout, c.Stderr = w, &errb
	start := time.Now()
	err := c.Run()
	d.trace(c, start, "")
	if err != nil {
		return fmt.Errorf("adb %s: %w\n%s", strings.Join(args, " "), err, errb.String())
	}
	return nil
}

// Start starts an adb command with its stdout going to w, and returns at
// once; killing the returned command ends it.
func (d Device) Start(w io.Writer, args ...string) (*exec.Cmd, error) {
	c := d.cmd(args...)
	c.Stdout = w
	return c, c.Start()
}

var errNoStatus = errors.New("no exit status in output")

// ShellStatus runs a command line in the device's shell and returns its stdout
// and exit status. adb itself doesn't report a failing command through every
// Android version's adb, so this appends an exit marker and reads it. An error
// is adb's own failure, or output without a readable status, and never the
// command's: its status is -1 then, with what stdout there is.
func (d Device) ShellStatus(cmdline string) (string, int, error) {
	out, err := d.Run("shell", cmdline+"; echo "+ShellExitMarker+"$?")
	if err != nil {
		return out, -1, err
	}
	body, tail, ok := cutLast(out, ShellExitMarker)
	if !ok {
		return out, -1, fmt.Errorf("adb shell %s: %w", cmdline, errNoStatus)
	}
	status, err := strconv.Atoi(strings.TrimSpace(tail))
	if err != nil {
		return body, -1, fmt.Errorf("adb shell %s: exit status %q", cmdline, d.inError(tail))
	}
	return body, status, nil
}

// Shell is ShellStatus with a command that exits non-zero as an error too.
func (d Device) Shell(cmdline string) (string, error) {
	out, status, err := d.ShellStatus(cmdline)
	switch {
	case errors.Is(err, errNoStatus):
		return out, fmt.Errorf("%w:\n%s", err, d.inError(out))
	case err != nil:
		return out, err
	case status != 0:
		return out, fmt.Errorf("adb shell %s: exit %d:\n%s", cmdline, status, d.inError(out))
	}
	return out, nil
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// State is the device's adb state ("device" when usable).
func (d Device) State() (string, error) {
	out, err := d.Run("get-state")
	return strings.TrimSpace(out), err
}

// ShellQuote quotes s for the device's sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
