package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const controlUsage = `usage: pocketnix control [--serial <serial>] [--package <pkg>] <command> [flags] [args]

Drives a debug build's terminal tabs over adb and reads back what they show,
through the app's ControlProvider: no screen needed, so it works on a locked
phone.

commands:
  dump [--tab <id>] [--lines <n>]
        the VM's state, each tab's last lines and the GUI windows
  tab [--profile <name>] [--timeout <dur>] [--no-enter] [<text>]
        open a terminal tab once the agent is up (60s), type <text> and
        Enter into it, and print its id
  type --tab <id> [--no-enter] <text>
        type <text> and Enter into an open tab
  wait --tab <id> [--timeout <dur>] [--lines <n>] <regexp>
        poll the tab's last lines until <regexp> matches one of them,
        then print them; exit 1 if it doesn't within the timeout (30s)
  menu (--tab <id> | --chip <id> | --main) [<label>...]
        list a menu, one "<label><TAB><enabled|disabled>" line per entry, or
        the page the labels lead to; labels ending at an action run it (e.g.
        "Open link…" <url>). --tab: the terminal's context menu; --chip: the
        tab's long-press menu; --main: the ⋮ menu
  links --tab <id>
        the terminal's Open link… list, one URL per line, newest first
  rename --tab <id> <title>
        give a terminal tab its own title; "" goes back to the shell's
  reopen --tab <id>
        start an ended terminal tab again, in its place
  offers
        the guest listeners offered as port forwards this boot, one
        "port=<guest> state=<offered|dismissed|allowed> phone=<port|->" line each
  offer <port> allow|dismiss
        answer guest port <port>'s offer as its notification does; allow
        returns once the phone port listens
  logcat [--follow]
        the log of every process running as the app's uid, which keeps
        its lines across the app's crashes and restarts; --follow streams it
  stats
        the GUI link's counters and latency histograms, as JSON (see
        pocketnix bench -h)

global flags:
  --serial <serial>   adb device to use (default: adb's only device)
  --package <pkg>     the app's package (default dev.nickolaj.pocketnix.debug)
`

const debugPackage = releasePackage + ".debug"

func runControl(args []string) error {
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, controlUsage) }
	serial := fs.String("serial", "", "")
	pkg := fs.String("package", debugPackage, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errUsage
	}
	c := control{provider{dev: adb.Device{Serial: *serial}, authority: *pkg + ".control"}}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	sub := flag.NewFlagSet("control "+cmd, flag.ContinueOnError)
	sub.Usage = fs.Usage
	switch cmd {
	case "dump":
		tab := sub.Int("tab", -1, "")
		lines := sub.Int("lines", 0, "")
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		out, err := c.dump(*tab, *lines)
		if err != nil {
			return err
		}
		fmt.Print(out)
	case "tab":
		profile := sub.String("profile", "", "")
		timeout := sub.Duration("timeout", 60*time.Second, "")
		noEnter := sub.Bool("no-enter", false, "")
		if err := parseArgs(sub, rest, -1); err != nil {
			return err
		}
		extras := []string{"timeoutMs:l:" + strconv.FormatInt(timeout.Milliseconds(), 10)}
		if text := strings.Join(sub.Args(), " "); text != "" {
			extras = append(extras, inputExtras(text, !*noEnter)...)
		}
		res, err := c.call("tab", *profile, extras...)
		if err != nil {
			return err
		}
		fmt.Println(res["tab"])
	case "type":
		tab := sub.Int("tab", -1, "")
		noEnter := sub.Bool("no-enter", false, "")
		if err := parseArgs(sub, rest, -1); err != nil {
			return err
		}
		if *tab < 0 || sub.NArg() == 0 {
			sub.Usage()
			return errUsage
		}
		_, err := c.call("type", strconv.Itoa(*tab), inputExtras(strings.Join(sub.Args(), " "), !*noEnter)...)
		return err
	case "menu", "links":
		tab := sub.Int("tab", -1, "")
		chip := sub.Int("chip", -1, "")
		main := sub.Bool("main", false, "")
		n := -1
		if cmd == "links" {
			n = 0
		}
		if err := parseArgs(sub, rest, n); err != nil {
			return err
		}
		method, arg, ok := menuTarget(cmd, *tab, *chip, *main)
		if !ok {
			sub.Usage()
			return errUsage
		}
		var extras []string
		for i, label := range sub.Args() {
			extras = append(extras, fmt.Sprintf("label%d:s:%s", i, escapeExtra(label)))
		}
		res, err := c.call(method, arg, extras...)
		if err != nil {
			return err
		}
		key := map[string]string{"menu": "entries", "links": "links"}[cmd]
		enc, ok := res[key]
		if !ok {
			return nil // an action ran
		}
		out, err := url.QueryUnescape(enc)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
		if out != "" {
			fmt.Println(out)
		}
	case "rename", "reopen":
		tab := sub.Int("tab", -1, "")
		n := 0
		if cmd == "rename" {
			n = 1
		}
		if err := parseArgs(sub, rest, n); err != nil {
			return err
		}
		if *tab < 0 {
			sub.Usage()
			return errUsage
		}
		var extras []string
		// No extra is an empty title: the provider's default.
		if cmd == "rename" && sub.Arg(0) != "" {
			extras = append(extras, "title:s:"+escapeExtra(sub.Arg(0)))
		}
		_, err := c.call(cmd, strconv.Itoa(*tab), extras...)
		return err
	case "offers":
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		out, err := c.offers()
		if err != nil {
			return err
		}
		if out != "" {
			fmt.Println(out)
		}
	case "offer":
		if err := parseArgs(sub, rest, 2); err != nil {
			return err
		}
		port, answer := sub.Arg(0), sub.Arg(1)
		if _, err := strconv.ParseUint(port, 10, 16); err != nil || (answer != "allow" && answer != "dismiss") {
			sub.Usage()
			return errUsage
		}
		_, err := c.call("offer", port, "answer:s:"+answer)
		return err
	case "stats":
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		out, err := c.stats()
		if err != nil {
			return err
		}
		fmt.Println(out)
	case "wait":
		tab := sub.Int("tab", -1, "")
		timeout := sub.Duration("timeout", 30*time.Second, "")
		lines := sub.Int("lines", 0, "")
		if err := parseArgs(sub, rest, 1); err != nil {
			return err
		}
		re, err := regexp.Compile(sub.Arg(0))
		if err != nil {
			return err
		}
		if *tab < 0 {
			sub.Usage()
			return errUsage
		}
		out, err := c.wait(*tab, *lines, re, *timeout)
		fmt.Print(out)
		return err
	case "logcat":
		follow := sub.Bool("follow", false, "")
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		return logcat(c.dev, *pkg, *follow, os.Stdout)
	default:
		fs.Usage()
		return errUsage
	}
	return nil
}

// logcat writes the main, system and crash logs of pkg's uid to w: what
// the buffers hold, or with follow, everything until adb exits.
func logcat(dev adb.Device, pkg string, follow bool, w io.Writer) error {
	out, err := dev.Shell("pm list packages -U " + adb.ShellQuote(pkg))
	if err != nil {
		return err
	}
	// The filter is a substring match: "package:<name> uid:<uid>" for each match.
	uid := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "package:"+pkg {
			uid, _ = strings.CutPrefix(f[1], "uid:")
		}
	}
	if uid == "" {
		return fmt.Errorf("%s isn't installed", pkg)
	}
	args := []string{"logcat", "-b", "main,system,crash", "--uid=" + uid}
	if !follow {
		args = append(args, "-d")
	}
	return dev.Stream(w, args...)
}

// menuTarget is the provider method and argument for a menu or links
// command's target flags; ok is false unless exactly one fits the command.
func menuTarget(cmd string, tab, chip int, main bool) (method, arg string, ok bool) {
	set := 0
	for _, b := range []bool{tab >= 0, chip >= 0, main} {
		if b {
			set++
		}
	}
	switch {
	case set != 1:
		return "", "", false
	case tab >= 0:
		return cmd, strconv.Itoa(tab), true
	case cmd != "menu":
		return "", "", false
	case chip >= 0:
		return "chip-menu", strconv.Itoa(chip), true
	default:
		return "main-menu", "", true
	}
}

// parseArgs parses a command's flags and checks it got n arguments (-1: any).
func parseArgs(fs *flag.FlagSet, args []string, n int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if n >= 0 && fs.NArg() != n {
		fs.Usage()
		return errUsage
	}
	return nil
}

// inputExtras are the extras typing text and, if enter, Enter.
func inputExtras(text string, enter bool) []string {
	return []string{"input:s:" + escapeExtra(text), "enter:b:" + strconv.FormatBool(enter)}
}

// escapeExtra escapes an extra's value for `content`, which takes a
// backslash as escaping the next character and splits at unescaped colons.
func escapeExtra(s string) string {
	return strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(s)
}

// control is the app's ControlProvider.
type control struct {
	provider
}

// dump reads the provider's dump: every tab, or tab (if >= 0) alone, with its
// last lines (if > 0; the app's default otherwise).
func (c control) dump(tab, lines int) (string, error) {
	q := url.Values{}
	if tab >= 0 {
		q.Set("tab", strconv.Itoa(tab))
	}
	if lines > 0 {
		q.Set("lines", strconv.Itoa(lines))
	}
	uri := "content://" + c.authority + "/dump"
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	out, err := c.dev.Shell("content read --uri " + adb.ShellQuote(uri))
	if err != nil {
		return "", err
	}
	if msg, ok := strings.CutPrefix(out, "error: "); ok {
		return "", fmt.Errorf("dump: %s", strings.TrimSpace(msg))
	}
	// Anything else that isn't a dump is `content` reporting a failure (no such provider, a refusal).
	if !strings.HasPrefix(out, "state=") {
		return "", fmt.Errorf("dump: %s", strings.TrimSpace(out))
	}
	return out, nil
}

// offers lists the app's port forward offers, one line per guest port.
func (c control) offers() (string, error) {
	res, err := c.call("offers", "")
	if err != nil {
		return "", err
	}
	out, err := url.QueryUnescape(res["offers"])
	if err != nil {
		return "", fmt.Errorf("offers: %w", err)
	}
	return out, nil
}

// stats reads the app's GuiStats snapshot, one line of JSON.
func (c control) stats() (string, error) {
	out, err := c.dev.Shell("content read --uri " + adb.ShellQuote("content://"+c.authority+"/stats"))
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if msg, ok := strings.CutPrefix(out, "error: "); ok {
		return "", fmt.Errorf("stats: %s", msg)
	}
	if !strings.HasPrefix(out, "{") {
		return "", fmt.Errorf("stats: %s", out)
	}
	return out, nil
}

// wait polls tab's dump until a transcript line matches re, and returns the
// last dump either way.
func (c control) wait(tab, lines int, re *regexp.Regexp, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	prefix := fmt.Sprintf("|%d| ", tab)
	for {
		out, err := c.dump(tab, lines)
		if err != nil {
			return "", err
		}
		for _, l := range strings.Split(out, "\n") {
			if t, ok := strings.CutPrefix(l, prefix); ok && re.MatchString(t) {
				return out, nil
			}
		}
		if time.Now().After(deadline) {
			return out, fmt.Errorf("tab %d: no line matched %q within %s", tab, re, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
