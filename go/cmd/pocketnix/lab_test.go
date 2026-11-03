package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const screenOff, screenAwake = "  mWakefulness=Dozing\n", "  mWakefulness=Awake\n"

// labPhone is a limited phone whose guest stalls while a fill runs at limitMiB,
// as a real one does, until the limit rises.
type labPhone struct {
	d        *fakeDevice
	limitMiB int
	vm       labVM
	filling  atomic.Bool
	restarts atomic.Int32
	cleans   atomic.Int32
	// onFill runs as a fill starts.
	onFill func(limited bool)
	// reaches says a fill at the limit stalls the guest.
	reaches bool
}

func newLabPhone(t *testing.T, vm labVM) *labPhone {
	t.Helper()
	d := limitedPhone(t)
	d.can(wakefulnessCommand, screenOff, 0)
	old := labTimes
	t.Cleanup(func() { labTimes = old })
	labTimes.echoEvery, labTimes.echoTimeout, labTimes.sampleEvery = time.Millisecond, time.Second, time.Millisecond
	labTimes.deadline, labTimes.recovery, labTimes.memory = 10*time.Second, 5*time.Second, 200*time.Millisecond
	return &labPhone{d: d, limitMiB: 3072, vm: vm, reaches: true}
}

func (p *labPhone) atLimit() bool {
	high, _ := strconv.ParseInt(p.d.limiterFile("limiter-high"), 10, 64)
	return high == int64(p.limitMiB)<<20
}

func (p *labPhone) rig(t *testing.T, o labOpts) *labRig {
	t.Helper()
	if o.out == "" {
		o.out = t.TempDir()
	}
	return &labRig{
		dev: adb.Device{}, pkg: debugPackage, o: o, w: &strings.Builder{},
		vm: func() (labVM, error) { return p.vm, nil },
		restartVM: func(context.Context) error {
			p.restarts.Add(1)
			return nil
		},
		guest: func(ctx context.Context, cmdline string) error {
			switch {
			case strings.Contains(cmdline, "of="+labFillRoot):
				if p.onFill != nil {
					p.onFill(p.atLimit())
				}
				if !p.atLimit() || !p.reaches {
					if p.atLimit() {
						p.d.write("limiter-events", []byte("50"))
					}
					return nil
				}
				p.filling.Store(true)
				p.d.write("limiter-events", []byte("50"))
				<-ctx.Done()
				p.filling.Store(false)
				return ctx.Err()
			case strings.Contains(cmdline, "umount"):
				p.cleans.Add(1)
			case cmdline == "true" && p.filling.Load() && p.atLimit():
				return errors.New("no answer")
			}
			return nil
		},
	}
}

func readReport(t *testing.T, dir string) labReport {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*", "report.json"))
	if len(files) != 1 {
		t.Fatalf("reports: %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var r labReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLabFindsTheLimitCausesAStall(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, memoryMiB: 4096})
	out := t.TempDir()
	r := p.rig(t, labOpts{out: out})
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, out)
	if !strings.Contains(report.Verdict, "causes") {
		t.Errorf("verdict: %s", report.Verdict)
	}
	if len(report.Arms) != 2 || report.Arms[0].Ended != labFinished || report.Arms[1].Ended != labStalled {
		t.Fatalf("arms: %+v", report.Arms)
	}
	if report.Arms[1].RecoverySecond <= 0 {
		t.Errorf("no recovery time: %+v", report.Arms[1])
	}
	if p.atLimit() {
		t.Error("the breaker left the limit down")
	}
	if p.cleans.Load() != 2 {
		t.Errorf("cleaned %d fills", p.cleans.Load())
	}
	if got := p.d.limiterFile("limiter-ignored"); got != "none" {
		t.Errorf("left ignored = %s", got)
	}
	if p.restarts.Load() != 0 {
		t.Errorf("restarted a VM whose memory goes back %d times", p.restarts.Load())
	}
}

func TestLabRestartsAVMOnGuestRAM(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, guestRAM: true, memoryMiB: 4096})
	if err := p.rig(t, labOpts{}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.restarts.Load() != 2 {
		t.Errorf("restarted %d times, want after each arm", p.restarts.Load())
	}
}

func TestLabRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		o           labOpts
		vm          labVM
		screen      string
		noLimiter   bool
	}{
		{name: "screen on", token: "screen", vm: labVM{running: true, memoryMiB: 4096}, screen: screenAwake},
		{name: "VM stopped", token: "running", vm: labVM{memoryMiB: 4096}},
		{name: "a VM that fits", token: "fits", vm: labVM{running: true, memoryMiB: 2800}},
		{name: "near the kill line", token: "kill line", vm: labVM{running: true, memoryMiB: 4096}, o: labOpts{limitMiB: 2000}},
		{name: "a fill bigger than the VM", token: "fit", vm: labVM{running: true, memoryMiB: 4096}, o: labOpts{fillMiB: 5000}},
		{name: "no limiter", token: "limiter", vm: labVM{running: true, memoryMiB: 4096}, noLimiter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLabPhone(t, tc.vm)
			if tc.screen != "" {
				p.d.can(wakefulnessCommand, tc.screen, 0)
			}
			if tc.noLimiter {
				p.d.can(limiterStatus, "Error: unknown sub-command\n", 255)
			}
			err := p.rig(t, tc.o).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.token) {
				t.Errorf("got %v", err)
			}
			if p.d.limiterFile("limiter-ignored") != "" {
				t.Error("touched the limiter before refusing")
			}
		})
	}
}

func TestLabForceRunsWithTheScreenOn(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, memoryMiB: 4096})
	p.d.can(wakefulnessCommand, screenAwake, 0)
	out := t.TempDir()
	if err := p.rig(t, labOpts{out: out, force: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(readReport(t, out).Arms) != 2 {
		t.Error("an arm didn't run")
	}
}

func TestLabStopsWhenTheScreenComesOn(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, memoryMiB: 4096})
	p.onFill = func(limited bool) {
		if limited {
			p.d.can(wakefulnessCommand, screenAwake, 0)
		}
	}
	p.reaches = true
	out := t.TempDir()
	r := p.rig(t, labOpts{out: out})
	// A stall would end the arm first: this guest keeps answering while the screen comes on.
	guest := r.guest
	r.guest = func(ctx context.Context, cmdline string) error {
		if cmdline == "true" {
			return nil
		}
		return guest(ctx, cmdline)
	}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, out)
	if !strings.Contains(report.Verdict, "screen") {
		t.Errorf("verdict: %s", report.Verdict)
	}
	if p.atLimit() {
		t.Error("left the limit down with the screen on")
	}
}

func TestLabStopsOnLowMemory(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, memoryMiB: 4096})
	p.d.write("limiter-avail", []byte("50000"))
	r := p.rig(t, labOpts{})
	guest := r.guest
	release := make(chan struct{})
	r.guest = func(ctx context.Context, cmdline string) error {
		if strings.Contains(cmdline, "of="+labFillRoot) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
			return nil
		}
		return guest(ctx, cmdline)
	}
	defer close(release)
	a := r.arm(context.Background(), "lifted", 0, fakePID, 100)
	if a.Ended != labLowMemory {
		t.Errorf("ended %s", a.Ended)
	}
}

func TestLabNeverLowersALimitOverMemory(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, memoryMiB: 4096})
	p.onFill = func(limited bool) {
		if !limited {
			p.d.write("limiter-current", []byte(strconv.FormatInt(int64(4000)<<20, 10)))
		}
	}
	err := p.rig(t, labOpts{}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Errorf("got %v", err)
	}
	if p.atLimit() {
		t.Error("lowered the limit under memory")
	}
}

func TestLabRestoresAnAppItFoundLifted(t *testing.T) {
	p := newLabPhone(t, labVM{running: true, guestRAM: true, memoryMiB: 4096})
	p.d.write("limiter-ignored", []byte("10200"))
	r := p.rig(t, labOpts{})
	// As on a phone: a proc state change reapplies the class's limit unless ignored.
	r.restartVM = func(context.Context) error {
		if p.d.limiterFile("limiter-ignored") != "10200" {
			p.d.write("limiter-high", []byte("3221225472"))
		}
		return nil
	}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.d.limiterFile("limiter-ignored"); got != "10200" {
		t.Errorf("ignored = %s", got)
	}
	if got, want := p.d.limiterFile("limiter-high"), strconv.FormatInt(liftedLimitMiB<<20, 10); got != want {
		t.Errorf("memory.high = %s, want it lifted again", got)
	}
}

func TestLabVerdict(t *testing.T) {
	events := []labSample{{HighEvents: 0}, {HighEvents: 9}}
	ok := labArm{Ended: labFinished}
	for _, tc := range []struct {
		name, token     string
		lifted, limited labArm
	}{
		{"the limit stalls", "causes", ok, labArm{Ended: labStalled}},
		{"both stall", "isn't the only cause", labArm{Ended: labStalled}, labArm{Ended: labStalled}},
		{"only lifted stalls", "only with the limit lifted", labArm{Ended: labStalled}, labArm{Ended: labFinished, Samples: events}},
		{"no stall at the limit", "kept answering", ok, labArm{Ended: labFinished, Samples: events}},
		{"never reached it", "never reached", ok, labArm{Ended: labFinished}},
		{"in front", "in front", ok, labArm{Ended: labStalled, InFront: true}},
		{"screen on", "screen", ok, labArm{Ended: labScreenOn}},
		{"low memory", "low on memory", labArm{Ended: labLowMemory}, labArm{Ended: labFinished, Samples: events}},
		{"deadline", "didn't finish", ok, labArm{Ended: labDeadline, Samples: events}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := labVerdict(tc.lifted, tc.limited); !strings.Contains(got, tc.token) {
				t.Errorf("got %q, want %q", got, tc.token)
			}
		})
	}
}
