package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
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
