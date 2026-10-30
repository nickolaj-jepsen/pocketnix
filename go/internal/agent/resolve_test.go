package agent_test

import (
	"os"
	"path/filepath"
	"strings"
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
	up, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		// Joined by hand: filepath.Join would clean the ".." away before it's sent.
		root + "/" + up + "/secret",
		filepath.Join(root, "escape"),
	} {
		if rel, err := ctl.ResolvePath(p); err == nil || !strings.Contains(err.Error(), "files.root") {
			t.Errorf("%s = %q, %v; want refused as outside files.root", p, rel, err)
		}
	}
	if rel, err := ctl.ResolvePath("docs/a b.txt"); err == nil {
		t.Errorf("a relative path resolved to %q", rel)
	}
}
