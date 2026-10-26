package agent

import (
	"context"
	"path"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// hostNode is a file or directory in a mounted host folder. Nodes carry no
// state but their place in the tree: every operation goes to the app by path.
type hostNode struct {
	fs.Inode
	f *hostFolder
}

var (
	_ fs.NodeLookuper  = (*hostNode)(nil)
	_ fs.NodeGetattrer = (*hostNode)(nil)
	_ fs.NodeSetattrer = (*hostNode)(nil)
	_ fs.NodeReaddirer = (*hostNode)(nil)
	_ fs.NodeOpener    = (*hostNode)(nil)
	_ fs.NodeCreater   = (*hostNode)(nil)
	_ fs.NodeMkdirer   = (*hostNode)(nil)
	_ fs.NodeUnlinker  = (*hostNode)(nil)
	_ fs.NodeRmdirer   = (*hostNode)(nil)
	_ fs.NodeRenamer   = (*hostNode)(nil)
)

// path is the node's path in the folder; false for a node unlinked since.
func (n *hostNode) path() (string, bool) {
	if n.IsRoot() {
		return "", true
	}
	if _, parent := n.Parent(); parent == nil {
		return "", false
	}
	return n.Path(nil), true
}

func (n *hostNode) child(name string) (string, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return "", syscall.ENOENT
	}
	return path.Join(p, name), 0
}

// fill sets attributes: the session user owns everything, and nothing is
// writable in a read-only folder.
func (f *hostFolder) fill(fi protocol.FileInfo, a *fuse.Attr) {
	a.Mode = syscall.S_IFREG | 0o644
	a.Nlink = 1
	if fi.Dir {
		a.Mode = syscall.S_IFDIR | 0o755
		a.Nlink = 2
	}
	if !f.writable.Load() || !fi.Writable {
		a.Mode &^= 0o222
	}
	a.Size = uint64(max(fi.Size, 0))
	a.Blocks = (a.Size + 511) / 512
	a.Blksize = 4096
	t := time.UnixMilli(fi.MTime)
	a.SetTimes(&t, &t, &t)
	a.Owner = fuse.Owner{Uid: uint32(f.owner.uid), Gid: uint32(f.owner.gid)}
}

func (n *hostNode) newChild(ctx context.Context, fi protocol.FileInfo, out *fuse.EntryOut) *fs.Inode {
	n.f.fill(fi, &out.Attr)
	mode := uint32(syscall.S_IFREG)
	if fi.Dir {
		mode = syscall.S_IFDIR
	}
	return n.NewInode(ctx, &hostNode{f: n.f}, fs.StableAttr{Mode: mode})
}

func (n *hostNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	dir, ok := n.path()
	if !ok {
		return nil, syscall.ENOENT
	}
	fi, errno := n.f.lookup(ctx, dir, name)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, fi, out), 0
}

func (n *hostNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	p, ok := n.path()
	if !ok {
		return syscall.ENOENT
	}
	fi, errno := n.f.stat(ctx, p)
	if errno != 0 {
		if !n.IsRoot() {
			return errno
		}
		// The mount point stays a directory while the app is away.
		fi = protocol.FileInfo{Dir: true, Writable: true}
	}
	n.f.fill(fi, &out.Attr)
	return 0
}

// Setattr truncates; mode, owner and times are accepted and ignored, as
// Android's providers keep none of them.
func (n *hostNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	p, ok := n.path()
	if !ok {
		return syscall.ENOENT
	}
	if size, ok := in.GetSize(); ok {
		h, _ := fh.(*hostFile)
		var hh *hostHandle
		if h != nil {
			hh = h.h
		}
		if errno := n.f.truncate(ctx, p, hh, int64(size)); errno != 0 {
			return errno
		}
	}
	return n.Getattr(ctx, fh, out)
}

func (n *hostNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return nil, syscall.ENOENT
	}
	files, errno := n.f.list(ctx, p)
	if errno != 0 {
		return nil, errno
	}
	entries := make([]fuse.DirEntry, 0, len(files))
	for _, fi := range files {
		mode := uint32(syscall.S_IFREG)
		if fi.Dir {
			mode = syscall.S_IFDIR
		}
		entries = append(entries, fuse.DirEntry{Name: fi.Name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *hostNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	p, ok := n.path()
	if !ok {
		return nil, 0, syscall.ENOENT
	}
	h, errno := n.f.open(ctx, p, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	// No FOPEN_KEEP_CACHE: the kernel drops cached pages on every open.
	return &hostFile{h: h}, 0, 0
}

func (n *hostNode) Create(ctx context.Context, name string, _, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	dir, ok := n.path()
	if !ok {
		return nil, nil, 0, syscall.ENOENT
	}
	fi, errno := n.f.create(ctx, dir, name, false)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	h, errno := n.f.open(ctx, fi.Path, syscall.O_RDWR)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	return n.newChild(ctx, fi, out), &hostFile{h: h}, 0, 0
}

func (n *hostNode) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	dir, ok := n.path()
	if !ok {
		return nil, syscall.ENOENT
	}
	fi, errno := n.f.create(ctx, dir, name, true)
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, fi, out), 0
}

func (n *hostNode) Unlink(ctx context.Context, name string) syscall.Errno {
	p, errno := n.child(name)
	if errno != 0 {
		return errno
	}
	return n.f.remove(ctx, p, false)
}

func (n *hostNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	p, errno := n.child(name)
	if errno != 0 {
		return errno
	}
	return n.f.remove(ctx, p, true)
}

func (n *hostNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	from, errno := n.child(name)
	if errno != 0 {
		return errno
	}
	np, ok := newParent.(*hostNode)
	if !ok {
		return syscall.EXDEV
	}
	to, errno := np.child(newName)
	if errno != 0 {
		return errno
	}
	return n.f.rename(ctx, from, to, flags)
}

// hostFile is an open file's handle.
type hostFile struct {
	h *hostHandle
}

var (
	_ fs.FileReader   = (*hostFile)(nil)
	_ fs.FileWriter   = (*hostFile)(nil)
	_ fs.FileFlusher  = (*hostFile)(nil)
	_ fs.FileFsyncer  = (*hostFile)(nil)
	_ fs.FileReleaser = (*hostFile)(nil)
)

func (h *hostFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, errno := h.h.read(ctx, off, len(dest))
	if errno != 0 {
		return nil, errno
	}
	return fuse.ReadResultData(data), 0
}

func (h *hostFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, errno := h.h.write(ctx, off, data)
	return uint32(n), errno
}

// Flush runs on every close(2): a write the provider failed to store shows there.
func (h *hostFile) Flush(ctx context.Context) syscall.Errno { return h.h.sync(ctx, false) }

func (h *hostFile) Fsync(ctx context.Context, _ uint32) syscall.Errno { return h.h.sync(ctx, true) }

func (h *hostFile) Release(context.Context) syscall.Errno {
	h.h.close()
	return 0
}
