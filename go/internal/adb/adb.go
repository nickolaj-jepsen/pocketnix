// Package adb drives one Android device through the adb binary on PATH.
package adb

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Device is one adb device; an empty Serial means adb's only device.
type Device struct {
	Serial string
	// Trace, when set, is called on the caller's goroutine as each Run or Stream
	// ends, also a failed one: the command's argv, when it started, its exit status
	// (-1 if it didn't start or a signal ended it), and the stdout Run kept (Stream
	// keeps none). Start's commands end out of sight, and aren't traced.
	Trace func(argv []string, start time.Time, status int, stdout string)
	// QuietErrors keeps a command's stdout out of the errors Run and Shell return,
	// for errors that go into a report: it can hold other apps' text. adb's own
	// stderr stays in them.
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

// Shell runs a command line in the device's shell. adb itself doesn't report
// a failing command through every Android version's adb, so this appends an
// exit marker and checks it.
func (d Device) Shell(cmdline string) (string, error) {
	out, err := d.Run("shell", cmdline+"; echo pocketnix-exit=$?")
	if err != nil {
		return out, err
	}
	body, status, ok := cutLast(out, "pocketnix-exit=")
	if !ok {
		return out, fmt.Errorf("adb shell %s: no exit status in output:\n%s", cmdline, d.inError(out))
	}
	if strings.TrimSpace(status) != "0" {
		return body, fmt.Errorf("adb shell %s: exit %s:\n%s", cmdline, strings.TrimSpace(status), d.inError(body))
	}
	return body, nil
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
