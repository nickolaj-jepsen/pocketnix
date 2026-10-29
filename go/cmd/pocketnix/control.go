package main

import (
	"flag"
	"fmt"
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
	default:
		fs.Usage()
		return errUsage
	}
	return nil
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

// inputExtras are the extras typing text and, if enter, Enter. `content`
// takes a backslash as escaping the next character and splits a binding at
// unescaped colons, so the text's backslashes and colons are escaped.
func inputExtras(text string, enter bool) []string {
	text = strings.NewReplacer(`\`, `\\`, ":", `\:`).Replace(text)
	return []string{"input:s:" + text, "enter:b:" + strconv.FormatBool(enter)}
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
