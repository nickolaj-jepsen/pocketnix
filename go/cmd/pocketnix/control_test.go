package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// fakeControl is the app's ControlProvider. `content read` of the dump
// answers the state's control-dump file verbatim when there is one, and
// `type` appends its input, and "<enter>" for Enter, to control-typed.
// Values are decoded as the phone's `content` does: a backslash escapes the
// next character.
//
// With a "control" directory (newAppModel) it models the app instead: its
// VM state, tabs whose transcripts get what is typed and what the guest
// answers (fakeReaction), GUI windows that launches open, port forward
// offers of guest listeners, and the GUI counters.
func fakeControl(state, line string) (string, int) {
	w := shellWords(line)
	model := isDir(filepath.Join(state, "control"))
	switch {
	case len(w) == 4 && w[0] == "content" && w[1] == "read":
		u, _ := url.Parse(w[3])
		if model && u.Path == "/stats" {
			return fakeStats(state), 0
		}
		if b, err := os.ReadFile(filepath.Join(state, "control-dump")); err == nil {
			return string(b), 0
		}
		if model {
			return fakeDump(state, u.Query()), 0
		}
		return "Error while accessing provider:dev.nickolaj.pocketnix.debug.control\n", 0
	case len(w) >= 6 && w[0] == "content" && w[1] == "call" && (w[5] == "type" || w[5] == "tab"):
		input, enter := "", false
		for i, a := range w {
			if i == 0 || w[i-1] != "--extra" {
				continue
			}
			if v, ok := strings.CutPrefix(a, "input:s:"); ok {
				input = contentEscape.ReplaceAllString(v, "$1")
			}
			if a == "enter:b:true" {
				enter = true
			}
		}
		typed := input
		if enter {
			typed += "<enter>"
		}
		f, _ := os.OpenFile(filepath.Join(state, "control-typed"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(typed + "\n")
		f.Close()
		if !model {
			return "Result: Bundle[{ok=true}]\n", 0
		}
		if w[5] == "tab" {
			if !strings.Contains(fakeHead(state), " phase=Running") {
				return "Result: Bundle[{error=the agent wasn't up within 60000 ms}]\n", 0
			}
			id := fakeNewTab(state)
			fakeType(state, id, input, enter)
			return fmt.Sprintf("Result: Bundle[{tab=%d, ok=true}]\n", id), 0
		}
		id, _ := strconv.Atoi(w[7])
		if !fakeTabExists(state, id) {
			return fmt.Sprintf("Result: Bundle[{error=no tab %d}]\n", id), 0
		}
		fakeType(state, id, input, enter)
		return "Result: Bundle[{ok=true}]\n", 0
	case model && len(w) == 8 && w[0] == "content" && w[1] == "call" && w[5] == "close":
		id, _ := strconv.Atoi(w[7])
		switch {
		case id == 0:
			return "Result: Bundle[{error=the console can't be closed}]\n", 0
		case !fakeTabExists(state, id):
			return fmt.Sprintf("Result: Bundle[{error=no tab %d}]\n", id), 0
		}
		os.Remove(fakeTabPath(state, id))
		os.Remove(fakeTabPath(state, id) + ".ended")
		return "Result: Bundle[{ok=true}]\n", 0
	case len(w) == 6 && w[0] == "content" && w[1] == "call" && w[5] == "offers":
		b, _ := os.ReadFile(filepath.Join(state, "control-offers"))
		if model {
			b = []byte(fakeOffers(state))
		}
		return "Result: Bundle[{offers=" + url.QueryEscape(string(b)) + ", ok=true}]\n", 0
	case len(w) >= 6 && w[0] == "content" && w[1] == "call" && w[5] == "offer":
		os.WriteFile(filepath.Join(state, "control-offer"), []byte(strings.Join(w[6:], " ")), 0o644)
		if model && len(w) == 10 && w[9] == "answer:s:allow" {
			port := w[7]
			if !exists(filepath.Join(state, "control", "listening", port)) {
				return "Result: Bundle[{error=nothing listens on guest port " + port + "}]\n", 0
			}
			putFile(filepath.Join(state, "control", "allowed", port), port)
		}
		return "Result: Bundle[{ok=true}]\n", 0
	}
	return "fake adb: unknown control command " + line, 127
}

var contentEscape = regexp.MustCompile(`\\(.)`)

// shellWords splits a command line the way sh does for adb.ShellQuote's quoting.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	in, started, escaped := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && !in:
			escaped, started = true, true
		case r == '\'':
			in, started = !in, true
		case r == ' ' && !in:
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		words = append(words, cur.String())
	}
	return words
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func putFile(p, s string) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(s), 0o644)
}

func appendFile(p, s string) {
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	f.WriteString(s)
	f.Close()
}

// fakeRunningHead is a dump's lines before its tabs for a running VM with
// a GPU and guest RAM.
const fakeRunningHead = `state=Running phase=Running(host=phone) link=Up host=phone
theme background=#1d1f21 source=default
setup=null managed=null
engine gpu=gfxstream gpuAsked=true gpuRefused=false guestRam=true virtioSnd=false audio=off`

func fakeHead(state string) string {
	if b, err := os.ReadFile(filepath.Join(state, "control", "head")); err == nil {
		return string(b)
	}
	return fakeRunningHead
}

// fakeBootDumps is how many dumps after a reboot show the guest booting.
const fakeBootDumps = 3

// fakeBootingHead is the head a dump shows, and counts down a reboot's boot.
func fakeBootingHead(state string) string {
	p := filepath.Join(state, "control", "booting")
	b, err := os.ReadFile(p)
	n, _ := strconv.Atoi(string(b))
	if err != nil || n <= 0 {
		os.Remove(p)
		return fakeHead(state)
	}
	putFile(p, strconv.Itoa(n-1))
	return strings.Replace(fakeHead(state), "state=Running phase=Running(host=phone) link=Up", "state=Starting phase=Booting link=Down", 1)
}

func fakeTabPath(state string, id int) string {
	return filepath.Join(state, "control", "tabs", strconv.Itoa(id))
}

func fakeTabExists(state string, id int) bool { return exists(fakeTabPath(state, id)) }

// fakeTabs are the open tabs' ids, in order.
func fakeTabs(state string) []int {
	entries, _ := os.ReadDir(filepath.Join(state, "control", "tabs"))
	var ids []int
	for _, e := range entries {
		if id, err := strconv.Atoi(e.Name()); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// fakeNewTab opens a tab with the next id, as Tabs.open does.
func fakeNewTab(state string) int {
	p := filepath.Join(state, "control", "next-tab")
	b, _ := os.ReadFile(p)
	id, _ := strconv.Atoi(string(b))
	id = max(id, 1)
	putFile(p, strconv.Itoa(id+1))
	putFile(fakeTabPath(state, id), "")
	return id
}

// fakeReaction is what the guest does when a line typed into a tab, or a
// launched app, matches. Output lines, files and paths are expanded with
// Typed's submatches ($1, ${1}); paths are under the device state.
type fakeReaction struct {
	// Typed is a regexp on a line typed into a tab; Launch an app id.
	Typed  string `json:"typed,omitempty"`
	Launch string `json:"launch,omitempty"`
	// Output goes into the tab's transcript.
	Output []string          `json:"output,omitempty"`
	Write  map[string]string `json:"write,omitempty"`
	Append map[string]string `json:"append,omitempty"`
	Remove []string          `json:"remove,omitempty"`
	// Reboot ends every terminal tab, and the console says so as AvfEngine does.
	Reboot bool `json:"reboot,omitempty"`
}

func fakeReactions(state string) []fakeReaction {
	b, _ := os.ReadFile(filepath.Join(state, "control", "reactions.json"))
	var rs []fakeReaction
	json.Unmarshal(b, &rs)
	return rs
}

// apply does r for a match m of re in s (nil re: no submatches), into tab
// (-1: none).
func (r fakeReaction) apply(state string, tab int, re *regexp.Regexp, s string, m []int) {
	expand := func(t string) string {
		if re == nil {
			return t
		}
		return string(re.ExpandString(nil, t, s, m))
	}
	for _, o := range r.Output {
		if tab >= 0 {
			appendFile(fakeTabPath(state, tab), expand(o)+"\n")
		}
	}
	for p, v := range r.Write {
		putFile(filepath.Join(state, expand(p)), expand(v))
	}
	for p, v := range r.Append {
		appendFile(filepath.Join(state, expand(p)), expand(v))
	}
	for _, p := range r.Remove {
		os.Remove(filepath.Join(state, expand(p)))
	}
	if r.Reboot {
		for _, id := range fakeTabs(state) {
			if id != 0 {
				putFile(fakeTabPath(state, id)+".ended", "")
			}
		}
		appendFile(fakeTabPath(state, 0), "[pocketnix: the guest rebooted; starting it again]\n")
		putFile(filepath.Join(state, "control", "booting"), strconv.Itoa(fakeBootDumps))
	}
}

// fakeType types into tab as Vm.debugType does: a line with Enter is echoed
// to the transcript, and the guest reacts to it; "exit" ends the shell.
func fakeType(state string, tab int, input string, enter bool) {
	if !enter || exists(fakeTabPath(state, tab)+".ended") {
		return
	}
	appendFile(filepath.Join(state, "control", "typed"), fmt.Sprintf("%d\t%s\n", tab, input))
	appendFile(fakeTabPath(state, tab), "$ "+input+"\n")
	if strings.TrimSpace(input) == "exit" {
		putFile(fakeTabPath(state, tab)+".ended", "")
		return
	}
	for _, r := range fakeReactions(state) {
		if r.Typed == "" {
			continue
		}
		re := regexp.MustCompile(r.Typed)
		if m := re.FindStringSubmatchIndex(input); m != nil {
			r.apply(state, tab, re, input, m)
			return
		}
	}
}

// fakeLaunch opens a window for the DEBUG_LAUNCH line's app, if the guest
// lists it, and does the reactions to it.
func fakeLaunch(state string, w []string) {
	app := ""
	for i, a := range w {
		if a == "--es" && i+2 < len(w) && w[i+1] == "app" {
			app = w[i+2]
		}
	}
	b, _ := os.ReadFile(filepath.Join(state, "control", "apps"))
	if !slicesContainLine(string(b), app) {
		return
	}
	p := filepath.Join(state, "control", "next-window")
	n, _ := os.ReadFile(p)
	id, _ := strconv.Atoi(string(n))
	id = max(id, 1)
	putFile(p, strconv.Itoa(id+1))
	putFile(filepath.Join(state, "control", "windows", app), strconv.Itoa(id))
	for _, r := range fakeReactions(state) {
		if r.Launch == app {
			r.apply(state, -1, nil, "", nil)
		}
	}
}

func slicesContainLine(s, v string) bool {
	for _, l := range strings.Split(s, "\n") {
		if l == v {
			return true
		}
	}
	return false
}

// fakeDump is Vm.debugDump's text for the model.
func fakeDump(state string, q url.Values) string {
	appendFile(filepath.Join(state, "control", "dump-reads"), q.Encode()+"\n")
	lines := 40
	if n, err := strconv.Atoi(q.Get("lines")); err == nil {
		lines = n
	}
	only := -1
	if t := q.Get("tab"); t != "" {
		only, _ = strconv.Atoi(t)
		if !fakeTabExists(state, only) {
			return fmt.Sprintf("error: no tab %d\n", only)
		}
	}
	var out strings.Builder
	out.WriteString(fakeBootingHead(state) + "\n")
	for _, id := range fakeTabs(state) {
		if only >= 0 && id != only {
			continue
		}
		kind, title := "Pty(profile=shell)", "fish"
		if id == 0 {
			kind, title = "Console", "console"
		}
		fmt.Fprintf(&out, "tab %d %s '%s' running=%t\n", id, kind, title, !exists(fakeTabPath(state, id)+".ended"))
		b, _ := os.ReadFile(fakeTabPath(state, id))
		text := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		if len(text) > lines {
			text = text[len(text)-lines:]
		}
		for _, l := range text {
			fmt.Fprintf(&out, "|%d| %s\n", id, l)
		}
	}
	if only < 0 {
		if gui, err := os.ReadFile(filepath.Join(state, "control", "gui")); err == nil {
			apps, _ := os.ReadFile(filepath.Join(state, "control", "apps"))
			ids := strings.Fields(string(apps))
			fmt.Fprintf(&out, "gui %s\ngui apps=[%s] recent=[] launcher=true launches=[]\n", strings.TrimSpace(string(gui)), strings.Join(ids, ", "))
			for _, app := range fakeWindows(state) {
				n, _ := os.ReadFile(filepath.Join(state, "control", "windows", app))
				fmt.Fprintf(&out, "gui window %s '%s' app=%s launch= tab= host=WindowHost shown=true settings=null min=0x0@1.0 geometry=800x600 zoom=null seq=3 sent=null surfaces=[%s:toplevel:800x600]\n", n, app, app, n)
			}
		}
	}
	return out.String()
}

func fakeWindows(state string) []string {
	entries, _ := os.ReadDir(filepath.Join(state, "control", "windows"))
	var apps []string
	for _, e := range entries {
		apps = append(apps, e.Name())
	}
	return apps
}

// fakeStats is GuiStats' snapshot: each read, every open window has drawn
// one more frame, from guest RAM when the state has control/pages.
func fakeStats(state string) string {
	p := filepath.Join(state, "control", "counters.json")
	b, _ := os.ReadFile(p)
	counters := map[string]int64{}
	json.Unmarshal(b, &counters)
	for range fakeWindows(state) {
		counters["commits"]++
		counters["pixelBytes"] += 4096
		if exists(filepath.Join(state, "control", "pages")) {
			counters["pagesCommits"]++
		} else {
			counters["zstdCommits"]++
		}
	}
	b, _ = json.Marshal(counters)
	putFile(p, string(b))
	return `{"source":"app","counters":` + string(b) + `,"histograms":{}}` + "\n"
}

// fakeOffers are the model's offers: one per guest listener, allowed
// forwards included, and an allowed forward ends with its listener.
func fakeOffers(state string) string {
	entries, _ := os.ReadDir(filepath.Join(state, "control", "listening"))
	var lines []string
	for _, e := range entries {
		port := e.Name()
		if b, err := os.ReadFile(filepath.Join(state, "control", "allowed", port)); err == nil {
			lines = append(lines, "port="+port+" state=allowed phone="+string(b))
		} else {
			lines = append(lines, "port="+port+" state=offered phone=-")
		}
	}
	return strings.Join(lines, "\n")
}

// fakePhoneConnect connects to a phone port: an allowed forward's guest
// listener answers what its listening file holds, once.
func fakePhoneConnect(state, phone string) (string, int) {
	entries, _ := os.ReadDir(filepath.Join(state, "control", "allowed"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(state, "control", "allowed", e.Name()))
		listener := filepath.Join(state, "control", "listening", e.Name())
		if string(b) == phone {
			answer, err := os.ReadFile(listener)
			if err != nil {
				return "", 1
			}
			os.Remove(listener)
			return string(answer), 0
		}
	}
	return "nc: connect: Connection refused\n", 1
}

func testControl(t *testing.T) (control, *fakeDevice) {
	d := newFakeDevice(t)
	return control{provider{dev: adb.Device{}, authority: debugPackage + ".control"}}, d
}

func (d *fakeDevice) setDump(s string) {
	os.WriteFile(filepath.Join(d.state, "control-dump"), []byte(s), 0o644)
}

// newAppModel makes the fake app a model with a running VM and its console.
func (d *fakeDevice) newAppModel() {
	putFile(fakeTabPath(d.state, 0), "<<< Welcome to NixOS >>>\n")
}

// setHead replaces the model's state, theme, setup and engine lines.
func (d *fakeDevice) setHead(head string) { putFile(filepath.Join(d.state, "control", "head"), head) }

// setGUI gives the model a GUI connection, with these apps in its list.
func (d *fakeDevice) setGUI(apps ...string) {
	putFile(filepath.Join(d.state, "control", "gui"), "connected=true ready=true instance=abc hashes ok=0 bad=0")
	putFile(filepath.Join(d.state, "control", "apps"), strings.Join(apps, "\n"))
}

func (d *fakeDevice) react(rs ...fakeReaction) {
	b, _ := json.Marshal(rs)
	putFile(filepath.Join(d.state, "control", "reactions.json"), string(b))
}

// typed is what was typed into the model's tabs with Enter, by tab.
func (d *fakeDevice) typed() map[int][]string {
	b, _ := os.ReadFile(filepath.Join(d.state, "control", "typed"))
	out := map[int][]string{}
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if id, text, ok := strings.Cut(l, "\t"); ok {
			n, _ := strconv.Atoi(id)
			out[n] = append(out[n], text)
		}
	}
	return out
}

func TestControlTypesTextVerbatim(t *testing.T) {
	_, d := testControl(t)
	if err := runControl([]string{"type", "--tab", "2", "printf", `'a:b\n'`}); err != nil {
		t.Fatal(err)
	}
	if err := runControl([]string{"type", "--tab", "2", "--no-enter", `C:\`}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d.state, "control-typed"))
	if got, want := string(b), "printf 'a:b\\n'<enter>\nC:\\\n"; got != want {
		t.Errorf("the app got input %q, want %q", got, want)
	}
}

func TestControlMenuTargets(t *testing.T) {
	for _, c := range []struct {
		cmd       string
		tab, chip int
		main      bool
		method    string
		arg       string
	}{
		{"menu", 3, -1, false, "menu", "3"},
		{"menu", -1, 0, false, "chip-menu", "0"},
		{"menu", -1, -1, true, "main-menu", ""},
		{"links", 3, -1, false, "links", "3"},
		{"links", -1, 3, false, "", ""},
		{"menu", 3, 3, false, "", ""},
		{"menu", -1, -1, false, "", ""},
	} {
		method, arg, ok := menuTarget(c.cmd, c.tab, c.chip, c.main)
		if ok != (c.method != "") || method != c.method || arg != c.arg {
			t.Errorf("%s tab=%d chip=%d main=%v: got %q %q %v, want %q %q", c.cmd, c.tab, c.chip, c.main, method, arg, ok, c.method, c.arg)
		}
	}
}

func TestControlWait(t *testing.T) {
	c, d := testControl(t)
	d.setDump("state=Running\ntab 2 Pty(profile=shell) 'shell' running=true\n|2| $ echo done-$((1+1))\n")
	if _, err := c.wait(context.Background(), 2, 0, regexp.MustCompile(`^done-2$`), 0); err == nil {
		t.Error("matched the typed command line, not its output")
	}
	d.setDump("state=Running\ntab 2 Pty(profile=shell) 'shell' running=true\n|2| $ echo done-$((1+1))\n|2| done-2\n")
	if _, err := c.wait(context.Background(), 2, 0, regexp.MustCompile(`^done-2$`), time.Second); err != nil {
		t.Error(err)
	}
}

// Without --tab, wait matches the dump's own lines and never a transcript.
func TestControlWaitDump(t *testing.T) {
	_, d := testControl(t)
	d.setDump("state=Starting phase=Booting link=Down host=null\ntab 1 Pty(profile=shell) 'shell' running=true\n|1| state=Running phase=Running\n")
	if err := runControl([]string{"wait", "--timeout", "0", `^state=\S+ phase=Running`}); err == nil {
		t.Error("matched a transcript line")
	}
	d.setDump("state=Running phase=Running(host=phone) link=Up host=phone\ngui window 3 'Cube' app=org.example.Cube launch= tab= host=WindowHost shown=true\n")
	for _, re := range []string{`^state=\S+ phase=Running`, `^gui window \d+ .* app=org\.example\.Cube `} {
		if out, err := stdoutOf(t, func() error { return runControl([]string{"wait", "--timeout", "1s", re}) }); err != nil || !strings.HasPrefix(out, "state=") {
			t.Errorf("wait %q: %v\n%s", re, err, out)
		}
	}
}

func TestControlDumpErrors(t *testing.T) {
	c, d := testControl(t)
	if _, err := c.dump(-1, 0); err == nil {
		t.Errorf("no provider: got %v", err)
	}
	d.setDump("error: no tab 9\n")
	if _, err := c.dump(9, 0); err == nil || !strings.Contains(err.Error(), "no tab 9") {
		t.Errorf("missing tab: got %v", err)
	}
}

func TestControlLogcatReadsTheAppsUid(t *testing.T) {
	newFakeDevice(t)
	for pkg, uid := range map[string]string{releasePackage: "10100", debugPackage: "10200"} {
		var out strings.Builder
		if err := logcat(adb.Device{}, pkg, false, &out); err != nil {
			t.Fatal(err)
		}
		if want := "from uid " + uid + "\n"; !strings.HasSuffix(out.String(), want) {
			t.Errorf("%s: logcat printed %q, want the lines of uid %s", pkg, out.String(), uid)
		}
	}
	if err := logcat(adb.Device{}, "dev.nickolaj.missing", false, io.Discard); err == nil || !strings.Contains(err.Error(), "dev.nickolaj.missing") {
		t.Errorf("a package that isn't installed: got %v", err)
	}
}

func TestControlLogcatSince(t *testing.T) {
	d := newFakeDevice(t)
	d.write("logcat", []byte("1761999000.000  1234  1234 E crosvm  : before\n1762000005.000  1234  1234 E crosvm  : after\n"))
	var out strings.Builder
	if err := logcatSince(adb.Device{}, debugPackage, "1762000000", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "before") || !strings.Contains(out.String(), "after") {
		t.Errorf("logcat since 1762000000 printed:\n%s", out.String())
	}
}

func TestControlOffers(t *testing.T) {
	c, d := testControl(t)
	want := "port=3000 state=offered phone=-\nport=8765 state=allowed phone=8766"
	os.WriteFile(filepath.Join(d.state, "control-offers"), []byte(want), 0o644)
	if got, err := c.offers(); err != nil || got != want {
		t.Errorf("offers: got %q, %v; want %q", got, err, want)
	}
}

func TestControlOfferSendsPortAndAnswer(t *testing.T) {
	_, d := testControl(t)
	if err := runControl([]string{"offer", "8765", "allow"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d.state, "control-offer"))
	if got, want := string(b), "--arg 8765 --extra answer:s:allow"; got != want {
		t.Errorf("the app got %q, want %q", got, want)
	}
	for _, args := range [][]string{{"offer", "8765", "maybe"}, {"offer", "web", "allow"}, {"offer", "8765"}} {
		if err := runControl(args); err != errUsage {
			t.Errorf("%q: got %v, want a usage error", args, err)
		}
	}
}

func TestControlClose(t *testing.T) {
	c, d := testControl(t)
	d.newAppModel()
	id := fakeNewTab(d.state)
	if err := runControl([]string{"close", "--tab", strconv.Itoa(id)}); err != nil {
		t.Fatal(err)
	}
	if dump, _ := c.dump(-1, 0); strings.Contains(dump, fmt.Sprintf("tab %d ", id)) {
		t.Errorf("tab %d is still there:\n%s", id, dump)
	}
	if err := runControl([]string{"close", "--tab", "0"}); err == nil || !strings.Contains(err.Error(), "console") {
		t.Errorf("closing the console: got %v", err)
	}
	if err := runControl([]string{"close"}); err != errUsage {
		t.Errorf("no tab: got %v, want a usage error", err)
	}
}

func TestControlLaunch(t *testing.T) {
	_, d := testControl(t)
	d.newAppModel()
	d.setGUI("org.example.Cube")
	out, err := stdoutOf(t, func() error {
		return runControl([]string{"launch", "--timeout", "2s", "org.example.Cube"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "gui window ") || !strings.Contains(out, " app=org.example.Cube ") {
		t.Errorf("launch printed %q, want the new window's line", out)
	}
	if want := "-n dev.nickolaj.pocketnix.debug/dev.nickolaj.pocketnix.ControlActivity"; !strings.Contains(d.events(), want) {
		t.Errorf("the launch didn't go to the debug app's ControlActivity; events:\n%s", d.events())
	}

	// A second launch waits for a window of its own, not the first one's.
	poll := controlPoll
	t.Cleanup(func() { controlPoll = poll })
	controlPoll = 10 * time.Millisecond
	d.setGUI()
	if err := runControl([]string{"launch", "--timeout", "100ms", "org.example.Cube"}); err == nil || !strings.Contains(err.Error(), "no new window") {
		t.Errorf("a launch that opened nothing: got %v", err)
	}
	if out, err := stdoutOf(t, func() error { return runControl([]string{"launch", "--timeout", "0", "org.example.Gone"}) }); err != nil || out != "" {
		t.Errorf("launch --timeout 0: %q, %v; want no wait", out, err)
	}
	if err := runControl([]string{"launch"}); err != errUsage {
		t.Errorf("no app: got %v, want a usage error", err)
	}
}
