package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSweepTempsRemovesWhatACrashLeft(t *testing.T) {
	dir, files := t.TempDir(), t.TempDir()
	cfg := filepath.Join(dir, "agent.json")
	b, _ := json.Marshal(map[string]any{"shell": "/bin/sh", "filesRoot": files})
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.StateDir = filepath.Join(dir, "state")
	if err := os.Mkdir(filepath.Join(files, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := s.openFilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	// A write the crash interrupted: its temp and its record stay.
	f, tmp, _, err := s.newTemp(root, "sub/doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// A record that names something that isn't a temp, and one for another root.
	for _, name := range []string{"keep.txt", "sub/.other.pocketnix-1"} {
		if err := os.WriteFile(filepath.Join(files, name), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, rec := range []tempRecord{{Root: root.Name(), Path: "keep.txt"}, {Root: "/elsewhere", Path: "sub/.other.pocketnix-1"}} {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(s.tempsDir(), "x"+string(rune('0'+i))+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root.Close()

	s.SweepTemps()
	if _, err := os.Stat(filepath.Join(files, filepath.FromSlash(tmp))); !os.IsNotExist(err) {
		t.Errorf("the temp survived the sweep: %v", err)
	}
	for _, name := range []string{"keep.txt", "sub/.other.pocketnix-1"} {
		if _, err := os.Stat(filepath.Join(files, name)); err != nil {
			t.Errorf("the sweep removed %s, which isn't this root's temp: %v", name, err)
		}
	}
	if des, _ := os.ReadDir(s.tempsDir()); len(des) != 0 {
		t.Errorf("records left after the sweep: %v", des)
	}
}
