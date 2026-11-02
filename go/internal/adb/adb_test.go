package adb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// private stands for text of other apps' in a shell command's stdout.
const private = "private-text"

// adbAnswering puts an adb first on PATH that prints stdout, complains on
// stderr and exits with status.
func adbAnswering(t *testing.T, stdout string, status int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\necho adb-complaint >&2\nexit %d\n", ShellQuote(stdout), status)
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestShellStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout     string
		adbStatus  int
		wantOut    string
		wantStatus int
		wantErr    bool
	}{
		"exit-0":      {stdout: "hello\n" + ShellExitMarker + "0\n", wantOut: "hello\n"},
		"exit-3":      {stdout: "hello\n" + ShellExitMarker + "3\n", wantOut: "hello\n", wantStatus: 3},
		"last-marker": {stdout: ShellExitMarker + "9\n" + ShellExitMarker + "1\n", wantOut: ShellExitMarker + "9\n", wantStatus: 1},
		"adb-fails":   {stdout: "hello", adbStatus: 1, wantOut: "hello", wantStatus: -1, wantErr: true},
		"no-marker":   {stdout: "hello", wantOut: "hello", wantStatus: -1, wantErr: true},
		"no-number":   {stdout: "hello\n" + ShellExitMarker + "oops\n", wantOut: "hello\n", wantStatus: -1, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			adbAnswering(t, tc.stdout, tc.adbStatus)
			out, status, err := Device{}.ShellStatus("cmd")
			if out != tc.wantOut || status != tc.wantStatus || (err != nil) != tc.wantErr {
				t.Errorf("got %q, status %d, error %v", out, status, err)
			}
		})
	}
}

func TestShellFailsOnAnyStatusButZero(t *testing.T) {
	adbAnswering(t, "hello\n"+ShellExitMarker+"0\n", 0)
	if out, err := (Device{}).Shell("cmd"); err != nil || out != "hello\n" {
		t.Errorf("exit 0: got %q, %v", out, err)
	}
	adbAnswering(t, "hello\n"+ShellExitMarker+"3\n", 0)
	out, err := Device{}.Shell("cmd")
	if err == nil || !strings.Contains(err.Error(), "3") || out != "hello\n" {
		t.Errorf("exit 3: got %q, %v", out, err)
	}
}

// Each way a shell command fails: its stdout is in Shell's error only when the
// device isn't quiet, and in ShellStatus's never when it is.
func TestQuietErrorsKeepStdoutOut(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout    string
		adbStatus int
		// shellErrs is whether Shell fails, statusErrs whether ShellStatus does.
		shellErrs, statusErrs bool
	}{
		"adb-fails":     {stdout: private, adbStatus: 1, shellErrs: true, statusErrs: true},
		"exit-1":        {stdout: private + "\n" + ShellExitMarker + "1\n", shellErrs: true},
		"no-marker":     {stdout: private, shellErrs: true, statusErrs: true},
		"late-output":   {stdout: ShellExitMarker + "0\n" + private + "\n", shellErrs: true, statusErrs: true},
		"marker-glued":  {stdout: private + ShellExitMarker + private, shellErrs: true, statusErrs: true},
		"before-status": {stdout: private + "\n" + ShellExitMarker + "0\n"},
	} {
		t.Run(name, func(t *testing.T) {
			adbAnswering(t, tc.stdout, tc.adbStatus)
			quiet := Device{QuietErrors: true}
			_, err := quiet.Shell("cmd")
			if (err != nil) != tc.shellErrs {
				t.Fatalf("Shell: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), private) {
				t.Errorf("Shell on a quiet device quotes stdout: %v", err)
			}
			_, _, err = quiet.ShellStatus("cmd")
			if (err != nil) != tc.statusErrs {
				t.Fatalf("ShellStatus: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), private) {
				t.Errorf("ShellStatus on a quiet device quotes stdout: %v", err)
			}
		})
	}

	// adb's own complaint stays, and a device that isn't quiet shows what ran.
	adbAnswering(t, private, 1)
	if _, err := (Device{QuietErrors: true}).Shell("cmd"); err == nil || !strings.Contains(err.Error(), "adb-complaint") {
		t.Errorf("a quiet device lost adb's complaint: %v", err)
	}
	for name, stdout := range map[string]string{"exit-1": private + "\n" + ShellExitMarker + "1\n", "no-marker": private} {
		adbAnswering(t, stdout, 0)
		if _, err := (Device{}).Shell("cmd"); err == nil || !strings.Contains(err.Error(), private) {
			t.Errorf("%s: Shell's error lacks stdout: %v", name, err)
		}
	}
}

// Trace sees what adb did: adb's status and raw stdout, the marker's line included,
// from which a log can read the shell command's own status.
func TestTraceSeesShellOutputRaw(t *testing.T) {
	raw := "hello\n" + ShellExitMarker + "3\n"
	adbAnswering(t, raw, 0)
	var statuses []int
	var stdouts []string
	dev := Device{Trace: func(_ []string, _ time.Time, status int, stdout string) {
		statuses, stdouts = append(statuses, status), append(stdouts, stdout)
	}}
	dev.Shell("cmd")
	dev.ShellStatus("cmd")
	if len(stdouts) != 2 {
		t.Fatalf("traced %d calls of 2", len(stdouts))
	}
	for i := range stdouts {
		if statuses[i] != 0 || stdouts[i] != raw {
			t.Errorf("call %d: status %d, stdout %q", i, statuses[i], stdouts[i])
		}
	}
}
