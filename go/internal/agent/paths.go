package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The module names paths the agent opens as root (files.root, share.directory,
// terminal.historyFile, the font). A path in the session user's home goes
// through the home's os.Root, since the user owns every step of it; anywhere
// else, no component may belong to anyone but root.

// confine is whether configured paths need vetting: an unprivileged agent
// can't reach more than its own user already can.
var confine = os.Getuid() == 0

// trustedOwner reports whether uid may own a component of a configured path
// outside the home.
var trustedOwner = func(uid uint32) bool { return uid == 0 }

// maxLinks bounds the symlinks followed while resolving one path, as the kernel does.
const maxLinks = 40

// underHome returns p relative to home, or false when p is elsewhere.
func underHome(home, p string) (string, bool) {
	if home == "" || home == "/" || !filepath.IsAbs(p) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// configuredDir opens dir, a directory the module names, making it first
// when create is set (in the home, owned by owner).
func configuredDir(home, dir string, create bool, owner *fileOwner) (*os.Root, error) {
	if rel, ok := underHome(home, dir); ok {
		h, err := os.OpenRoot(home)
		if err != nil {
			return nil, err
		}
		defer h.Close()
		if rel == "." {
			return h.OpenRoot(".")
		}
		if create {
			if _, err := h.Stat(rel); errors.Is(err, fs.ErrNotExist) {
				if err := h.MkdirAll(rel, 0o755); err != nil {
					return nil, err
				}
				if owner != nil {
					h.Lchown(rel, owner.uid, owner.gid)
				}
			}
		}
		return h.OpenRoot(rel)
	}
	p, err := trustedPath(dir, create)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(p)
}

// configuredFile opens a file the module names, read-only and without
// blocking on a FIFO.
func configuredFile(home, p string) (*os.File, error) {
	if rel, ok := underHome(home, p); ok {
		return openInHome(home, rel)
	}
	resolved, err := trustedPath(p, false)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// trustedPath resolves p one component at a time, refusing it when someone
// other than root could change where it leads: a component (a symlink
// included) they own, or a directory on the way they may write that isn't
// sticky. With create, missing directories are made (as root) on the way, and
// checked like the rest, so a racing user can't slip one in.
func trustedPath(p string, create bool) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	if !confine {
		if create {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return "", err
			}
		}
		return filepath.Clean(p), nil
	}
	resolved := "/"
	parent, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	rest := strings.Split(p, "/")
	for links := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			if parent, err = os.Lstat(resolved); err != nil {
				return "", err
			}
			continue
		}
		if err := checkDir(resolved, parent); err != nil {
			return "", err
		}
		next := filepath.Join(resolved, name)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := os.Mkdir(next, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
			fi, err = os.Lstat(next)
		}
		if err != nil {
			return "", err
		}
		if err := checkOwner(next, fi); err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if links++; links > maxLinks {
				return "", fmt.Errorf("%s: too many symlinks", p)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				resolved = "/"
				if parent, err = os.Lstat(resolved); err != nil {
					return "", err
				}
			}
			rest = append(strings.Split(target, "/"), rest...)
			continue
		}
		resolved, parent = next, fi
	}
	return resolved, nil
}

func checkOwner(p string, fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner", p)
	}
	if !trustedOwner(st.Uid) {
		return fmt.Errorf("%s belongs to uid %d, not root: put the path in the user's home, or make root own it", p, st.Uid)
	}
	return nil
}

// checkDir refuses a directory whose entries others may replace.
func checkDir(p string, fi fs.FileInfo) error {
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("%s is writable by others", p)
	}
	return nil
}
