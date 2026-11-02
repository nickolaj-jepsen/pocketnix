package main

import (
	"bytes"
	"context"
	"errors"
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
  wait [--tab <id>] [--timeout <dur>] [--lines <n>] <regexp>
        poll the tab's last lines until <regexp> matches one of them,
        then print them; exit 1 if it doesn't within the timeout (30s).
        Without --tab, match the dump's own lines instead (state=…,
        engine …, gui window …), never a transcript
  close --tab <id>
        close a terminal tab, as its × does: what still runs in it gets
        a hangup
  launch [--timeout <dur>] <app>
        open a guest app as the launcher does (<app>: an id from dump's
        apps=), wait for a new window of it (60s; 0 doesn't wait) and
        print its gui window line; it opens only on a shown display
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
        its lines across the app's crashes and restarts, and the OS's lines
        that name the app; --follow streams it
  lift-limit [--restore]
        take the app out of Android's memory limit until the phone reboots
        (docs/development.md, "Android's memory limit")
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
		var out string
		if *tab < 0 {
			_, out, err = c.waitDump(context.Background(), re, *timeout)
		} else {
			out, err = c.wait(context.Background(), *tab, *lines, re, *timeout)
		}
		fmt.Print(out)
		return err
	case "close":
		tab := sub.Int("tab", -1, "")
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		if *tab < 0 {
			sub.Usage()
			return errUsage
		}
		return c.close(*tab)
	case "launch":
		timeout := sub.Duration("timeout", 60*time.Second, "")
		if err := parseArgs(sub, rest, 1); err != nil {
			return err
		}
		win, err := c.launch(context.Background(), sub.Arg(0), *timeout)
		if win != "" {
			fmt.Println(win)
		}
		return err
	case "logcat":
		follow := sub.Bool("follow", false, "")
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		return logcat(c.dev, *pkg, *follow, os.Stdout)
	case "lift-limit":
		restore := sub.Bool("restore", false, "")
		if err := parseArgs(sub, rest, 0); err != nil {
			return err
		}
		if *restore {
			return restoreLimit(c.dev, os.Stdout)
		}
		return liftLimit(context.Background(), c.dev, *pkg, os.Stdout)
	default:
		fs.Usage()
		return errUsage
	}
	return nil
}

// logcat writes the main, system, crash and events logs of pkg's uid to w,
// with the other uids' lines that name pkg ([appLog]): what the buffers hold,
// or with follow, everything until adb exits.
func logcat(dev adb.Device, pkg string, follow bool, w io.Writer) error {
	uid, err := appUID(dev, pkg)
	if err != nil {
		return err
	}
	args := []string{"logcat", "-b", "main,system,crash,events", "-v", "threadtime,uid"}
	if !follow {
		args = append(args, "-d")
	}
	log := newAppLog(pkg, uid, w)
	err = dev.Stream(log, args...)
	if ferr := log.flush(); err == nil {
		err = ferr
	}
	return err
}

// threadtimeUIDLine splits a `-v threadtime,uid` line into its uid, pid, tag and message.
var threadtimeUIDLine = regexp.MustCompile(`^\S+ \S+ +(\S+) +(\d+) +\d+ \S (\S*?) *: ?(.*)$`)

// appLog is a filter over a device's whole log that writes only the lines about
// one app, so other apps' text stays out of what it prints. A line is about the
// app when its uid logged it, or when another uid's line names its package or
// uid. MemoryLimiter's kill names only a pid, so that tag also matches the
// pids seen so far.
type appLog struct {
	w       io.Writer
	uids    map[string]bool
	names   *regexp.Regexp
	pids    map[string]bool
	partial []byte
}

// newAppLog filters for pkg, with uids as [appUID] gives them, onto w.
func newAppLog(pkg, uids string, w io.Writer) *appLog {
	l := &appLog{w: w, uids: map[string]bool{}, pids: map[string]bool{}}
	var alts []string
	for _, uid := range strings.Split(uids, ",") {
		l.uids[uid] = true
		alts = append(alts, regexp.QuoteMeta(uid))
	}
	// The edges keep dev.x from matching dev.x.debug or dev.x.MainActivity.
	l.names = regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(pkg) + `($|[^\w.]|\.(\s|$))|(?i:\buid[= ])(` + strings.Join(alts, "|") + `)\b`)
	return l
}

var limiterPid = regexp.MustCompile(`\bpid=(\d+)`)

func (l *appLog) keep(line string) bool {
	m := threadtimeUIDLine.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	uid, pid, tag, msg := m[1], m[2], m[3], m[4]
	if l.uids[uid] {
		l.pids[pid] = true
		return true
	}
	if tag == "adbd" { // It echoes the shell commands sent to the phone, these tools' included.
		return false
	}
	if l.names.MatchString(msg) {
		if p := limiterPid.FindStringSubmatch(msg); tag == "MemoryLimiter" && p != nil {
			l.pids[p[1]] = true
		}
		return true
	}
	if tag == "MemoryLimiter" {
		for _, word := range strings.Fields(msg) {
			if l.pids[word] {
				return true
			}
		}
	}
	return false
}

func (l *appLog) Write(b []byte) (int, error) {
	l.partial = append(l.partial, b...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := l.partial[:i+1]
		if l.keep(strings.TrimRight(string(line), "\r\n")) {
			if _, err := l.w.Write(line); err != nil {
				return 0, err
			}
		}
		l.partial = l.partial[i+1:]
	}
}

// flush writes a last line that came without its newline.
func (l *appLog) flush() error {
	if len(l.partial) == 0 || !l.keep(string(l.partial)) {
		return nil
	}
	_, err := l.w.Write(append(l.partial, '\n'))
	return err
}

// logcatSince writes the lines pkg's uid logged since since (the device's
// `date +%s`) to w, each stamped with its epoch time.
func logcatSince(dev adb.Device, pkg, since string, w io.Writer) error {
	uid, err := appUID(dev, pkg)
	if err != nil {
		return err
	}
	return dev.Stream(w, "logcat", "-b", "main,system,crash", "--uid="+uid, "-d", "-v", "epoch", "-T", since+".000")
}

// appUID is installed package pkg's uids, comma-separated with one per
// Android user, as logcat's --uid takes them.
func appUID(dev adb.Device, pkg string) (string, error) {
	out, err := dev.Shell("pm list packages -U " + adb.ShellQuote(pkg))
	if err != nil {
		return "", err
	}
	// The filter is a substring match: "package:<name> uid:<uid>" for each match.
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "package:"+pkg {
			if uid, ok := strings.CutPrefix(f[1], "uid:"); ok {
				return uid, nil
			}
		}
	}
	return "", fmt.Errorf("%s isn't installed", pkg)
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

// controlPoll is how often a wait reads the dump again.
var controlPoll = 500 * time.Millisecond

// errNotYet is a wait's error when its condition didn't hold within the timeout.
var errNotYet = errors.New("not within the timeout")

// until polls dump(tab, lines) until done holds for it, and returns the last
// dump either way: an error wrapping errNotYet after timeout, or ctx's error.
func (c control) until(ctx context.Context, tab, lines int, timeout time.Duration, done func(dump string) bool) (string, error) {
	return c.untilEvery(ctx, tab, lines, timeout, controlPoll, done)
}

// untilEvery is until, reading the dump every every.
func (c control) untilEvery(ctx context.Context, tab, lines int, timeout, every time.Duration, done func(dump string) bool) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.dump(tab, lines)
		if err != nil {
			return "", err
		}
		if done(out) {
			return out, nil
		}
		if time.Now().After(deadline) {
			return out, fmt.Errorf("%w (%s)", errNotYet, timeout)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(every):
		}
	}
}

// wait polls tab's dump until a transcript line matches re, and returns the
// last dump either way.
func (c control) wait(ctx context.Context, tab, lines int, re *regexp.Regexp, timeout time.Duration) (string, error) {
	out, err := c.until(ctx, tab, lines, timeout, func(dump string) bool { return tabLine(dump, tab, re) != nil })
	if errors.Is(err, errNotYet) {
		err = fmt.Errorf("tab %d: no line matched %q within %s", tab, re, timeout)
	}
	return out, err
}

// tabLine is the submatches of the last line of tab's transcript in dump
// that re matches, or nil.
func tabLine(dump string, tab int, re *regexp.Regexp) []string {
	prefix := fmt.Sprintf("|%d| ", tab)
	var m []string
	for _, l := range strings.Split(dump, "\n") {
		if t, ok := strings.CutPrefix(l, prefix); ok {
			if sm := re.FindStringSubmatch(t); sm != nil {
				m = sm
			}
		}
	}
	return m
}

// ownLine is the submatches of the first of dump's own lines (the state,
// engine and gui lines, not a transcript's) that re matches, or nil.
func ownLine(dump string, re *regexp.Regexp) []string {
	for _, l := range strings.Split(dump, "\n") {
		// Transcript lines are "|<tab>| …", and the updates log's "|log| …".
		if strings.HasPrefix(l, "|") {
			continue
		}
		if m := re.FindStringSubmatch(l); m != nil {
			return m
		}
	}
	return nil
}

// waitDump polls the dump until one of its own lines matches re, and
// returns that line's submatches and the last dump.
func (c control) waitDump(ctx context.Context, re *regexp.Regexp, timeout time.Duration) ([]string, string, error) {
	var m []string
	// One transcript line per tab keeps the dump small; its own lines are all there.
	out, err := c.until(ctx, -1, 1, timeout, func(dump string) bool {
		m = ownLine(dump, re)
		return m != nil
	})
	if errors.Is(err, errNotYet) {
		err = fmt.Errorf("no dump line matched %q within %s", re, timeout)
	}
	return m, out, err
}

// close closes terminal tab id, hanging up what runs in it.
func (c control) close(tab int) error {
	_, err := c.call("close", strconv.Itoa(tab))
	return err
}

// guiWindow matches a dump's window line: its id and app.
var guiWindow = regexp.MustCompile(`^gui window (\d+) '.*' app=(\S*) launch=`)

// windows are the ids of dump's GUI windows of app.
func windows(dump, app string) map[string]bool {
	ids := map[string]bool{}
	for _, l := range strings.Split(dump, "\n") {
		if m := guiWindow.FindStringSubmatch(l); m != nil && m[2] == app {
			ids[m[1]] = true
		}
	}
	return ids
}

// launch opens app as the launcher does (ControlActivity's DEBUG_LAUNCH)
// and, unless timeout is 0, waits for a window of it that wasn't there
// before and returns its dump line.
func (c control) launch(ctx context.Context, app string, timeout time.Duration) (string, error) {
	before, err := c.dump(-1, 1)
	if err != nil {
		return "", err
	}
	had := windows(before, app)
	pkg := strings.TrimSuffix(c.authority, ".control")
	// The extra mirrors ControlActivity.EXTRA_APP.
	out, err := amStart(c.dev, pkg, actionLaunch, "--es app "+adb.ShellQuote(app))
	if err != nil {
		return "", err
	}
	// am reports a missing activity or package on stdout, exiting 0.
	if strings.Contains(out, "Error") {
		return "", fmt.Errorf("launch %s: %s", app, strings.TrimSpace(out))
	}
	if timeout == 0 {
		return "", nil
	}
	var line string
	_, err = c.until(ctx, -1, 1, timeout, func(dump string) bool {
		for _, l := range strings.Split(dump, "\n") {
			if m := guiWindow.FindStringSubmatch(l); m != nil && m[2] == app && !had[m[1]] {
				line = l
				return true
			}
		}
		return false
	})
	if errors.Is(err, errNotYet) {
		err = fmt.Errorf("launch %s: no new window within %s (is the phone unlocked, with pocketnix in front?)", app, timeout)
	}
	return line, err
}
