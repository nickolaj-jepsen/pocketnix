// Command pocketnix-guest lets guest programs reach Android through the
// pocketnix agent: notifications, opening URLs and files, the clipboard, the
// share sheet and file pickers. Installed as xdg-open, it behaves as that.
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
	"github.com/nickolaj-jepsen/pocketnix/go/internal/notifyd"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

const usage = `usage: pocketnix-guest <command> [args]

commands:
  notify [--title <title>] <body>…  post an Android notification
  open <url|file>                   open a URL, or a file under the app's
                                    file provider root, on Android
  copy [text]                       set Android's clipboard (stdin without text)
  copy --type <mime>                set it to stdin's bytes, of that type
  paste                             print the clipboard Android last handed over
  paste --type <mime>             write its data of that type (or type/*)
  paste --list-types                print its types
  share <file>… | --text [text]     offer files under the file provider root,
                                    or text (stdin without), to Android's share sheet
  pick [--type <mime>]… [--multiple] [--dest <dir>]
                                    pick files on Android; prints their paths
                                    (in the share directory, or moved to dest)
  save [--name <name>] [--type <mime>] <file>
                                    store a file on Android, where you pick
  notifyd                           serve org.freedesktop.Notifications on the
                                    session bus (notify-send's daemon)
  portal                           serve the FileChooser portal backend on
                                    the session bus

As xdg-open it opens. $POCKETNIX_GUEST_SOCKET overrides the agent's socket.
`

// version is set by the build (nix/go.nix).
var version = "dev"

var errUsage = errors.New("usage")

func main() {
	args := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "xdg-open":
		args = append([]string{"open"}, args...)
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
	case "notifyd":
		if len(rest) != 0 {
			return errUsage
		}
		return notifyd.Run(socket(), version)
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
		if t, typed := typeFlag(rest); t != "" {
			if !isText(t) {
				return copyTyped(t, typed)
			}
			rest = typed
		}
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
		if len(rest) == 1 && rest[0] == "--list-types" {
			return listTypes()
		}
		if t, typed := typeFlag(rest); t != "" && !isText(t) {
			return pasteTyped(t, typed)
		}
		text, err := send(protocol.GuestRequest{Type: protocol.GuestPaste})
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	case "share":
		return share(rest)
	case "pick":
		return pick(rest)
	case "save":
		return save(rest)
	case "portal":
		return portal(rest)
	default:
		return errUsage
	}
}
