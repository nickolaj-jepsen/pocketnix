package agent

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

// hostHarness is an agent whose folder mounts are stubbed out: the tests
// drive the operations FUSE maps to, against a plain directory served the
// way the app serves an Android folder.
type hostHarness struct {
	t       *testing.T
	s       *Server
	c       *client.Client
	ctl     *client.Control
	mounted map[string]bool
}

func startHost(t *testing.T) *hostHarness {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent.json")
	b, _ := json.Marshal(map[string]any{"shell": "/bin/sh", "hostFolders": map[string]any{"mountPoint": filepath.Join(dir, "mnt")}})
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &hostHarness{t: t, s: s, mounted: map[string]bool{}}
	mount, unmount := mountFolder, unmountFolder
	mountFolder = func(_ *Server, _ string, f *hostFolder) error { h.mounted[f.name] = true; return nil }
	unmountFolder = func(_ *Server, f *hostFolder) error { delete(h.mounted, f.name); return nil }
	t.Cleanup(func() { mountFolder, unmountFolder = mount, unmount })
	l, err := transport.Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go s.Serve(l)
	addr := "tcp:" + l.Addr().String()
	h.c = &client.Client{App: "test", Dial: func() (net.Conn, error) { return transport.Dial(addr) }}
	if h.ctl, err = h.c.Control(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.ctl.Close() })
	return h
}

// serve plays the app for folder name from a fresh directory, returning the
// directory and the stream.
func (h *hostHarness) serve(name string, writable bool) (string, net.Conn) {
	h.t.Helper()
	dir := h.t.TempDir()
	srv, err := client.NewHostFSServer(dir, writable)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { srv.Close() })
	conn := h.stream(name)
	go srv.Serve(conn)
	f := h.s.hostFolder(name)
	for deadline := time.Now().Add(5 * time.Second); f.link.pick() == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatal("the agent never took the stream")
		}
	}
	return dir, conn
}

func (h *hostHarness) stream(name string) net.Conn {
	h.t.Helper()
	conn, err := h.c.HostFS(name)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	return conn
}

func errnoIs(t *testing.T, what string, got, want syscall.Errno) {
	t.Helper()
	if got != want {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

func TestHostFolderOperations(t *testing.T) {
	h := startHost(t)
	if err := h.ctl.HostFolders([]protocol.HostFolder{{Name: "shared", Writable: true}}); err != nil {
		t.Fatal(err)
	}
	dir, _ := h.serve("shared", true)
	f := h.s.hostFolder("shared")
	ctx := context.Background()

	if _, errno := f.create(ctx, "", "docs", true); errno != 0 {
		t.Fatalf("mkdir: %v", errno)
	}
	fi, errno := f.create(ctx, "docs", "a.txt", false)
	if errno != 0 || fi.Path != "docs/a.txt" {
		t.Fatalf("create: %+v %v", fi, errno)
	}
	_, errno = f.create(ctx, "docs", "a.txt", false)
	errnoIs(t, "create over a file", errno, syscall.EEXIST)

	// open(O_WRONLY) mustn't truncate, as Android's "w" would.
	hd, errno := f.open(ctx, "docs/a.txt", syscall.O_WRONLY)
	if errno != 0 {
		t.Fatalf("open: %v", errno)
	}
	if n, errno := hd.write(ctx, 0, []byte("hello world")); errno != 0 || n != 11 {
		t.Fatalf("write: %d %v", n, errno)
	}
	errnoIs(t, "flush", hd.sync(ctx, false), 0)
	hd.close()
	hd, _ = f.open(ctx, "docs/a.txt", syscall.O_RDWR)
	if b, errno := hd.read(ctx, 6, 100); errno != 0 || string(b) != "world" {
		t.Fatalf("read: %q %v", b, errno)
	}
	errnoIs(t, "truncate through the handle", f.truncate(ctx, "docs/a.txt", hd, 5), 0)
	hd.close()
	if b, _ := os.ReadFile(filepath.Join(dir, "docs/a.txt")); string(b) != "hello" {
		t.Errorf("after truncate: %q", b)
	}
	errnoIs(t, "truncate by path", f.truncate(ctx, "docs/a.txt", nil, 2), 0)
	if fi, errno := f.lookup(ctx, "docs", "a.txt"); errno != 0 || fi.Size != 2 {
		t.Errorf("lookup after truncate: %+v %v", fi, errno)
	}
	hd, _ = f.open(ctx, "docs/a.txt", syscall.O_WRONLY|syscall.O_TRUNC)
	hd.close()
	if fi, _ := f.stat(ctx, "docs/a.txt"); fi.Size != 0 {
		t.Errorf("O_TRUNC left %d bytes", fi.Size)
	}

	// rename(2) replaces a file, as editors saving through a temp file expect.
	os.WriteFile(filepath.Join(dir, "docs/a.txt"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs/.a.txt.swp"), []byte("new"), 0o644)
	errnoIs(t, "rename over a file", f.rename(ctx, "docs/.a.txt.swp", "docs/a.txt", 0), 0)
	if b, _ := os.ReadFile(filepath.Join(dir, "docs/a.txt")); string(b) != "new" {
		t.Errorf("after rename over: %q", b)
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), nil, 0o644)
	errnoIs(t, "rename with RENAME_NOREPLACE", f.rename(ctx, "b.txt", "docs/a.txt", unix.RENAME_NOREPLACE), syscall.EEXIST)
	errnoIs(t, "rename a file over a directory", f.rename(ctx, "b.txt", "docs", 0), syscall.EISDIR)
	errnoIs(t, "rename a directory into itself", f.rename(ctx, "docs", "docs/sub", 0), syscall.EINVAL)
	errnoIs(t, "move to another directory", f.rename(ctx, "b.txt", "docs/b.txt", 0), 0)
	if _, err := os.Stat(filepath.Join(dir, "docs/b.txt")); err != nil {
		t.Errorf("after move: %v", err)
	}

	errnoIs(t, "unlink a directory", f.remove(ctx, "docs", false), syscall.EISDIR)
	errnoIs(t, "rmdir a file", f.remove(ctx, "docs/a.txt", true), syscall.ENOTDIR)
	errnoIs(t, "rmdir a full directory", f.remove(ctx, "docs", true), syscall.ENOTEMPTY)
	errnoIs(t, "unlink", f.remove(ctx, "docs/a.txt", false), 0)
	errnoIs(t, "unlink", f.remove(ctx, "docs/b.txt", false), 0)
	errnoIs(t, "rmdir", f.remove(ctx, "docs", true), 0)
	if files, errno := f.list(ctx, ""); errno != 0 || len(files) != 0 {
		t.Errorf("left: %+v %v", files, errno)
	}
	_, errno = f.stat(ctx, "docs")
	errnoIs(t, "stat what's gone", errno, syscall.ENOENT)
}

func TestHostFolderListingAnswersLookups(t *testing.T) {
	h := startHost(t)
	h.ctl.HostFolders([]protocol.HostFolder{{Name: "shared", Writable: true}})
	dir, _ := h.serve("shared", true)
	f := h.s.hostFolder("shared")
	ctx := context.Background()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644)
	if files, _ := f.list(ctx, ""); len(files) != 1 || files[0].Path != "a" {
		t.Fatalf("list: %+v", files)
	}
	// Added behind the agent's back: unseen until the listing goes stale.
	os.WriteFile(filepath.Join(dir, "b"), nil, 0o644)
	_, errno := f.lookup(ctx, "", "b")
	errnoIs(t, "lookup within the listing's lifetime", errno, syscall.ENOENT)
	f.cmu.Lock()
	l := f.cache[""]
	l.at = time.Now().Add(-hostCacheTTL)
	f.cache[""] = l
	f.cmu.Unlock()
	_, errno = f.lookup(ctx, "", "b")
	errnoIs(t, "lookup once stale", errno, 0)
	// A change through the mount is seen at once.
	f.list(ctx, "")
	f.create(ctx, "", "c", false)
	_, errno = f.lookup(ctx, "", "c")
	errnoIs(t, "lookup after create", errno, 0)
}

func TestReadOnlyHostFolder(t *testing.T) {
	h := startHost(t)
	h.ctl.HostFolders([]protocol.HostFolder{{Name: "ro"}})
	dir, _ := h.serve("ro", false)
	os.WriteFile(filepath.Join(dir, "a"), []byte("keep"), 0o644)
	f := h.s.hostFolder("ro")
	ctx := context.Background()
	_, errno := f.create(ctx, "", "b", false)
	errnoIs(t, "create", errno, syscall.EROFS)
	_, errno = f.open(ctx, "a", syscall.O_RDWR)
	errnoIs(t, "open for writing", errno, syscall.EROFS)
	errnoIs(t, "unlink", f.remove(ctx, "a", false), syscall.EROFS)
	hd, errno := f.open(ctx, "a", syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open for reading: %v", errno)
	}
	defer hd.close()
	if b, errno := hd.read(ctx, 0, 10); string(b) != "keep" {
		t.Errorf("read: %q %v", b, errno)
	}
	// The app can make a folder read-only on a later announcement.
	h.ctl.HostFolders([]protocol.HostFolder{{Name: "ro", Writable: true}})
	errnoIs(t, "after it turned writable", f.checkWritable(), 0)
}

func TestHostFolderWithoutTheApp(t *testing.T) {
	h := startHost(t)
	h.ctl.HostFolders([]protocol.HostFolder{{Name: "shared", Writable: true}})
	f := h.s.hostFolder("shared")
	ctx := context.Background()
	_, errno := f.stat(ctx, "")
	errnoIs(t, "before the app connects", errno, syscall.EIO)

	_, conn := h.serve("shared", true)
	if _, errno := f.stat(ctx, ""); errno != 0 {
		t.Fatalf("stat: %v", errno)
	}
	conn.Close()
	start := time.Now()
	for f.link.pick() != nil {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the agent kept a closed stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, errno = f.stat(ctx, "")
	errnoIs(t, "with the app gone", errno, syscall.EIO)
	if time.Since(start) > 5*time.Second {
		t.Errorf("failing took %v", time.Since(start))
	}

	h.serve("shared", true)
	if _, errno := f.stat(ctx, ""); errno != 0 {
		t.Errorf("after the app came back: %v", errno)
	}
}

func TestHostFoldersAnnouncement(t *testing.T) {
	h := startHost(t)
	err := h.ctl.HostFolders([]protocol.HostFolder{{Name: "a"}, {Name: "b"}, {Name: "../etc"}})
	if err == nil || !strings.Contains(err.Error(), "../etc") {
		t.Errorf("a bad name: %v", err)
	}
	if !h.mounted["a"] || !h.mounted["b"] || len(h.mounted) != 2 {
		t.Fatalf("mounted %v, want a and b", h.mounted)
	}
	if err := h.ctl.HostFolders([]protocol.HostFolder{{Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	if h.mounted["a"] || !h.mounted["b"] {
		t.Errorf("mounted %v, want b", h.mounted)
	}
	if _, err := h.c.HostFS("a"); err == nil {
		t.Error("a stream for an unmounted folder was taken")
	}
	if err := h.ctl.HostFolders(nil); err != nil || len(h.mounted) != 0 {
		t.Errorf("after none: %v %v", h.mounted, err)
	}
}

func TestAndroidMode(t *testing.T) {
	for flags, want := range map[uint32]string{
		syscall.O_RDONLY:                    "r",
		syscall.O_WRONLY:                    "rw",
		syscall.O_RDWR:                      "rw",
		syscall.O_WRONLY | syscall.O_APPEND: "rw",
		syscall.O_WRONLY | syscall.O_TRUNC:  "rwt",
	} {
		if got := androidMode(flags); got != want {
			t.Errorf("androidMode(%#o) = %q, want %q", flags, got, want)
		}
	}
	if errnoNamed("ENOTEMPTY") != syscall.ENOTEMPTY || errnoNamed("") != syscall.EIO || errnoNamed("EWHATEVER") != syscall.EIO {
		t.Error("errno names")
	}
}
