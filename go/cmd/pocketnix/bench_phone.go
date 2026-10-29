package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// androidHz is Android's USER_HZ, the unit of /proc's CPU times.
const androidHz = 100

// androidSample is the phone's CPU counters at one moment.
type androidSample struct {
	uptime float64
	// Ticks per group: the app's threads by name, the VM's processes
	// (everything else under the app's uid), surfaceflinger.
	groups map[string]float64
	vm     map[string]float64
	busy   float64
	idle   float64
	freqs  map[string]int
}

// sampleScript prints one tagged line per thing measured: U uptime, A an
// app thread's stat, V a VM process's stat, S surfaceflinger's, P the
// phone's CPU line, F each CPU's clock.
func sampleScript(pkg string) string {
	return `pid=$(pidof ` + pkg + `); uid=$(stat -c %u /proc/$pid); echo "U $(cat /proc/uptime)";
for t in /proc/$pid/task/*; do echo "A $(cat $t/stat)"; done;
ps -A -o PID=,UID= | while read p u; do [ "$u" = "$uid" ] && [ "$p" != "$pid" ] && echo "V $(cat /proc/$p/stat)"; done;
for p in $(pidof surfaceflinger); do echo "S $(cat /proc/$p/stat)"; done;
echo "P $(head -1 /proc/stat)";
for c in /sys/devices/system/cpu/cpu[0-9]*; do echo "F ${c##*/} $(cat $c/cpufreq/scaling_cur_freq 2>/dev/null)"; done`
}

func (p *phone) sample() (androidSample, error) {
	out, err := p.dev.Shell(sampleScript(p.o.pkg))
	if err != nil {
		return androidSample{}, err
	}
	return parseAndroidSample(out)
}

// appThreads are the app's threads reported on their own; the rest are app-other.
var appThreads = map[string]bool{"gui-link": true, "gui-render": true, "gui-writer": true, "RenderThread": true}

func parseAndroidSample(out string) (androidSample, error) {
	s := androidSample{groups: map[string]float64{}, vm: map[string]float64{}, freqs: map[string]int{}}
	for _, line := range strings.Split(out, "\n") {
		tag, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch tag {
		case "U":
			f := strings.Fields(rest)
			if len(f) > 0 {
				s.uptime, _ = strconv.ParseFloat(f[0], 64)
			}
		case "A", "V", "S":
			name, ticks, ok := statTicks(rest)
			if !ok {
				continue
			}
			switch tag {
			case "A":
				s.groups["app"] += ticks
				if appThreads[name] {
					s.groups[name] += ticks
				} else {
					s.groups["app-other"] += ticks
				}
			case "V":
				s.groups["vm"] += ticks
				s.vm[name] += ticks
			case "S":
				s.groups["surfaceflinger"] += ticks
			}
		case "P":
			f := strings.Fields(rest)
			v := func(i int) float64 {
				if i >= len(f) {
					return 0
				}
				x, _ := strconv.ParseFloat(f[i], 64)
				return x
			}
			// cpu user nice system idle iowait irq softirq
			s.busy = v(1) + v(2) + v(3) + v(6) + v(7)
			s.idle = v(4) + v(5)
		case "F":
			f := strings.Fields(rest)
			if len(f) == 2 {
				s.freqs[f[0]], _ = strconv.Atoi(f[1])
			}
		}
	}
	if s.uptime == 0 {
		return s, fmt.Errorf("no uptime in the phone's sample:\n%s", out)
	}
	return s, nil
}

// statTicks is a /proc/<pid>/stat line's comm and utime + stime.
func statTicks(stat string) (string, float64, bool) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return "", 0, false
	}
	f := strings.Fields(stat[end+1:])
	if len(f) < 13 {
		return "", 0, false
	}
	u, err1 := strconv.ParseFloat(f[11], 64)
	k, err2 := strconv.ParseFloat(f[12], 64)
	return stat[open+1 : end], u + k, err1 == nil && err2 == nil
}

// androidCPU is each group's CPU between two samples as % of one core, and
// the whole phone's as % of all cores.
func androidCPU(a, b androidSample) map[string]any {
	dt := b.uptime - a.uptime
	if dt <= 0 {
		return nil
	}
	pct := func(x, y float64) float64 { return round1((y - x) / androidHz / dt * 100) }
	out := map[string]any{"seconds": round1(dt)}
	for g, v := range b.groups {
		out[g] = pct(a.groups[g], v)
	}
	vm := map[string]any{}
	for n, v := range b.vm {
		if p := pct(a.vm[n], v); p > 0 {
			vm[n] = p
		}
	}
	out["vmProcesses"] = vm
	if busy, idle := b.busy-a.busy, b.idle-a.idle; busy+idle > 0 {
		out["phoneBusyPctOfAll"] = round1(busy / (busy + idle) * 100)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

var (
	batteryTemp   = regexp.MustCompile(`(?m)^\s*temperature: (\d+)`)
	batteryLevel  = regexp.MustCompile(`(?m)^\s*level: (\d+)`)
	powered       = regexp.MustCompile(`(?m)^\s*(AC|USB|Wireless|Dock) powered: true`)
	thermalStatus = regexp.MustCompile(`Thermal Status: (\d+)`)
)

// conditions are what skews a run: heat, throttling and a charger.
func (p *phone) conditions() map[string]any {
	c := map[string]any{}
	if out, err := p.dev.Shell("dumpsys battery"); err == nil {
		if m := batteryTemp.FindStringSubmatch(out); m != nil {
			t, _ := strconv.Atoi(m[1])
			c["batteryTempC"] = float64(t) / 10
		}
		if m := batteryLevel.FindStringSubmatch(out); m != nil {
			c["batteryLevel"], _ = strconv.Atoi(m[1])
		}
		c["charging"] = powered.MatchString(out)
	}
	if out, err := p.dev.Shell("dumpsys thermalservice | grep -m1 'Thermal Status'"); err == nil {
		if m := thermalStatus.FindStringSubmatch(out); m != nil {
			c["thermalStatus"], _ = strconv.Atoi(m[1])
		}
	}
	return c
}

// settlePoll is how often the thermal status is read while waiting.
const settlePoll = 5 * time.Second

// settle waits while Android reports throttling. The battery's temperature
// says nothing here: charging over USB warms it whatever the load.
func (p *phone) settle(max time.Duration) {
	deadline := time.Now().Add(max)
	for {
		status := num(p.conditions()["thermalStatus"])
		if !(status > 0) || time.Now().After(deadline) {
			return
		}
		fmt.Fprintf(os.Stderr, "bench: cooling down: thermal status %g\n", status)
		time.Sleep(settlePoll)
	}
}

// tapper taps the middle of the screen, where the bench's window is, one
// tap after another until stopped: each `input tap` takes a few hundred ms.
type tapper struct {
	done chan struct{}
	wg   sync.WaitGroup
}

var screenSize = regexp.MustCompile(`(\d+)x(\d+)\s*$`)

func startTaps(dev adb.Device) *tapper {
	t := &tapper{done: make(chan struct{})}
	out, err := dev.Shell("wm size")
	m := screenSize.FindStringSubmatch(strings.TrimSpace(out))
	if err != nil || m == nil {
		return t
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			select {
			case <-t.done:
				return
			default:
				dev.Shell(fmt.Sprintf("input tap %d %d", w/2, h/2))
			}
		}
	}()
	return t
}

func (t *tapper) stop() {
	if t == nil {
		return
	}
	close(t.done)
	t.wg.Wait()
}

// powerSampler reads the battery's current and voltage every
// powerInterval until stopped.
type powerSampler struct {
	done    chan struct{}
	wg      sync.WaitGroup
	samples [][2]float64
}

const powerInterval = 500 * time.Millisecond

func startPower(dev adb.Device) *powerSampler {
	s := &powerSampler{done: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(powerInterval)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				out, err := dev.Shell("cat /sys/class/power_supply/battery/current_now /sys/class/power_supply/battery/voltage_now")
				f := strings.Fields(out)
				if err != nil || len(f) != 2 {
					continue
				}
				ua, _ := strconv.ParseFloat(f[0], 64)
				uv, _ := strconv.ParseFloat(f[1], 64)
				s.samples = append(s.samples, [2]float64{ua, uv})
			}
		}
	}()
	return s
}

func (s *powerSampler) stop() {
	if s == nil {
		return
	}
	close(s.done)
	s.wg.Wait()
}

// result is the mean current (mA, the kernel's sign), voltage and their
// product's magnitude (mW). A charger makes them the charge's, not the
// load's: see the conditions' "charging".
func (s *powerSampler) result() map[string]any {
	if s == nil || len(s.samples) == 0 {
		return nil
	}
	var ma, v, mw float64
	for _, x := range s.samples {
		ma += x[0] / 1000
		v += x[1] / 1e6
		mw += x[0] * x[1] / 1e9
	}
	n := float64(len(s.samples))
	if mw < 0 {
		mw = -mw
	}
	return map[string]any{"samples": len(s.samples), "currentMa": round1(ma / n), "voltageV": math.Round(v/n*100) / 100, "powerMw": round1(mw / n)}
}
