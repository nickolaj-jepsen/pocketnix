package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Mounting as root in a directory the user owns: the mount point is opened
// one step at a time without following symlinks, and the mount and unmount
// go through /proc/self/fd, so the user can't swap a path under them.

// hostFSType is the mounts' type, as /proc/self/mountinfo shows it.
const hostFSType = "fuse.pocketnix"

// hostMount is a mounted folder.
type hostMount struct {
	server *fuse.Server
	// root is an O_PATH descriptor of the mount's root, which unmounting names.
	root int
}

// openMountDir opens dir (a configured path: confined to the home, or
// root's all the way) as a raw descriptor, made when missing.
func (s *Server) openMountDir(dir string, create bool, owner *fileOwner) (*os.File, bool, error) {
	home := userHome(s.config().User)
	_, inHome := underHome(home, dir)
	if !inHome {
		owner = nil
	}
	root, err := configuredDir(home, dir, create, owner)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	f, err := root.Open(".")
	return f, inHome, err
}

func (s *Server) mountHostFolder(dir string, f *hostFolder) (*hostMount, error) {
	if os.Getuid() != 0 {
		return nil, errors.New("mounting needs the agent to run as root")
	}
	parent, inHome, err := s.openMountDir(dir, true, &f.owner)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	pfd := int(parent.Fd())
	if err := clearStaleMount(pfd, f.name); err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(pfd, f.name, 0o755); err == nil {
		if inHome {
			// Not following a symlink the user may have swapped in since.
			unix.Fchownat(pfd, f.name, f.owner.uid, f.owner.gid, unix.AT_SYMLINK_NOFOLLOW)
		}
	} else if !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	target, err := openNoFollowDir(pfd, f.name)
	if err != nil {
		return nil, err
	}
	defer unix.Close(target)
	// Mounting over files would hide them.
	if err := checkEmpty(pfd, f.name); err != nil {
		return nil, err
	}
	fuseFd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	// No allow_other: only the session user (its uid and primary gid) gets in, not even root.
	opts := fmt.Sprintf("fd=%d,rootmode=40000,user_id=%d,group_id=%d,max_read=%d", fuseFd, f.owner.uid, f.owner.gid, protocol.MaxHostFSData)
	if err := unix.Mount(f.name, procFd(target), hostFSType, unix.MS_NOSUID|unix.MS_NODEV, opts); err != nil {
		unix.Close(fuseFd)
		return nil, fmt.Errorf("mount in %s: %w", dir, err)
	}
	m := &hostMount{root: -1}
	ttl := hostCacheTTL
	nodes := fs.NewNodeFS(&hostNode{f: f}, &fs.Options{EntryTimeout: &ttl, AttrTimeout: &ttl, NegativeTimeout: &ttl})
	// A /dev/fd/N mount point hands go-fuse the mounted descriptor: it neither
	// mounts nor probes the mount, which root may not enter.
	m.server, err = fuse.NewServer(nodes, "/dev/fd/"+strconv.Itoa(fuseFd), &fuse.MountOptions{
		Name:          "pocketnix",
		FsName:        f.name,
		MaxWrite:      protocol.MaxHostFSData,
		DisableXAttrs: true,
	})
	if err == nil {
		go m.server.Serve()
		m.root, err = unix.Openat(pfd, f.name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		// target is the directory under the mount; the mount's root is the one to name.
		if root, rerr := unix.Openat(pfd, f.name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); rerr == nil {
			unix.Unmount(procFd(root), unix.MNT_DETACH)
			unix.Close(root)
		}
		return nil, err
	}
	return m, nil
}

// unmountHostFolder detaches the mount, so shells inside it don't keep it
// busy, and removes the empty mount point.
func (s *Server) unmountHostFolder(dir, name string, m *hostMount) error {
	if err := unix.Unmount(procFd(m.root), unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
		return err
	}
	unix.Close(m.root)
	if parent, _, err := s.openMountDir(dir, false, nil); err == nil {
		unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
		parent.Close()
	}
	return nil
}

func procFd(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

// openNoFollowDir opens name in dirfd as an O_PATH descriptor, refusing anything but a directory.
func openNoFollowDir(dirfd int, name string) (int, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		if err == nil {
			err = fmt.Errorf("%s is not a directory", name)
		}
		return -1, err
	}
	return fd, nil
}

func checkEmpty(dirfd int, name string) error {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(fd), name)
	defer d.Close()
	if _, err := d.Readdirnames(1); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("%s isn't empty", name)
		}
		return err
	}
	return nil
}

// mountOf returns the id and type of the mount fd is on.
func mountOf(fd int) (int, string, error) {
	b, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return 0, "", err
	}
	id := -1
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "mnt_id:"); ok {
			id, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(mi))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 1 || fields[0] != strconv.Itoa(id) {
			continue
		}
		for i, f := range fields {
			if f == "-" && i+1 < len(fields) {
				return id, fields[i+1], nil
			}
		}
	}
	return 0, "", fmt.Errorf("mount %d not in mountinfo", id)
}

// clearStaleMount detaches a folder mount on name that an earlier agent
// left (its connection gone with it), and refuses any other mount there.
func clearStaleMount(dirfd int, name string) error {
	fd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	pid, _, err := mountOf(dirfd)
	if err != nil {
		return err
	}
	id, typ, err := mountOf(fd)
	if err != nil || id == pid {
		return err
	}
	if typ != hostFSType {
		return fmt.Errorf("%s is already a %s mount", name, typ)
	}
	return unix.Unmount(procFd(fd), unix.MNT_DETACH)
}

// sweepHostMounts detaches stale folder mounts in dir other than live's, and
// removes their mount points.
func (s *Server) sweepHostMounts(dir string, live map[string]*hostFolder) {
	parent, _, err := s.openMountDir(dir, false, nil)
	if err != nil {
		return
	}
	defer parent.Close()
	names, _ := parent.Readdirnames(-1)
	pfd := int(parent.Fd())
	for _, name := range names {
		if f, ok := live[name]; ok && f.dir == dir {
			continue
		}
		fd, err := unix.Openat(pfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		pid, _, _ := mountOf(pfd)
		id, typ, err := mountOf(fd)
		if err == nil && id != pid && typ == hostFSType {
			if err := unix.Unmount(procFd(fd), unix.MNT_DETACH); err != nil {
				log.Printf("host folder %s: stale mount: %v", name, err)
			} else {
				unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR)
			}
		}
		unix.Close(fd)
	}
}
