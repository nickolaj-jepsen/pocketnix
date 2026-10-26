// Command pocketnix-guest lets guest programs reach Android through the
// pocketnix agent: notifications, opening URLs and files, and the clipboard.
// Installed as xdg-open or notify-send, it behaves as those.
package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

const usage = `usage: pocketnix-guest <command> [args]

commands:
  notify [--title <title>] <body>…  post an Android notification
  open <url|file>                   open a URL, or a file under the app's
                                    file provider root, on Android
  copy [text]                       set Android's clipboard (stdin without text)
  paste                             print the clipboard Android last handed over

As xdg-open it opens; as notify-send it notifies (SUMMARY [BODY], other
options ignored). $POCKETNIX_GUEST_SOCKET overrides the agent's socket.
`

var errUsage = errors.New("usage")

func main() {
	args := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "xdg-open":
		args = append([]string{"open"}, args...)
	case "notify-send":
		args = append([]string{"notify-send"}, args...)
	}
	err := run(args)
	if errors.Is(err, errUsage) {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, filepath.Base(os.Args[0])+":", err)
		os.Exit(1)
	}
}

func socket() string {
	if s := os.Getenv("POCKETNIX_GUEST_SOCKET"); s != "" {
		return s
	}
	return protocol.GuestSocket
}

func send(req protocol.GuestRequest) (string, error) { return client.Guest(socket(), req) }

func run(args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "notify":
		var title string
		if len(rest) >= 2 && (rest[0] == "--title" || rest[0] == "-t") {
			title, rest = rest[1], rest[2:]
		}
		if len(rest) == 0 && title == "" {
			return errUsage
		}
		_, err := send(protocol.GuestRequest{Type: protocol.GuestNotify, Title: title, Body: strings.Join(rest, " ")})
		return err
	case "notify-send":
		summary, body, err := notifySendArgs(rest)
		if err != nil {
			return err
		}
		_, err = send(protocol.GuestRequest{Type: protocol.GuestNotify, Title: summary, Body: body})
		return err
	case "open":
		if len(rest) != 1 {
			return errUsage
		}
		req := protocol.GuestRequest{Type: protocol.GuestOpen}
		if u, err := url.Parse(rest[0]); err == nil && len(u.Scheme) > 1 && u.Scheme != "file" {
			req.URL = rest[0]
		} else {
			p := rest[0]
			if u != nil && u.Scheme == "file" {
				p = u.Path
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			req.Path = abs
		}
		_, err := send(req)
		return err
	case "copy":
		text := strings.Join(rest, " ")
		if len(rest) == 0 {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			text = string(b)
		}
		_, err := send(protocol.GuestRequest{Type: protocol.GuestCopy, Text: text})
		return err
	case "paste":
		text, err := send(protocol.GuestRequest{Type: protocol.GuestPaste})
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	default:
		return errUsage
	}
}

// notifySendArgs takes libnotify's notify-send arguments apart: SUMMARY and
// an optional BODY; the options are accepted and ignored.
func notifySendArgs(args []string) (summary, body string, err error) {
	withValue := map[string]bool{
		"-u": true, "--urgency": true, "-t": true, "--expire-time": true, "-a": true, "--app-name": true,
		"-i": true, "--icon": true, "-c": true, "--category": true, "-h": true, "--hint": true,
		"-r": true, "--replace-id": true, "-A": true, "--action": true,
	}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case withValue[a]:
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// --opt=value, or a flag without a value (-e, -p, -w).
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		return "", "", errors.New("no summary specified")
	}
	summary = pos[0]
	if len(pos) > 1 {
		body = strings.Join(pos[1:], " ")
	}
	return summary, body, nil
}
