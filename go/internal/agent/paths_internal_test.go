package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// asRoot makes configured-path checks run as a root agent would, with this
// test's own user as the untrusted one: everything it creates belongs to
// "the session user".
func asRoot(t *testing.T) {
	t.Helper()
	me := uint32(os.Getuid())
	oldConfine, oldTrusted := confine, trustedOwner
	confine, trustedOwner = true, func(uid uint32) bool { return uid != me }
	t.Cleanup(func() { confine, trustedOwner = oldConfine, oldTrusted })
}

func TestConfiguredPathInTheHomeStaysThere(t *testing.T) {
	home := t.TempDir()
	if err := os.Symlink("/", filepath.Join(home, "shared")); err != nil {
		t.Fatal(err)
	}
	if r, err := configuredDir(home, filepath.Join(home, "shared"), false, nil); err == nil {
		r.Close()
		t.Error("a symlink in the home opened /")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(home, "history")); err != nil {
		t.Fatal(err)
	}
	if f, err := configuredFile(home, filepath.Join(home, "history")); err == nil {
		f.Close()
		t.Error("a symlink in the home opened /etc/passwd")
	}

	r, err := configuredDir(home, filepath.Join(home, "inbox", "new"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if fi, err := os.Lstat(filepath.Join(home, "inbox", "new")); err != nil || !fi.IsDir() {
		t.Errorf("not made in the home: %v", err)
	}
}

func TestConfiguredPathOutsideTheHomeNeedsRoot(t *testing.T) {
	asRoot(t)
	userDir := t.TempDir() // the "user's"
	link := filepath.Join(userDir, "link")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{userDir, link, filepath.Join(link, "etc")} {
		if _, err := trustedPath(p, false); err == nil {
			t.Errorf("%s: accepted a path the user controls", p)
		}
	}
	if _, err := trustedPath(filepath.Join(userDir, "new"), true); err == nil {
		t.Error("made a directory inside one the user owns")
	}
	if _, err := os.Stat(filepath.Join(userDir, "new")); err == nil {
		t.Error("made it anyway")
	}
	if f, err := configuredFile("/nonexistent-home", filepath.Join(link, "etc", "passwd")); err == nil {
		f.Close()
		t.Error("opened /etc/passwd through the user's symlink")
	}

	// Nothing on the way to /tmp belongs to the user, and it's sticky.
	if p, err := trustedPath("/tmp/../tmp/.", false); err != nil || p != "/tmp" {
		t.Errorf("/tmp: %q, %v", p, err)
	}
}

func TestTrustedPathWithoutConfinement(t *testing.T) {
	dir := t.TempDir()
	p, err := trustedPath(filepath.Join(dir, "a", "b"), true)
	if err != nil || p != filepath.Join(dir, "a", "b") {
		t.Fatalf("%q, %v", p, err)
	}
	if _, err := trustedPath("relative", false); err == nil {
		t.Error("accepted a relative path")
	}
}
