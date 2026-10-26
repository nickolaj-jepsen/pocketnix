package agent_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// startIntegrations serves a config whose file root and share directory are
// fresh temp dirs, and a guest socket.
func startIntegrations(t *testing.T, extra map[string]any) (h *harness, root, inbox, guestSock string) {
	t.Helper()
	root, inbox = t.TempDir(), t.TempDir()
	cfg := baseConfig()
	cfg["filesRoot"] = root
	cfg["share"] = map[string]any{"directory": inbox}
	for k, v := range extra {
		cfg[k] = v
	}
	h = start(t, cfg, unixAddr)
	guestSock = filepath.Join(t.TempDir(), "guest.sock")
	l, err := agent.ListenGuest(guestSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go h.srv.ServeGuest(l)
	return h, root, inbox, guestSock
}

func nextEvent(t *testing.T, ctl *client.Control) protocol.Event {
	t.Helper()
	select {
	case ev := <-ctl.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	panic("unreachable")
}

func TestGuestRequestsReachTheApp(t *testing.T) {
	h, root, _, sock := startIntegrations(t, nil)
	guest := func(req protocol.GuestRequest) (string, error) { return client.Guest(sock, req) }

	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestNotify, Body: "hi"}); err == nil || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("notify without the app: %v", err)
	}

	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if _, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true }); err != nil {
		t.Fatal(err)
	}

	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestNotify, Title: "make", Body: "done"}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeNotify || ev.Title != "make" || ev.Body != "done" {
		t.Errorf("event = %+v", ev)
	}

	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestOpen, URL: "https://nixos.org"}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeOpen || ev.URL != "https://nixos.org" {
		t.Errorf("event = %+v", ev)
	}
	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestOpen, URL: "not a url"}); err == nil {
		t.Error("opened a non-URL")
	}

	// Files open through the file provider, so they must be under its root.
	must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "docs", "a.pdf"), nil, 0o644))
	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestOpen, Path: filepath.Join(root, "docs", "a.pdf")}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeOpen || ev.Path != "docs/a.pdf" {
		t.Errorf("event = %+v", ev)
	}
	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestOpen, Path: "/etc/hostname"}); err == nil {
		t.Error("opened a file outside the root")
	}

	// Clipboard both ways: a guest copy reaches Android; Android's reaches a guest paste.
	if _, err := guest(protocol.GuestRequest{Type: protocol.GuestCopy, Text: "from guest"}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeGuestClipboard || ev.Text != "from guest" {
		t.Errorf("event = %+v", ev)
	}
	if err := ctl.SetClipboard("from android"); err != nil {
		t.Fatal(err)
	}
	if got, err := guest(protocol.GuestRequest{Type: protocol.GuestPaste}); err != nil || got != "from android" {
		t.Errorf("paste = %q, %v", got, err)
	}
	// Pushes don't disturb request pairing.
	if err := ctl.Ping(); err != nil {
		t.Errorf("ping after events: %v", err)
	}
}

func TestFileProvider(t *testing.T) {
	h, root, _, _ := startIntegrations(t, nil)
	c := h.client
	must(t, os.MkdirAll(filepath.Join(root, "dir"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "dir", "a.txt"), []byte("alpha"), 0o600))
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644))
	must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))

	entries, err := c.List("")
	if err != nil {
		t.Fatal(err)
	}
	// The symlink out of the root isn't listed.
	if names := fileNames(entries); !slices.Equal(names, []string{"dir"}) || !entries[0].Dir {
		t.Errorf("root lists %v (%+v)", names, entries)
	}
	if _, err := c.ReadFile("escape"); err == nil {
		t.Error("read through a symlink out of the root")
	}
	if _, err := c.ReadFile("../" + filepath.Base(outside) + "/secret"); err == nil {
		t.Error("read a path out of the root")
	}

	if b, err := c.ReadFile("dir/a.txt"); err != nil || string(b) != "alpha" {
		t.Errorf("read = %q, %v", b, err)
	}
	// A rewrite keeps the file's mode.
	if err := c.WriteFile("dir/a.txt", strings.NewReader("beta")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dir", "a.txt")); string(b) != "beta" {
		t.Errorf("after write: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(root, "dir", "a.txt")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode after write = %v", fi.Mode().Perm())
	}

	// Create picks a free name; directories too.
	r, err := c.File(protocol.Hello{Op: protocol.FileCreate, Path: "dir", Name: "a.txt"})
	if err != nil || r.File == nil || r.File.Path != "dir/a (1).txt" {
		t.Fatalf("create = %+v, %v", r.File, err)
	}
	r, err = c.File(protocol.Hello{Op: protocol.FileCreate, Path: "", Name: "sub", Dir: true})
	if err != nil || r.File == nil || !r.File.Dir {
		t.Fatalf("mkdir = %+v, %v", r.File, err)
	}
	if _, err := c.File(protocol.Hello{Op: protocol.FileCreate, Path: "", Name: "../x"}); err == nil {
		t.Error("created a file named ../x")
	}

	r, err = c.File(protocol.Hello{Op: protocol.FileRename, Path: "dir/a (1).txt", Name: "b.txt"})
	if err != nil || r.File.Path != "dir/b.txt" {
		t.Fatalf("rename = %+v, %v", r.File, err)
	}
	if _, err := c.File(protocol.Hello{Op: protocol.FileRename, Path: "dir/b.txt", Name: "a.txt"}); err == nil {
		t.Error("renamed over an existing file")
	}
	r, err = c.File(protocol.Hello{Op: protocol.FileStat, Path: "dir/b.txt"})
	if err != nil || r.File.Size != 0 || r.File.Name != "b.txt" || r.File.MTime == 0 {
		t.Errorf("stat = %+v, %v", r.File, err)
	}

	if _, err := c.File(protocol.Hello{Op: protocol.FileDelete, Path: "dir"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dir")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dir still there: %v", err)
	}
	if _, err := c.File(protocol.Hello{Op: protocol.FileDelete, Path: ""}); err == nil {
		t.Error("deleted the root")
	}
	// The file outside the root survived everything.
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Error(err)
	}
}

func fileNames(fs []protocol.FileInfo) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	slices.Sort(out)
	return out
}

func TestShare(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	h, _, inbox, _ := startIntegrations(t, map[string]any{})
	cfg := baseConfig()
	cfg["share"] = map[string]any{"directory": inbox, "command": "echo >" + marker}
	h.write(cfg)
	must(t, h.srv.Reload())

	p, err := h.client.Share("photo.jpg", []byte("JPEG"))
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(inbox, "photo.jpg") {
		t.Errorf("landed at %s", p)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, []byte("JPEG")) {
		t.Errorf("content %q", b)
	}
	// The share command ran on it, in the background.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(marker)
		if strings.TrimSpace(string(b)) == p {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("share command didn't run on %s (marker %q)", p, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A second share with the same name doesn't overwrite.
	p2, err := h.client.Share("photo.jpg", []byte("other"))
	if err != nil || p2 != filepath.Join(inbox, "photo (1).jpg") {
		t.Errorf("second share = %s, %v", p2, err)
	}

	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	tp, err := ctl.ShareText("a link https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(tp); string(b) != "a link https://example.com" || filepath.Dir(tp) != inbox {
		t.Errorf("text share at %s: %q", tp, b)
	}
}
