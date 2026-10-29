package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Android folders the app shares, mounted with FUSE under the mount point
// (docs/architecture.md, Integrations). The agent can't dial the app, so the app
// keeps hostfs streams open per folder and answers the file operations the
// agent sends on them; without one, operations fail with EIO and the mount
// stays, so a shell inside it keeps its cwd.

// hostFolders is the mounted folders by name.
type hostFolders struct {
	mu      sync.Mutex // serializes apply
	folders map[string]*hostFolder
}

// hostFolder is one mounted folder: its streams from the app and a short
// cache of directory listings, since listing is the slow part of SAF.
type hostFolder struct {
	name     string
	writable atomic.Bool
	owner    fileOwner
	// dir is the mount point's parent, as configured when mounted.
	dir   string
	link  hostLink
	mount *hostMount

	cmu   sync.Mutex
	cache map[string]listing
}

type listing struct {
	at      time.Time
	entries map[string]protocol.FileInfo
}

// hostCacheTTL is how long a listing answers lookups, and the kernel keeps
// entries and attributes.
const hostCacheTTL = 2 * time.Second

// mountFolder mounts f under dir; a variable so tests can do without FUSE.
var mountFolder = func(s *Server, dir string, f *hostFolder) error {
	m, err := s.mountHostFolder(dir, f)
	f.mount = m
	return err
}

// unmountFolder undoes mountFolder.
var unmountFolder = func(s *Server, f *hostFolder) error {
	if f.mount == nil {
		return nil
	}
	return s.unmountHostFolder(f.dir, f.name, f.mount)
}

// hostFoldersDir is where folders are mounted: absolute, or relative to the
// session user's home.
func (s *Server) hostFoldersDir() (string, error) {
	cfg := s.config()
	dir := cfg.HostFolders.MountPoint
	if dir == "" {
		dir = "Android"
	}
	if !filepath.IsAbs(dir) {
		home := userHome(cfg.User)
		if home == "" {
			return "", fmt.Errorf("host folders' mount point %q: the session user has no home", dir)
		}
		dir = filepath.Join(home, dir)
	}
	return filepath.Clean(dir), nil
}

// applyHostFolders mounts the folders in want that aren't, and unmounts
// those not in want. A folder that fails stays out (or in) and is named in
// the error; the rest apply.
func (s *Server) applyHostFolders(want []protocol.HostFolder) error {
	h := &s.hostfs
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.folders == nil {
		h.folders = map[string]*hostFolder{}
	}
	dir, err := s.hostFoldersDir()
	if err != nil {
		return err
	}
	var errs []error
	wanted := map[string]protocol.HostFolder{}
	for _, f := range want {
		if err := validName(f.Name); err != nil {
			errs = append(errs, fmt.Errorf("host folder %q: %w", f.Name, err))
			continue
		}
		if _, dup := wanted[f.Name]; dup {
			errs = append(errs, fmt.Errorf("host folder %q: named twice", f.Name))
			continue
		}
		wanted[f.Name] = f
	}
	// Unmount first: a folder renamed in the app may reuse a name.
	for name, f := range h.folders {
		if w, ok := wanted[name]; ok && f.dir == dir {
			f.writable.Store(w.Writable)
			continue
		}
		if err := unmountFolder(s, f); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", name, err))
			continue
		}
		f.link.close()
		delete(h.folders, name)
		log.Printf("host folder %s unmounted", name)
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	slices.Sort(names)
	u, err := sessionUser(s.config().User)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	s.sweepHostMounts(dir, h.folders)
	for _, name := range names {
		if _, ok := h.folders[name]; ok {
			continue
		}
		f := &hostFolder{name: name, owner: fileOwner{u.uid, u.gid}, dir: dir}
		f.writable.Store(wanted[name].Writable)
		if err := mountFolder(s, dir, f); err != nil {
			errs = append(errs, fmt.Errorf("mount %s: %w", name, err))
			continue
		}
		h.folders[name] = f
		log.Printf("host folder %s mounted in %s", name, dir)
	}
	return errors.Join(errs...)
}

func (s *Server) hostFolder(name string) *hostFolder {
	s.hostfs.mu.Lock()
	defer s.hostfs.mu.Unlock()
	return s.hostfs.folders[name]
}

// serveHostFS adds an app's stream to its folder until either side closes it.
func (s *Server) serveHostFS(c net.Conn, h protocol.Hello) {
	f := s.hostFolder(h.Name)
	if f == nil {
		reject(c, fmt.Sprintf("no host folder %q", h.Name))
		return
	}
	if protocol.WriteJSON(c, s.hello()) != nil {
		return
	}
	f.link.serve(c)
}

// hostLink is a folder's streams from the app.
type hostLink struct {
	mu      sync.Mutex
	streams []*hostStream
	closed  bool
}

// hostStream is one stream and the requests awaiting their answers on it.
type hostStream struct {
	conn net.Conn
	wmu  sync.Mutex

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]chan hostAnswer
	dead    bool
}

type hostAnswer struct {
	reply protocol.HostFSReply
	data  []byte
}

// hostFSTimeout bounds an answer; an app that takes longer is as good as gone.
var hostFSTimeout = 30 * time.Second

const hostWriteTimeout = 10 * time.Second

func (l *hostLink) serve(c net.Conn) {
	st := &hostStream{conn: c, pending: map[uint32]chan hostAnswer{}}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.streams = append(l.streams, st)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.streams = slices.DeleteFunc(l.streams, func(o *hostStream) bool { return o == st })
		l.mu.Unlock()
		st.fail()
	}()
	// Only frames follow the Hellos, so the reader may read ahead.
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		var reply protocol.HostFSReply
		typ, id, data, err := protocol.ReadHostFS(r, &reply)
		if err != nil {
			return
		}
		if typ != protocol.FrameHostFSReply {
			log.Printf("hostfs: unexpected frame %#x", typ)
			return
		}
		st.mu.Lock()
		ch := st.pending[id]
		delete(st.pending, id)
		st.mu.Unlock()
		if ch != nil {
			ch <- hostAnswer{reply, data}
		}
	}
}

// close ends every stream, for a folder that's gone.
func (l *hostLink) close() {
	l.mu.Lock()
	l.closed = true
	streams := l.streams
	l.mu.Unlock()
	for _, st := range streams {
		st.conn.Close()
	}
}

// fail ends the stream's pending requests.
func (st *hostStream) fail() {
	st.conn.Close()
	st.mu.Lock()
	st.dead = true
	for id, ch := range st.pending {
		close(ch)
		delete(st.pending, id)
	}
	st.mu.Unlock()
}

// pick returns the stream with the fewest requests in flight.
func (l *hostLink) pick() *hostStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best *hostStream
	bestN := 0
	for _, st := range l.streams {
		st.mu.Lock()
		n := len(st.pending)
		st.mu.Unlock()
		if best == nil || n < bestN {
			best, bestN = st, n
		}
	}
	return best
}

// call sends one request and waits for its answer. Without a stream, or
// when the stream ends first, it fails at once with EIO.
func (l *hostLink) call(ctx context.Context, req protocol.HostFSRequest, data []byte) (protocol.HostFSReply, []byte, syscall.Errno) {
	st := l.pick()
	if st == nil {
		return protocol.HostFSReply{}, nil, syscall.EIO
	}
	ch := make(chan hostAnswer, 1)
	st.mu.Lock()
	if st.dead {
		st.mu.Unlock()
		return protocol.HostFSReply{}, nil, syscall.EIO
	}
	st.nextID++
	id := st.nextID
	st.pending[id] = ch
	st.mu.Unlock()
	forget := func() {
		st.mu.Lock()
		delete(st.pending, id)
		st.mu.Unlock()
	}
	st.wmu.Lock()
	st.conn.SetWriteDeadline(time.Now().Add(hostWriteTimeout))
	err := protocol.WriteHostFS(st.conn, protocol.FrameHostFSRequest, id, req, data)
	st.wmu.Unlock()
	if err != nil {
		forget()
		st.conn.Close()
		return protocol.HostFSReply{}, nil, syscall.EIO
	}
	timer := time.NewTimer(hostFSTimeout)
	defer timer.Stop()
	select {
	case a, ok := <-ch:
		if !ok {
			return protocol.HostFSReply{}, nil, syscall.EIO
		}
		if a.reply.Error != "" {
			return a.reply, nil, errnoNamed(a.reply.Errno)
		}
		return a.reply, a.data, 0
	case <-ctx.Done():
		forget()
		return protocol.HostFSReply{}, nil, syscall.EINTR
	case <-timer.C:
		forget()
		log.Printf("hostfs: no answer to %s in %v", req.Op, hostFSTimeout)
		return protocol.HostFSReply{}, nil, syscall.EIO
	}
}

// errnoNamed maps an app's errno name to the errno, EIO when unknown.
func errnoNamed(name string) syscall.Errno {
	errnosOnce.Do(func() {
		errnos = map[string]syscall.Errno{}
		for e := syscall.Errno(1); e < 256; e++ {
			if n := unix.ErrnoName(e); n != "" {
				errnos[n] = e
			}
		}
	})
	if e, ok := errnos[name]; ok {
		return e
	}
	return syscall.EIO
}

var (
	errnosOnce sync.Once
	errnos     map[string]syscall.Errno
)

// Path-level operations, which the FUSE nodes (hostfs_fuse.go) map to.

func (f *hostFolder) do(ctx context.Context, req protocol.HostFSRequest) (protocol.HostFSReply, syscall.Errno) {
	r, _, errno := f.link.call(ctx, req, nil)
	return r, errno
}

func (f *hostFolder) stat(ctx context.Context, p string) (protocol.FileInfo, syscall.Errno) {
	r, errno := f.do(ctx, protocol.HostFSRequest{Op: protocol.FileStat, Path: p})
	if errno == 0 && r.File == nil {
		errno = syscall.EIO
	}
	if errno != 0 {
		return protocol.FileInfo{}, errno
	}
	return *r.File, 0
}

// list lists directory dir, and keeps the listing for lookups.
func (f *hostFolder) list(ctx context.Context, dir string) ([]protocol.FileInfo, syscall.Errno) {
	r, errno := f.do(ctx, protocol.HostFSRequest{Op: protocol.FileList, Path: dir})
	if errno != 0 {
		return nil, errno
	}
	entries := make(map[string]protocol.FileInfo, len(r.Files))
	files := r.Files[:0]
	for _, fi := range r.Files {
		if validName(fi.Name) != nil {
			continue
		}
		fi.Path = path.Join(dir, fi.Name)
		entries[fi.Name] = fi
		files = append(files, fi)
	}
	f.cmu.Lock()
	if f.cache == nil {
		f.cache = map[string]listing{}
	}
	f.cache[dir] = listing{at: time.Now(), entries: entries}
	f.cmu.Unlock()
	return files, 0
}

// lookup stats name in dir, from dir's listing while that's fresh.
func (f *hostFolder) lookup(ctx context.Context, dir, name string) (protocol.FileInfo, syscall.Errno) {
	f.cmu.Lock()
	l, ok := f.cache[dir]
	f.cmu.Unlock()
	if ok && time.Since(l.at) < hostCacheTTL {
		if fi, ok := l.entries[name]; ok {
			return fi, 0
		}
		return protocol.FileInfo{}, syscall.ENOENT
	}
	return f.stat(ctx, path.Join(dir, name))
}

// forget drops the cached listings after a change.
func (f *hostFolder) forget() {
	f.cmu.Lock()
	f.cache = nil
	f.cmu.Unlock()
}

func (f *hostFolder) checkWritable() syscall.Errno {
	if !f.writable.Load() {
		return syscall.EROFS
	}
	return 0
}

// create makes name in dir. A provider that picked another name (it was
// taken after all) gets its file removed again: the caller asked for name.
func (f *hostFolder) create(ctx context.Context, dir, name string, isDir bool) (protocol.FileInfo, syscall.Errno) {
	if errno := f.checkWritable(); errno != 0 {
		return protocol.FileInfo{}, errno
	}
	defer f.forget()
	r, errno := f.do(ctx, protocol.HostFSRequest{Op: protocol.FileCreate, Path: dir, Name: name, Dir: isDir})
	if errno == 0 && r.File == nil {
		errno = syscall.EIO
	}
	if errno != 0 {
		return protocol.FileInfo{}, errno
	}
	if r.File.Name != name {
		f.do(context.Background(), protocol.HostFSRequest{Op: protocol.FileDelete, Path: path.Join(dir, r.File.Name)})
		return protocol.FileInfo{}, syscall.EEXIST
	}
	fi := *r.File
	fi.Path = path.Join(dir, name)
	return fi, 0
}

// remove is unlink (isDir false) or rmdir: the provider deletes recursively,
// so the POSIX checks happen here.
func (f *hostFolder) remove(ctx context.Context, p string, isDir bool) syscall.Errno {
	if errno := f.checkWritable(); errno != 0 {
		return errno
	}
	fi, errno := f.stat(ctx, p)
	if errno != 0 {
		return errno
	}
	switch {
	case isDir && !fi.Dir:
		return syscall.ENOTDIR
	case !isDir && fi.Dir:
		return syscall.EISDIR
	case isDir:
		entries, errno := f.list(ctx, p)
		if errno != 0 {
			return errno
		}
		if len(entries) > 0 {
			return syscall.ENOTEMPTY
		}
	}
	defer f.forget()
	_, errno = f.do(ctx, protocol.HostFSRequest{Op: protocol.FileDelete, Path: p})
	return errno
}

// rename moves from to to, replacing what's there as rename(2) does; the
// replacement isn't atomic, since providers only rename to free names.
func (f *hostFolder) rename(ctx context.Context, from, to string, flags uint32) syscall.Errno {
	if errno := f.checkWritable(); errno != 0 {
		return errno
	}
	if flags&^unix.RENAME_NOREPLACE != 0 {
		return syscall.EINVAL
	}
	if from == to {
		return 0
	}
	if strings.HasPrefix(to, from+"/") {
		return syscall.EINVAL
	}
	src, errno := f.stat(ctx, from)
	if errno != 0 {
		return errno
	}
	defer f.forget()
	dst, errno := f.stat(ctx, to)
	switch {
	case errno == syscall.ENOENT:
	case errno != 0:
		return errno
	case flags&unix.RENAME_NOREPLACE != 0:
		return syscall.EEXIST
	case src.Dir && !dst.Dir:
		return syscall.ENOTDIR
	case !src.Dir && dst.Dir:
		return syscall.EISDIR
	default:
		if errno := f.remove(ctx, to, dst.Dir); errno != 0 {
			return errno
		}
	}
	_, errno = f.do(ctx, protocol.HostFSRequest{Op: protocol.FileRename, Path: from, To: to})
	return errno
}

// androidMode is the Android open mode for open(2) flags.
func androidMode(flags uint32) string {
	switch {
	case flags&syscall.O_ACCMODE == syscall.O_RDONLY:
		return "r"
	case flags&syscall.O_TRUNC != 0:
		return "rwt"
	default:
		// Write-only opens read-write too: Android's "w" truncates.
		return "rw"
	}
}

// hostHandle is a file the app holds open.
type hostHandle struct {
	f     *hostFolder
	id    int64
	dirty atomic.Bool
}

func (f *hostFolder) open(ctx context.Context, p string, flags uint32) (*hostHandle, syscall.Errno) {
	mode := androidMode(flags)
	if mode != "r" {
		if errno := f.checkWritable(); errno != 0 {
			return nil, errno
		}
	}
	r, errno := f.do(ctx, protocol.HostFSRequest{Op: protocol.FileOpen, Path: p, Mode: mode})
	if errno != 0 {
		return nil, errno
	}
	if mode == "rwt" {
		f.forget()
	}
	return &hostHandle{f: f, id: r.Handle}, 0
}

// truncate sets p's size, through h when it's open.
func (f *hostFolder) truncate(ctx context.Context, p string, h *hostHandle, size int64) syscall.Errno {
	if errno := f.checkWritable(); errno != 0 {
		return errno
	}
	req := protocol.HostFSRequest{Op: protocol.FileTruncate, Path: p, Size: size}
	if h != nil {
		req.Handle = h.id
		h.dirty.Store(true)
	}
	defer f.forget()
	_, errno := f.do(ctx, req)
	return errno
}

func (h *hostHandle) read(ctx context.Context, off int64, n int) ([]byte, syscall.Errno) {
	_, data, errno := h.f.link.call(ctx, protocol.HostFSRequest{Op: protocol.FileRead, Handle: h.id, Offset: off, Size: int64(n)}, nil)
	if len(data) > n {
		data = data[:n]
	}
	return data, errno
}

func (h *hostHandle) write(ctx context.Context, off int64, data []byte) (int, syscall.Errno) {
	if errno := h.f.checkWritable(); errno != 0 {
		return 0, errno
	}
	h.dirty.Store(true)
	r, _, errno := h.f.link.call(ctx, protocol.HostFSRequest{Op: protocol.FileWrite, Handle: h.id, Offset: off}, data)
	if errno != 0 {
		return 0, errno
	}
	h.f.forget()
	return int(min(r.Size, int64(len(data)))), 0
}

// sync flushes the writes since the last sync, so a failed write shows at close(2).
func (h *hostHandle) sync(ctx context.Context, always bool) syscall.Errno {
	if !h.dirty.Swap(false) && !always {
		return 0
	}
	_, errno := h.f.do(ctx, protocol.HostFSRequest{Op: protocol.FileSync, Handle: h.id})
	return errno
}

func (h *hostHandle) close() {
	h.f.do(context.Background(), protocol.HostFSRequest{Op: protocol.FileClose, Handle: h.id})
}
