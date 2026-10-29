package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// fakeControl is the app's ControlProvider: `content read` of the dump
// answers the state's control-dump file, and `type` appends its input, and
// "<enter>" for Enter, to control-typed. Values are decoded as the phone's
// `content` does: a backslash escapes the next character.
func fakeControl(state, line string) (string, int) {
	w := shellWords(line)
	switch {
	case len(w) == 4 && w[0] == "content" && w[1] == "read":
		b, err := os.ReadFile(filepath.Join(state, "control-dump"))
		if err != nil {
			return "Error while accessing provider:dev.nickolaj.pocketnix.debug.control\n", 0
		}
		return string(b), 0
	case len(w) >= 6 && w[0] == "content" && w[1] == "call" && w[5] == "type":
		typed := ""
		for i, a := range w {
			if i == 0 || w[i-1] != "--extra" {
				continue
			}
			if v, ok := strings.CutPrefix(a, "input:s:"); ok {
				typed += contentEscape.ReplaceAllString(v, "$1")
			}
			if a == "enter:b:true" {
				typed += "<enter>"
			}
		}
		f, _ := os.OpenFile(filepath.Join(state, "control-typed"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(typed + "\n")
		f.Close()
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

func testControl(t *testing.T) (control, *fakeDevice) {
	d := newFakeDevice(t)
	return control{provider{dev: adb.Device{}, authority: debugPackage + ".control"}}, d
}

func (d *fakeDevice) setDump(s string) {
	os.WriteFile(filepath.Join(d.state, "control-dump"), []byte(s), 0o644)
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

func TestControlWait(t *testing.T) {
	c, d := testControl(t)
	d.setDump("state=Running\ntab 2 Pty(profile=shell) 'shell' running=true\n|2| $ echo done-$((1+1))\n")
	if _, err := c.wait(2, 0, regexp.MustCompile(`^done-2$`), 0); err == nil {
		t.Error("matched the typed command line, not its output")
	}
	d.setDump("state=Running\ntab 2 Pty(profile=shell) 'shell' running=true\n|2| $ echo done-$((1+1))\n|2| done-2\n")
	if _, err := c.wait(2, 0, regexp.MustCompile(`^done-2$`), time.Second); err != nil {
		t.Error(err)
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
