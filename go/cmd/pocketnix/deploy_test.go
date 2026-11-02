package main

import (
	"os"
	"path/filepath"
	"testing"
)

// failClosedGuest puts an ssh and a nix first on PATH that refuse everything,
// so no deploy test reaches a real guest or builds; A4a's fakes replace them
// with a model of the guest's drop-ins, GC roots and sudo.
func failClosedGuest(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"ssh", "nix"} {
		script := "#!/bin/sh\necho \"fake " + name + ": not modelled\" >&2\nexit 97\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// deployHost never resolves, should a test's fakes miss a call.
const deployHost = "pocketnix-test.invalid"

func TestDeployAgentAndRemove(t *testing.T) {
	failClosedGuest(t)
	t.Skip("pending: A4a")
	t.Run("leaves a drop-in and a root", func(t *testing.T) {
		t.Skip("pending: A4a")
		if err := runDeploy([]string{"--host", deployHost, "agent"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("warns about a leftover drop-in", func(t *testing.T) {
		t.Skip("pending: A4a")
	})
	t.Run("remove leaves neither", func(t *testing.T) {
		t.Skip("pending: A4a")
	})
	t.Run("refuses without passwordless sudo before any copy", func(t *testing.T) {
		t.Skip("pending: A4a")
	})
}
