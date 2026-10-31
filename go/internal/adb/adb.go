// Package adb drives one Android device through the adb binary on PATH.
package adb

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Device is one adb device; an empty Serial means adb's only device.
type Device struct {
	Serial string
}

func (d Device) cmd(args ...string) *exec.Cmd {
	if d.Serial != "" {
		args = append([]string{"-s", d.Serial}, args...)
	}
	return exec.Command("adb", args...)
}

// Run runs an adb command and returns its stdout.
func (d Device) Run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := d.cmd(args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("adb %s: %w\n%s%s", strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String(), nil
}

// Stream runs an adb command with its stdout going to w as it comes, until
// it exits.
func (d Device) Stream(w io.Writer, args ...string) error {
	var errb bytes.Buffer
	c := d.cmd(args...)
	c.Stdout, c.Stderr = w, &errb
	if err := c.Run(); err != nil {
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
		return out, fmt.Errorf("adb shell %s: no exit status in output:\n%s", cmdline, out)
	}
	if strings.TrimSpace(status) != "0" {
		return body, fmt.Errorf("adb shell %s: exit %s:\n%s", cmdline, strings.TrimSpace(status), body)
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
