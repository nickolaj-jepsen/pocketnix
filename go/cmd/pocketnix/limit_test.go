package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// limitedPhone has the debug app running under the out-of-front limit.
func limitedPhone(t *testing.T) *fakeDevice {
	t.Helper()
	d := newFakeDevice(t)
	d.write("limiter-high", []byte("3221225472"))
	return d
}

func (d *fakeDevice) limiterFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(d.state, name))
	return strings.TrimSpace(string(b))
}

func TestLiftLimit(t *testing.T) {
	lifted := strconv.FormatInt(liftedLimitMiB<<20, 10)
	for _, tc := range []struct{ name, ignored string }{
		{"a limited process", ""},
		// manual is dropped while the uid is ignored, so this needs the ignore undone first.
		{"lifted before", "10200"},
		{"every uid ignored", "all"},
		{"another uid ignored", "10999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := limitedPhone(t)
			d.write("limiter-ignored", []byte(tc.ignored))
			var out strings.Builder
			if err := liftLimit(context.Background(), adb.Device{}, debugPackage, &out); err != nil {
				t.Fatal(err)
			}
			if got := d.limiterFile("limiter-high"); got != lifted {
				t.Errorf("memory.high = %s, want %s", got, lifted)
			}
			if got := d.limiterFile("limiter-ignored"); got != "10200" {
				t.Errorf("ignored = %s, want the app's uid", got)
			}
			if !strings.Contains(out.String(), "10200") {
				t.Errorf("the output doesn't name the uid: %s", out.String())
			}
		})
	}
}

func TestLiftLimitSaysWhenAProcessDidntLift(t *testing.T) {
	d := limitedPhone(t)
	d.write("limiter-stuck", nil)
	defer func(old time.Duration) { limitWait = old }(limitWait)
	limitWait = time.Millisecond
	err := liftLimit(context.Background(), adb.Device{}, debugPackage, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "memory.high") {
		t.Errorf("got %v", err)
	}
}

func TestLiftLimitNeedsALimiter(t *testing.T) {
	newFakeDevice(t)
	if err := liftLimit(context.Background(), adb.Device{}, debugPackage, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "limiter") {
		t.Errorf("got %v", err)
	}
}

func TestLiftLimitIgnoresTheRunningProcesssUid(t *testing.T) {
	d := limitedPhone(t)
	d.can("pm list packages -U "+debugPackage, "package:"+debugPackage+" uid:1010200,10200\n", 0)
	if err := liftLimit(context.Background(), adb.Device{}, debugPackage, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := d.limiterFile("limiter-ignored"); got != "10200" {
		t.Errorf("ignored = %s, want the process's uid", got)
	}
}

func TestRestoreLimitIgnoresNothing(t *testing.T) {
	d := limitedPhone(t)
	d.write("limiter-ignored", []byte("10200"))
	if err := restoreLimit(adb.Device{}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := d.limiterFile("limiter-ignored"); got != "none" {
		t.Errorf("ignored = %s", got)
	}
}
