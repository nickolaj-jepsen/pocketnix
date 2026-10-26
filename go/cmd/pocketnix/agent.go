package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

const agentUsage = `usage: pocketnix agent --connect <addr> <command> [args]

Talks to a guest agent directly, as the app does; <addr> is tcp:<host>:<port>,
unix:<path> or vsock:<cid>:<port>.

commands:
  config                 print the config the agent sends on connect
  watch                  print every config the agent sends, one JSON per line
  exec [flags]           run a terminal session, printing its output; exits with its status
      --profile <name>     profile to run (default: the module's default)
      --size <rows>x<cols> initial size (default 24x80)
      --resize <rows>x<cols> resize once the session printed something
      --input <text>       type <text> once the session printed something
  forward <port>         splice stdin/stdout to a guest TCP port (an ssh ProxyCommand)
  font <file>            save the configured font
  ping | poweroff | reboot
  updates                print the update state and generations
  update-check           look for a newer system; prints the state after
  update                 check, fetch and switch to a newer system
  switch-generation <n>  switch to generation <n> (a rollback, or forward again)
  history [limit]        print the user's shell history, newest first
  events                 print the guest's notify/open/clipboard pushes, one JSON per line
  set-clipboard <text>   hand the agent a clipboard, as the app does on focus
  share <file>           share a file, as Android's share sheet does
  share-text <text>      share text
  ls [path] | cat <path> | put <path>   the file provider (put reads stdin)
`

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, agentUsage) }
	connect := fs.String("connect", "", "agent address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *connect == "" || fs.NArg() == 0 {
		fs.Usage()
		return errUsage
	}
	c := &client.Client{App: "pocketnix-cli " + version, Dial: func() (net.Conn, error) { return transport.Dial(*connect) }}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	switch cmd {
	case "config", "watch":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		enc := json.NewEncoder(os.Stdout)
		for cfg := range ctl.Configs() {
			if err := enc.Encode(cfg); err != nil {
				return err
			}
			if cmd == "config" {
				return nil
			}
		}
		return errors.New("control stream closed")
	case "ping", "poweroff", "reboot":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		switch cmd {
		case "ping":
			return ctl.Ping()
		case "poweroff":
			return ctl.Poweroff()
		default:
			return ctl.Reboot()
		}
	case "updates", "update-check", "update", "switch-generation", "history":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		return agentUpdates(ctl, cmd, rest)
	case "events", "set-clipboard", "share-text":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		switch cmd {
		case "events":
			enc := json.NewEncoder(os.Stdout)
			for ev := range ctl.Events() {
				if err := enc.Encode(ev); err != nil {
					return err
				}
			}
			return errors.New("control stream closed")
		case "set-clipboard":
			return ctl.SetClipboard(strings.Join(rest, " "))
		default:
			p, err := ctl.ShareText(strings.Join(rest, " "))
			if err == nil {
				fmt.Println(p)
			}
			return err
		}
	case "share":
		if len(rest) != 1 {
			return errUsage
		}
		b, err := os.ReadFile(rest[0])
		if err != nil {
			return err
		}
		p, err := c.Share(filepath.Base(rest[0]), b)
		if err == nil {
			fmt.Println(p)
		}
		return err
	case "ls":
		entries, err := c.List(strings.Join(rest, ""))
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			enc.Encode(e)
		}
		return nil
	case "cat":
		if len(rest) != 1 {
			return errUsage
		}
		b, err := c.ReadFile(rest[0])
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	case "put":
		if len(rest) != 1 {
			return errUsage
		}
		return c.WriteFile(rest[0], os.Stdin)
	case "exec":
		return agentExec(c, rest)
	case "forward":
		if len(rest) != 1 {
			return errUsage
		}
		port, err := strconv.Atoi(rest[0])
		if err != nil {
			return err
		}
		conn, err := c.Forward(port)
		if err != nil {
			return err
		}
		return spliceStdio(conn)
	case "font":
		if len(rest) != 1 {
			return errUsage
		}
		b, err := c.Font()
		if err != nil {
			return err
		}
		return os.WriteFile(rest[0], b, 0o644)
	default:
		fs.Usage()
		return errUsage
	}
}

func agentUpdates(ctl *client.Control, cmd string, args []string) error {
	var start func() error
	switch cmd {
	case "updates":
		u, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true })
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(u)
	case "history":
		limit := 0
		if len(args) == 1 {
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return err
			}
			limit = n
		}
		entries, err := ctl.History(limit)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Println(e)
		}
		return nil
	case "update-check":
		start = ctl.UpdateCheck
	case "update":
		start = ctl.UpdateApply
	case "switch-generation":
		if len(args) != 1 {
			return errUsage
		}
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return err
		}
		start = func() error { return ctl.SwitchGeneration(n) }
	}
	u, err := ctl.Run(start)
	for _, l := range u.Log {
		fmt.Fprintln(os.Stderr, l)
	}
	if encErr := json.NewEncoder(os.Stdout).Encode(u); err == nil {
		err = encErr
	}
	return err
}

func parseSize(s string) (rows, cols int, err error) {
	r, c, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, fmt.Errorf("size %q: want <rows>x<cols>", s)
	}
	if rows, err = strconv.Atoi(r); err != nil {
		return 0, 0, err
	}
	cols, err = strconv.Atoi(c)
	return rows, cols, err
}

func agentExec(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	profile := fs.String("profile", "", "")
	size := fs.String("size", "24x80", "")
	resize := fs.String("resize", "", "")
	input := fs.String("input", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, cols, err := parseSize(*size)
	if err != nil {
		return err
	}
	p, err := c.PTY(*profile, rows, cols)
	if err != nil {
		return err
	}
	defer p.Close()
	// Typing before the session's shell is up would race its tty setup, so
	// both wait for the first output.
	pending := *resize != "" || *input != ""
	buf := make([]byte, 32*1024)
	for {
		n, err := p.Read(buf)
		os.Stdout.Write(buf[:n])
		if n > 0 && pending {
			pending = false
			if *resize != "" {
				r, c, err := parseSize(*resize)
				if err != nil {
					return err
				}
				if err := p.Resize(r, c); err != nil {
					return err
				}
			}
			if *input != "" {
				// Let the resize land before the input that may report it.
				time.Sleep(100 * time.Millisecond)
				p.Write([]byte(*input))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	status, _ := p.Exit()
	if status != 0 {
		return exitStatus(status)
	}
	return nil
}

// exitStatus makes the CLI exit with a session's status.
type exitStatus int32

func (e exitStatus) Error() string { return fmt.Sprintf("session exited with status %d", int32(e)) }

// spliceStdio ends when the guest closes its side, even if stdin stays open.
func spliceStdio(conn net.Conn) error {
	go func() {
		io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	_, err := io.Copy(os.Stdout, conn)
	return err
}
