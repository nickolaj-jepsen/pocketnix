package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The agent reads the user's history file as root: what the user puts at
// that path mustn't leak root's files, hang the agent or exhaust its memory.
func TestHistoryFileIsConfinedToTheHome(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "shadow")
	if err := os.WriteFile(secret, []byte("root:$y$secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	read := func(target string) error {
		os.Remove(filepath.Join(home, ".bash_history"))
		if err := os.Symlink(target, filepath.Join(home, ".bash_history")); err != nil {
			t.Fatal(err)
		}
		f, err := openInHome(home, ".bash_history")
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = readTail(f, historyTail)
		return err
	}

	if err := read(secret); err == nil {
		t.Error("read through a symlink out of the home")
	}
	if err := read("/dev/zero"); err == nil {
		t.Error("read /dev/zero")
	}

	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- read(fifo) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("read a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a FIFO")
	}

	// A relative symlink that stays in the home is fine (os.Root takes any absolute one as an escape).
	if err := os.WriteFile(filepath.Join(home, "real"), []byte("ls\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := read("real"); err != nil {
		t.Errorf("symlink within the home: %v", err)
	}
}
