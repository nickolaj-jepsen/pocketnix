package main

import (
	"bufio"
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
  gui                    splice stdin/stdout to the guest compositor's GUI socket
  udp [flags] <port>     send each stdin line as a datagram to a guest UDP port,
                         printing each reply as a line
      --wait <duration>    how long to wait for replies after stdin ends (default 2s)
  font <file>            save the configured font
  ping | poweroff | reboot
  updates                print the update state and generations
  update-check           look for a newer system; prints the state after
  update                 check, fetch and switch to a newer system
  update-cancel          stop a running check or download
  switch-generation <n>  switch to generation <n> (a rollback, or forward again)
  history [limit]        print the user's shell history, newest first
  events                 print the guest's notify/open/clipboard/transfer pushes and
                         port-listeners snapshots, one JSON per line
  set-clipboard [--type <mime>]… <text>
                         hand the agent a clipboard (its text and types), as the app does on focus
  pick-answer <ref> [file…]   answer a pick event with files; none cancels
  save-answer <ref> [file]    store a save's (or a typed copy's) bytes in file; none cancels
  paste-answer <ref> [file]   answer a clipboard request with file's bytes; none fails it
  wake                   print whether the guest asks the phone to stay awake, one JSON per change
  host-state <json>      send the phone's state (protocol.HostState), as the app does,
                         and hold it until interrupted: a closed stream makes it unknown
  share <file>           share a file, as Android's share sheet does
  share-text <text>      share text
  ls [path] | cat <path> | put <path>   the file provider (put reads stdin)
  patch <path> <offset>  write stdin into a file at offset, as an in-place edit does
  notify-action <id> <key>       choose a guest notification's action
  notify-closed <id> [reason]    dismiss a guest notification (reason 2 unless given)
  hostfs-serve [flags] <dir>  share a directory as the app shares an Android folder,
                         until a stream or the control stream ends
      --name <name>        folder name (default: the directory's)
      --read-only          refuse changes
      --streams <n>        streams to keep open (default 2)
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
	case "updates", "update-check", "update", "update-cancel", "switch-generation", "history":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		return agentUpdates(ctl, cmd, rest)
	case "wake", "host-state":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		return agentHost(ctl, cmd, rest)
	case "events", "set-clipboard", "share-text":
		ctl, err := c.Control()
		if err != nil {
			return err
		}
		defer ctl.Close()
		switch cmd {
		case "events":
			enc := json.NewEncoder(os.Stdout)
			events, ports := ctl.Events(), ctl.PortListeners()
			// Until both close, so neither loses what it buffered when the stream ends.
			for events != nil || ports != nil {
				var v any
				select {
				case ev, ok := <-events:
					if !ok {
						events = nil
						continue
					}
					v = ev
				case p, ok := <-ports:
					if !ok {
						ports = nil
						continue
					}
					v = p
				}
				if err := enc.Encode(v); err != nil {
					return err
				}
			}
			return errors.New("control stream closed")
		case "set-clipboard":
			var types []string
			for len(rest) >= 2 && rest[0] == "--type" {
				types, rest = append(types, rest[1]), rest[2:]
			}
			if types != nil {
				return ctl.SetClipboardTypes(strings.Join(rest, " "), types)
			}
			return ctl.SetClipboard(strings.Join(rest, " "))
		default:
			p, err := ctl.ShareText(strings.Join(rest, " "))
			if err == nil {
				fmt.Println(p)
			}
			return err
		}
	case "pick-answer", "save-answer", "paste-answer":
		return agentTransfer(c, cmd, rest)
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
		// Writes carry their size, so read it all first.
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return c.WriteFile(rest[0], b)
	case "patch":
		if len(rest) != 2 {
			return errUsage
		}
		off, err := strconv.ParseInt(rest[1], 10, 64)
		if err != nil {
			return errUsage
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		f, err := c.Open(rest[0], "rw")
		if err != nil {
			return err
		}
		if err := f.WriteAt(off, b); err != nil {
			f.Close()
			return err
		}
		return f.Commit()
	case "exec":
		return agentExec(c, rest)
	case "hostfs-serve":
		return agentHostFS(c, rest)
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
	case "gui":
		if len(rest) != 0 {
			return errUsage
		}
		conn, err := c.GUI()
		if err != nil {
			return err
		}
		return spliceStdio(conn)
	case "udp":
		return agentUDP(c, rest)
	case "font":
		if len(rest) != 1 {
			return errUsage
		}
		b, err := c.Font()
		if err != nil {
			return err
		}
		return os.WriteFile(rest[0], b, 0o644)
	case "notify-action", "notify-closed":
		return agentNotify(c, cmd, rest)
	default:
		fs.Usage()
		return errUsage
	}
}

// agentNotify answers a guest notification as the app does when the user acts on it.
func agentNotify(c *client.Client, cmd string, args []string) error {
	if len(args) < 1 || len(args) > 2 || (cmd == "notify-action" && len(args) != 2) {
		return errUsage
	}
	id, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}
	ctl, err := c.Control()
	if err != nil {
		return err
	}
	defer ctl.Close()
	if cmd == "notify-action" {
		return ctl.NotifyAction(uint32(id), args[1])
	}
	reason := uint64(protocol.ClosedDismissed)
	if len(args) == 2 {
		if reason, err = strconv.ParseUint(args[1], 10, 32); err != nil {
			return err
		}
	}
	return ctl.NotifyClosed(uint32(id), uint32(reason))
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
	case "update-cancel":
		return ctl.UpdateCancel()
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

func agentHost(ctl *client.Control, cmd string, args []string) error {
	if cmd == "wake" {
		enc := json.NewEncoder(os.Stdout)
		for w := range ctl.Wakes() {
			if err := enc.Encode(w); err != nil {
				return err
			}
		}
		return errors.New("control stream closed")
	}
	if len(args) != 1 {
		return errUsage
	}
	var st protocol.HostState
	if err := json.Unmarshal([]byte(args[0]), &st); err != nil {
		return err
	}
	if err := ctl.HostState(st); err != nil {
		return err
	}
	for range ctl.Configs() {
	}
	return errors.New("control stream closed")
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
	// Wait for the first output: typing earlier races the shell's tty setup.
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

func agentUDP(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("udp", flag.ContinueOnError)
	wait := fs.Duration("wait", 2*time.Second, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		return err
	}
	d, err := c.UDP(port)
	if err != nil {
		return err
	}
	defer d.Close()
	sent := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if err := d.Send(sc.Bytes()); err != nil {
				sent <- err
				return
			}
		}
		sent <- sc.Err()
	}()
	got := make(chan []byte)
	failed := make(chan error, 1)
	go func() {
		buf := make([]byte, protocol.MaxDatagram)
		for {
			n, err := d.Recv(buf)
			if err != nil {
				failed <- err
				return
			}
			got <- append([]byte(nil), buf[:n]...)
		}
	}()
	// Replies may trail the last datagram; stop once none came for wait after stdin ended.
	var idle <-chan time.Time
	for {
		select {
		case err := <-sent:
			if err != nil {
				return err
			}
			sent = nil
			idle = time.After(*wait)
		case b := <-got:
			fmt.Printf("%s\n", b)
			if sent == nil {
				idle = time.After(*wait)
			}
		case err := <-failed:
			return err
		case <-idle:
			return nil
		}
	}
}

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
