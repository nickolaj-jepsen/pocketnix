package image

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// Injection is one file to write into the image's root filesystem.
type Injection struct {
	Guest  string // absolute path in the guest
	Source string // host file, or "-" for stdin
	Mode   os.FileMode
}

// Inject writes files into the root partition of the image at path, in
// place, owned by root. Parent directories are created 0755 as needed.
// Callers inject into a copy: the image is usually a read-only store path.
// The sources are staged beside the image while debugfs runs.
func Inject(imgPath string, files []Injection, stdin io.Reader) error {
	root, err := RootPartition(imgPath)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(imgPath), ".pocketnix-inject-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var script strings.Builder
	dirs := map[string]bool{"/": true}
	for i, f := range files {
		if !path.IsAbs(f.Guest) || path.Clean(f.Guest) != f.Guest || f.Guest == "/" {
			return fmt.Errorf("guest path %q must be absolute and clean", f.Guest)
		}
		// debugfs reads the source by path; stage every source so a "-" (or a
		// path with spaces) works the same way.
		src := fmt.Sprintf("%s/%d", tmp, i)
		if err := stage(src, f.Source, stdin); err != nil {
			return err
		}
		var parents []string
		for d := path.Dir(f.Guest); !dirs[d]; d = path.Dir(d) {
			parents = append([]string{d}, parents...)
			dirs[d] = true
		}
		for _, d := range parents {
			if exists(imgPath, root, d) {
				continue
			}
			fmt.Fprintf(&script, "mkdir %s\n", quote(d))
			fmt.Fprintf(&script, "sif %s mode 040755\nsif %s uid 0\nsif %s gid 0\n", quote(d), quote(d), quote(d))
		}
		fmt.Fprintf(&script, "rm %s\n", quote(f.Guest))
		fmt.Fprintf(&script, "write %s %s\n", src, quote(f.Guest))
		fmt.Fprintf(&script, "sif %s mode 0%o\n", quote(f.Guest), 0o100000|uint32(f.Mode.Perm()))
		fmt.Fprintf(&script, "sif %s uid 0\nsif %s gid 0\n", quote(f.Guest), quote(f.Guest))
	}

	cmd := exec.Command("debugfs", "-w", "-f", "-", device(imgPath, root))
	cmd.Stdin = strings.NewReader(script.String())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("debugfs: %w\n%s", err, out.String())
	}
	// debugfs exits 0 even when a command fails; check each injected file landed.
	for _, f := range files {
		if err := checkFile(imgPath, root, f); err != nil {
			return fmt.Errorf("inject %s: %w\ndebugfs output:\n%s", f.Guest, err, out.String())
		}
	}
	return nil
}

func stage(dst, src string, stdin io.Reader) error {
	var r io.Reader
	if src == "-" {
		r = stdin
	} else {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func device(imgPath string, root Partition) string {
	return fmt.Sprintf("%s?offset=%d", imgPath, root.Offset)
}

func exists(imgPath string, root Partition, guest string) bool {
	out, err := exec.Command("debugfs", "-R", "stat "+quote(guest), device(imgPath, root)).CombinedOutput()
	return err == nil && strings.Contains(string(out), "Inode:")
}

// checkFile confirms debugfs's view of f: a regular file of the right mode and size.
func checkFile(imgPath string, root Partition, f Injection) error {
	want, err := fileSize(f)
	if err != nil {
		return err
	}
	out, err := exec.Command("debugfs", "-R", "stat "+quote(f.Guest), device(imgPath, root)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs stat: %w", err)
	}
	s := string(out)
	mode := fmt.Sprintf("Mode:  %04o", uint32(f.Mode.Perm()))
	if !strings.Contains(s, "Type: regular") || !strings.Contains(s, mode) {
		return fmt.Errorf("not a regular file with %s:\n%s", mode, s)
	}
	if want >= 0 && !strings.Contains(s, fmt.Sprintf("Size: %d\n", want)) {
		return fmt.Errorf("size isn't %d:\n%s", want, s)
	}
	return nil
}

// fileSize is the source's size, or -1 for stdin (already consumed).
func fileSize(f Injection) (int64, error) {
	if f.Source == "-" {
		return -1, nil
	}
	fi, err := os.Stat(f.Source)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// quote makes a path one debugfs argument.
func quote(p string) string {
	if strings.ContainsAny(p, " \t\"") {
		return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return p
}

// CopySparse copies the file at src to a new file at dst, leaving runs of
// zeros as holes (an image is mostly an empty ESP and free space).
func CopySparse(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	zero := make([]byte, len(buf))
	for off := int64(0); off < fi.Size(); {
		n, err := in.ReadAt(buf, off)
		if n > 0 && !bytes.Equal(buf[:n], zero[:n]) {
			if _, werr := out.WriteAt(buf[:n], off); werr != nil {
				out.Close()
				return werr
			}
		}
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Truncate(fi.Size()); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
