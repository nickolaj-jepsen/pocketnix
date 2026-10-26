package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	defaultHistory = 500
	maxHistory     = 5000
	// historyTail bounds how much of a history file is read, from its end.
	historyTail = 4 << 20
)

// history returns the session user's shell commands, most recent first and
// each once, from the configured file or the login shell's default one.
func history(cfg *File, limit int) ([]string, error) {
	if limit <= 0 {
		limit = defaultHistory
	}
	limit = min(limit, maxHistory)
	path, shell := cfg.History, cfg.Shell
	var f *os.File
	var err error
	if path != "" {
		// The module's own path: trusted, but still read with care.
		f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	} else {
		// The agent is root and the user owns this path: open it inside the
		// home, so a symlink can't reach /etc/shadow or /dev/zero.
		u, uerr := sessionUser(cfg.User)
		if uerr != nil {
			return nil, uerr
		}
		if shell == "" {
			shell = u.shell
		}
		path = defaultHistoryFile(filepath.Base(shell))
		f, err = openInHome(u.home, path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := readTail(f, historyTail)
	if err != nil {
		return nil, err
	}
	var cmds []string
	switch {
	case strings.HasSuffix(path, "fish_history"):
		cmds = parseFish(b)
	case strings.Contains(filepath.Base(path), "zsh"):
		cmds = parseZsh(b)
	default:
		cmds = parseBash(b)
	}
	return recentUnique(cmds, limit), nil
}

// defaultHistoryFile is relative to the home.
func defaultHistoryFile(shell string) string {
	switch shell {
	case "fish":
		return ".local/share/fish/fish_history"
	case "zsh":
		return ".zsh_history"
	default:
		return ".bash_history"
	}
}

// openInHome opens rel under home without leaving it, and without blocking
// on a FIFO.
func openInHome(home, rel string) (*os.File, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// readTail reads at most the last n bytes of a regular file.
func readTail(f *os.File, n int64) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", f.Name())
	}
	if fi.Size() > n {
		if _, err := f.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, n))
		// Drop the partial first line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
		return b, err
	}
	// The file may grow while it's read.
	return io.ReadAll(io.LimitReader(f, n))
}

// recentUnique keeps each command's latest use, newest first.
func recentUnique(cmds []string, limit int) []string {
	seen := map[string]bool{}
	out := []string{}
	for i := len(cmds) - 1; i >= 0 && len(out) < limit; i-- {
		c := strings.TrimSpace(cmds[i])
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

var bashTimestamp = regexp.MustCompile(`^#\d+$`)

func parseBash(b []byte) []string {
	var cmds []string
	for _, line := range strings.Split(string(b), "\n") {
		if !bashTimestamp.MatchString(line) {
			cmds = append(cmds, line)
		}
	}
	return cmds
}

var zshExtended = regexp.MustCompile(`^: \d+:\d+;`)

// parseZsh reads plain and EXTENDED_HISTORY lines; a trailing backslash
// continues a multi-line command.
func parseZsh(b []byte) []string {
	b = zshUnmetafy(b)
	var cmds []string
	var cur strings.Builder
	cont := false
	for _, line := range strings.Split(string(b), "\n") {
		if !cont {
			line = zshExtended.ReplaceAllString(line, "")
			cur.Reset()
		}
		if strings.HasSuffix(line, `\`) {
			cur.WriteString(strings.TrimSuffix(line, `\`))
			cur.WriteByte('\n')
			cont = true
			continue
		}
		cur.WriteString(line)
		cont = false
		cmds = append(cmds, cur.String())
	}
	return cmds
}

// zshUnmetafy undoes zsh's history encoding: 0x83 marks a byte xor 0x20.
func zshUnmetafy(b []byte) []byte {
	if bytes.IndexByte(b, 0x83) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == 0x83 && i+1 < len(b) {
			i++
			out = append(out, b[i]^0x20)
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// parseFish reads fish's YAML-like history: "- cmd: <escaped command>".
func parseFish(b []byte) []string {
	var cmds []string
	for _, line := range strings.Split(string(b), "\n") {
		if c, ok := strings.CutPrefix(line, "- cmd: "); ok {
			cmds = append(cmds, fishUnescape(c))
		}
	}
	return cmds
}

func fishUnescape(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				out.WriteByte('\n')
				i++
				continue
			case '\\':
				out.WriteByte('\\')
				i++
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
