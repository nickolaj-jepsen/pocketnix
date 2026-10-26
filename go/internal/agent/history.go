package agent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	if path == "" || shell == "" {
		u, err := sessionUser(cfg.User)
		if err != nil {
			return nil, err
		}
		if shell == "" {
			shell = u.shell
		}
		if path == "" {
			path = defaultHistoryFile(u.home, filepath.Base(shell))
		}
	}
	b, err := readTail(path, historyTail)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
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

func defaultHistoryFile(home, shell string) string {
	switch shell {
	case "fish":
		return filepath.Join(home, ".local/share/fish/fish_history")
	case "zsh":
		return filepath.Join(home, ".zsh_history")
	default:
		return filepath.Join(home, ".bash_history")
	}
}

func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > n {
		if _, err := f.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return nil, err
		}
		b, err := io.ReadAll(f)
		// Drop the partial first line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
		return b, err
	}
	return io.ReadAll(f)
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
