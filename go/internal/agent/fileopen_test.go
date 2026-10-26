package agent_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpenReadsRanges(t *testing.T) {
	h, root, _, _ := startIntegrations(t, nil)
	data := []byte(strings.Repeat("0123456789", 300))
	must(t, os.WriteFile(filepath.Join(root, "f"), data, 0o644))
	must(t, os.Mkdir(filepath.Join(root, "dir"), 0o755))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644))
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))

	f, err := h.client.Open("f", "r")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Info.Size != int64(len(data)) {
		t.Errorf("size = %d", f.Info.Size)
	}
	if b, err := f.ReadAt(1003, 4); err != nil || string(b) != "3456" {
		t.Errorf("read at 1003 = %q, %v", b, err)
	}
	// Short at the end, empty past it.
	if b, err := f.ReadAt(2995, 100); err != nil || string(b) != "56789" {
		t.Errorf("read across the end = %q, %v", b, err)
	}
	if b, err := f.ReadAt(5000, 10); err != nil || len(b) != 0 {
		t.Errorf("read past the end = %q, %v", b, err)
	}
	if err := f.WriteAt(0, []byte("x")); err == nil {
		t.Error("wrote through a read-only open")
	}

	for _, p := range []string{"missing", "dir", "escape", "../" + filepath.Base(outside) + "/secret"} {
		if f, err := h.client.Open(p, "r"); err == nil {
			f.Close()
			t.Errorf("opened %s", p)
		}
	}
	if f, err := h.client.Open("escape", "w"); err == nil {
		f.WriteAt(0, []byte("yes"))
		f.Commit()
		t.Error("opened a symlink out of the root for writing")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "no" {
		t.Errorf("the file outside the root is now %q", b)
	}
	if f, err := h.client.Open("f", "rwx"); err == nil {
		f.Close()
		t.Error("accepted mode rwx")
	}
}

// temps lists the directory's leftover temporary files.
func temps(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if strings.Contains(de.Name(), ".pocketnix-") {
			out = append(out, de.Name())
		}
	}
	return out
}

func TestOpenWritesLandOnCommit(t *testing.T) {
	h, root, _, _ := startIntegrations(t, nil)
	h.srv.StateDir = t.TempDir()
	file := filepath.Join(root, "a.txt")
	must(t, os.WriteFile(file, []byte("alpha"), 0o600))

	// "rw" starts from the original, and its reads see its own writes.
	f, err := h.client.Open("a.txt", "rw")
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.WriteAt(2, []byte("XY")))
	must(t, f.WriteAt(7, []byte("!")))
	if b, err := f.ReadAt(0, 100); err != nil || string(b) != "alXYa\x00\x00!" {
		t.Errorf("read back %q, %v", b, err)
	}
	must(t, f.Sync())
	// Until the commit, everyone else sees the old file.
	if b, _ := os.ReadFile(file); string(b) != "alpha" {
		t.Errorf("before commit: %q", b)
	}
	must(t, f.Commit())
	if b, _ := os.ReadFile(file); string(b) != "alXYa\x00\x00!" {
		t.Errorf("after commit: %q", b)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode after commit = %v", fi.Mode().Perm())
	}

	// "w" truncates.
	f, err = h.client.Open("a.txt", "w")
	if err != nil || f.Info.Size != 0 {
		t.Fatalf("open w: %+v, %v", f, err)
	}
	if b, err := f.ReadAt(0, 10); err != nil || len(b) != 0 {
		t.Errorf("a truncating open reads %q, %v", b, err)
	}
	must(t, f.WriteAt(0, []byte("new")))
	must(t, f.Commit())
	if b, _ := os.ReadFile(file); string(b) != "new" {
		t.Errorf("after w: %q", b)
	}

	// An "rw" open that wrote nothing leaves the file untouched.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	must(t, os.Chtimes(file, old, old))
	f, err = h.client.Open("a.txt", "rw")
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.Commit())
	if fi, _ := os.Stat(file); !fi.ModTime().Equal(old) {
		t.Errorf("an unchanged open rewrote the file (mtime %v)", fi.ModTime())
	}

	// A new file, written with "w".
	f, err = h.client.Open("b.txt", "w")
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.WriteAt(0, []byte("bee")))
	must(t, f.Commit())
	if fi, err := os.Stat(filepath.Join(root, "b.txt")); err != nil || fi.Mode().Perm() != 0o644 || fi.Size() != 3 {
		t.Errorf("new file: %v, %v", fi, err)
	}

	if left := temps(t, root); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if des, _ := os.ReadDir(filepath.Join(h.srv.StateDir, "file-temps")); len(des) != 0 {
		t.Errorf("records left: %v", des)
	}
}

func TestOpenWithoutCommitDiscards(t *testing.T) {
	h, root, _, _ := startIntegrations(t, nil)
	h.srv.StateDir = t.TempDir()
	file := filepath.Join(root, "a.txt")
	must(t, os.WriteFile(file, []byte("alpha"), 0o644))

	f, err := h.client.Open("a.txt", "rw")
	if err != nil {
		t.Fatal(err)
	}
	must(t, f.WriteAt(0, []byte("ZZZZZZZZ")))
	if left := temps(t, root); len(left) != 1 {
		t.Errorf("temporary files while writing: %v", left)
	}
	f.Close()
	// The agent notices the closed stream on its own time.
	deadline := time.Now().Add(5 * time.Second)
	for len(temps(t, root)) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if left := temps(t, root); len(left) != 0 {
		t.Errorf("temporary files after an abort: %v", left)
	}
	if b, _ := os.ReadFile(file); string(b) != "alpha" {
		t.Errorf("an abort changed the file: %q", b)
	}
	des, _ := os.ReadDir(root)
	if names := func() (n []string) {
		for _, de := range des {
			n = append(n, de.Name())
		}
		return
	}(); !slices.Equal(names, []string{"a.txt"}) {
		t.Errorf("root holds %v", names)
	}
}
