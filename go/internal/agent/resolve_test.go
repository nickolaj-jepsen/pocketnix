package agent_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePath(t *testing.T) {
	h, root, _, _ := startIntegrations(t, nil)
	must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "docs", "a b.txt"), []byte("x"), 0o644))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644))
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()

	if rel, err := ctl.ResolvePath(filepath.Join(root, "docs", "a b.txt")); err != nil || rel != "docs/a b.txt" {
		t.Errorf("inside the root = %q, %v", rel, err)
	}
	for _, p := range []string{
		filepath.Join(root, "..", filepath.Base(outside), "secret"),
		filepath.Join(root, "escape"),
		"docs/a b.txt",
	} {
		if rel, err := ctl.ResolvePath(p); err == nil {
			t.Errorf("%s resolved to %q", p, rel)
		}
	}
}
