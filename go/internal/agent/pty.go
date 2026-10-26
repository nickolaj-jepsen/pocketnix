package agent

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// exitDrain bounds how long output is still forwarded after the session's
// process exits: a background child holding the tty open must not keep the
// tab alive.
const exitDrain = 500 * time.Millisecond

func (s *Server) servePTY(c net.Conn, hello protocol.Hello) {
	cfg := s.config()
	prof, ok := cfg.profile(hello.Profile)
	if !ok {
		reject(c, fmt.Sprintf("unknown profile %q", hello.Profile))
		return
	}
	cmd, err := sessionCommand(cfg, prof)
	if err != nil {
		reject(c, err.Error())
		return
	}
	ws := &pty.Winsize{Rows: clampSize(hello.Rows, 24), Cols: clampSize(hello.Cols, 80)}
	ptmx, err := startPTY(cmd, ws)
	if err != nil {
		reject(c, "start session: "+err.Error())
		return
	}
	defer ptmx.Close()
	w := &lockedWriter{w: c}
	if protocol.WriteJSON(w, s.hello()) != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return
	}

	// Guest → app.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && protocol.WriteFrame(w, protocol.FrameData, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// App → guest. Ends when the app closes the stream, or when the session
	// ends and the connection is closed below.
	go func() {
		for {
			typ, payload, err := protocol.ReadFrame(c)
			if err != nil {
				hangup(cmd.Process.Pid)
				return
			}
			switch typ {
			case protocol.FrameData:
				if _, err := ptmx.Write(payload); err != nil {
					return
				}
			case protocol.FrameResize:
				sz, err := protocol.DecodeWinsize(payload)
				if err != nil {
					log.Printf("pty: %v", err)
					continue
				}
				pty.Setsize(ptmx, &pty.Winsize{Rows: sz.Rows, Cols: sz.Cols, X: sz.X, Y: sz.Y})
			}
		}
	}()

	status := waitStatus(cmd.Wait())
	select {
	case <-outDone:
	case <-time.After(exitDrain):
	}
	protocol.WriteFrame(w, protocol.FrameExit, protocol.EncodeExit(status))
}

// hangup does what closing the master would: Go defers that close while the
// output goroutine is blocked reading, so signal the session's group directly.
func hangup(pid int) {
	syscall.Kill(-pid, syscall.SIGHUP)
	syscall.Kill(-pid, syscall.SIGCONT)
}

func clampSize(v int, def uint16) uint16 {
	if v < 1 || v > 0xffff {
		return def
	}
	return uint16(v)
}

// sessionCommand builds the login-shell command for prof as the configured user.
func sessionCommand(cfg *File, prof protocol.Profile) (*exec.Cmd, error) {
	u, err := sessionUser(cfg.User)
	if err != nil {
		return nil, err
	}
	shell := cfg.Shell
	if shell == "" {
		shell = u.shell
	}
	args := []string{"-l"}
	if prof.Command != "" {
		args = append(args, "-c", prof.Command)
	}
	cmd := exec.Command(shell, args...)
	// argv[0] as a bare name, as login(1) runs it.
	cmd.Args[0] = filepath.Base(shell)
	cmd.Dir = u.home
	if _, err := os.Stat(u.home); err != nil {
		cmd.Dir = "/"
	}
	cmd.Env = sessionEnv(u, shell)
	if u.uid != os.Getuid() {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid), Groups: u.groups},
		}
	}
	return cmd, nil
}

type sessUser struct {
	name, home, shell string
	uid, gid          int
	groups            []uint32
}

func sessionUser(name string) (*sessUser, error) {
	var u *user.User
	var err error
	if name == "" {
		u, err = user.Current()
	} else {
		u, err = user.Lookup(name)
	}
	if err != nil {
		return nil, fmt.Errorf("session user: %w", err)
	}
	su := &sessUser{name: u.Username, home: u.HomeDir, shell: loginShell(u.Username)}
	su.uid, _ = strconv.Atoi(u.Uid)
	su.gid, _ = strconv.Atoi(u.Gid)
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				su.groups = append(su.groups, uint32(n))
			}
		}
	}
	return su, nil
}

func sessionEnv(u *sessUser, shell string) []string {
	env := []string{
		"HOME=" + u.home,
		"USER=" + u.name,
		"LOGNAME=" + u.name,
		"SHELL=" + shell,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}
	// The login shell's profile sets the real PATH; this only has to find it.
	path := os.Getenv("PATH")
	if path == "" {
		path = "/run/wrappers/bin:/run/current-system/sw/bin:/usr/bin:/bin"
	}
	env = append(env, "PATH="+path)
	for _, k := range []string{"LANG", "LOCALE_ARCHIVE", "TZDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if rt := fmt.Sprintf("/run/user/%d", u.uid); dirExists(rt) {
		env = append(env, "XDG_RUNTIME_DIR="+rt)
	}
	return env
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// startPTY starts cmd on a new pty whose slave belongs to the session user,
// as login(1) leaves it.
func startPTY(cmd *exec.Cmd, ws *pty.Winsize) (*os.File, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	if err := pty.Setsize(ptmx, ws); err != nil {
		ptmx.Close()
		return nil, err
	}
	if cred := credential(cmd); cred != nil {
		gid := int(cred.Gid)
		if g, err := user.LookupGroup("tty"); err == nil {
			gid, _ = strconv.Atoi(g.Gid)
		}
		tty.Chown(int(cred.Uid), gid)
		tty.Chmod(0o620)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, err
	}
	return ptmx, nil
}

func credential(cmd *exec.Cmd) *syscall.Credential {
	if cmd.SysProcAttr == nil {
		return nil
	}
	return cmd.SysProcAttr.Credential
}

// waitStatus maps cmd.Wait's result to a FrameExit status.
func waitStatus(err error) int32 {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return -int32(ws.Signal())
		}
		return int32(ee.ExitCode())
	}
	return -1
}

// loginShell reads the user's shell from /etc/passwd; os/user doesn't expose it.
func loginShell(name string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/bin/sh"
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 7 && f[0] == name && f[6] != "" {
			return f[6]
		}
	}
	return "/bin/sh"
}
