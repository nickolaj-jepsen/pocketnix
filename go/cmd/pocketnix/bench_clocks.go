package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// clockInterval is the clock sampler's period, in the shell's sleep syntax.
const clockInterval = "0.2"

// clockMaxSeconds ends the phone's sampling loop if adb never kills it.
const clockMaxSeconds = 900

// clockScript prints, once, each VM process's command line (C) and
// affinity (M), and each cpufreq policy's CPUs (R); then, every
// clockInterval until stopped, the uptime (T), each policy's clock (F) and
// the stat line of every thread of the app and the VM's processes (S). It
// reads with the shell's builtin read, so a sample forks nothing but sleep.
func clockScript(pkg string) string {
	return `pid=$(pidof ` + pkg + `); uid=$(stat -c %u /proc/$pid)
vm=$(ps -A -o PID=,UID= | while read p u; do [ "$u" = "$uid" ] && [ "$p" != "$pid" ] && echo $p; done)
for p in $vm; do echo "C $p $(tr '\0' ' ' < /proc/$p/cmdline)"; echo "M $p $(grep Cpus_allowed_list /proc/$p/status)"; done
for d in /sys/devices/system/cpu/cpufreq/policy*; do read r < $d/related_cpus; echo "R ${d##*/} $r"; done
while :; do
  read up rest < /proc/uptime; echo "T $up"
  [ ${up%.*} -gt $(( ${start:=${up%.*}} + ` + strconv.Itoa(clockMaxSeconds) + ` )) ] && break
  for d in /sys/devices/system/cpu/cpufreq/policy*; do read f < $d/scaling_cur_freq; echo "F ${d##*/} $f"; done
  for p in $pid $vm; do for t in /proc/$p/task/*; do read s < $t/stat && echo "S $p $s"; done; done
  sleep ` + clockInterval + `
done`
}

// clockSampler runs clockScript on the phone until stopped.
type clockSampler struct {
	cmd *exec.Cmd
	out bytes.Buffer
	mu  sync.Mutex
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func startClocks(dev adb.Device, pkg string) *clockSampler {
	s := &clockSampler{}
	cmd, err := dev.Start(lockedWriter{&s.mu, &s.out}, "shell", clockScript(pkg))
	if err != nil {
		return nil
	}
	s.cmd = cmd
	return s
}

// stop ends the sampling and returns what it saw (see clockReport), nil
// when it saw nothing.
func (s *clockSampler) stop() map[string]any {
	if s == nil {
		return nil
	}
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return clockReport(s.out.String())
}

// threadStat is one thread's line from one sample.
type threadStat struct {
	pid, tid int
	name     string
	ticks    float64
	cpu      int
}

// parseStat reads a /proc/<pid>/task/<tid>/stat line: its tid, comm,
// utime + stime and the CPU it last ran on.
func parseStat(stat string) (threadStat, bool) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return threadStat{}, false
	}
	f := strings.Fields(stat[end+1:])
	// After the comm, field 3 (state) is f[0]: utime 14, stime 15, processor 39.
	if len(f) < 37 {
		return threadStat{}, false
	}
	tid, err1 := strconv.Atoi(strings.TrimSpace(stat[:open]))
	u, err2 := strconv.ParseFloat(f[11], 64)
	k, err3 := strconv.ParseFloat(f[12], 64)
	cpu, err4 := strconv.Atoi(f[36])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return threadStat{}, false
	}
	return threadStat{tid: tid, name: stat[open+1 : end], ticks: u + k, cpu: cpu}, true
}

// cpuList expands a sysfs CPU list ("0-3 8", "4-7").
func cpuList(s string) []int {
	var out []int
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		lo, hi, ranged := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if ranged {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out
}

// clockGroup is where a group of threads ran between samples.
type clockGroup struct {
	ticks    float64
	mhzTicks float64
	policy   map[string]float64
}

func (g *clockGroup) add(ticks float64, policy string, mhz float64) {
	if g.policy == nil {
		g.policy = map[string]float64{}
	}
	g.ticks += ticks
	g.mhzTicks += ticks * mhz
	g.policy[policy] += ticks
}

func (g *clockGroup) json() map[string]any {
	share := map[string]any{}
	for p, t := range g.policy {
		share[p] = round1(t / g.ticks * 100)
	}
	return map[string]any{"ticks": g.ticks, "mhz": round1(g.mhzTicks / g.ticks), "policyPct": share}
}

// clockGroupOf is the group a thread's time counts in: the VM's processes
// together, and the app's GUI threads by name; "" for the app's others.
func clockGroupOf(t threadStat, app int) string {
	switch {
	case t.pid != app:
		return "vm"
	case strings.HasPrefix(t.name, "gui-"):
		return t.name
	}
	return ""
}

// clockReport turns clockScript's output into each policy's clock over the
// samples, and, per group of threads, the CPU time it got, the clock it
// ran at (weighted by that time) and its share of the time per policy. A
// thread's time between two samples counts on the CPU it was on at the
// second, at that CPU's clock then.
func clockReport(out string) map[string]any {
	policyOf := map[int]string{}
	var procs []any
	affinity := map[string]string{}
	type sample struct {
		freqs   map[string]float64
		threads map[int]threadStat
	}
	var samples []sample
	app := 0
	for _, line := range strings.Split(out, "\n") {
		tag, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch tag {
		case "C":
			pid, cmdline, _ := strings.Cut(rest, " ")
			procs = append(procs, map[string]any{"pid": pid, "cmdline": strings.TrimSpace(cmdline)})
		case "M":
			pid, list, _ := strings.Cut(rest, " ")
			affinity[pid] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(list), "Cpus_allowed_list:"))
		case "R":
			policy, cpus, _ := strings.Cut(rest, " ")
			for _, c := range cpuList(cpus) {
				policyOf[c] = policy
			}
		case "T":
			samples = append(samples, sample{freqs: map[string]float64{}, threads: map[int]threadStat{}})
		case "F":
			f := strings.Fields(rest)
			if len(samples) > 0 && len(f) == 2 {
				khz, _ := strconv.ParseFloat(f[1], 64)
				samples[len(samples)-1].freqs[f[0]] = khz / 1000
			}
		case "S":
			pid, stat, _ := strings.Cut(rest, " ")
			t, ok := parseStat(stat)
			if len(samples) == 0 || !ok {
				continue
			}
			t.pid, _ = strconv.Atoi(pid)
			if app == 0 {
				app = t.pid
			}
			samples[len(samples)-1].threads[t.tid] = t
		}
	}
	// The last sample may be cut off by the kill.
	if len(samples) > 0 && len(samples[len(samples)-1].freqs) < len(distinct(policyOf)) {
		samples = samples[:len(samples)-1]
	}
	if len(samples) < 2 {
		return nil
	}
	groups := map[string]*clockGroup{}
	vcpus := map[string]*clockGroup{}
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1], samples[i]
		for tid, t := range cur.threads {
			was, ok := prev.threads[tid]
			if !ok || t.ticks <= was.ticks {
				continue
			}
			policy := policyOf[t.cpu]
			d, mhz := t.ticks-was.ticks, cur.freqs[policy]
			if g := clockGroupOf(t, app); g != "" {
				if groups[g] == nil {
					groups[g] = &clockGroup{}
				}
				groups[g].add(d, policy, mhz)
			}
			if strings.HasPrefix(t.name, "crosvm_vcpu") {
				if vcpus[t.name] == nil {
					vcpus[t.name] = &clockGroup{}
				}
				vcpus[t.name].add(d, policy, mhz)
			}
		}
	}
	policies := map[string]any{}
	for _, p := range distinct(policyOf) {
		var mhz []float64
		for _, s := range samples {
			if f, ok := s.freqs[p]; ok {
				mhz = append(mhz, f)
			}
		}
		sort.Float64s(mhz)
		if len(mhz) == 0 {
			continue
		}
		var cpus []int
		for c, q := range policyOf {
			if q == p {
				cpus = append(cpus, c)
			}
		}
		sort.Ints(cpus)
		policies[p] = map[string]any{"cpus": fmt.Sprint(cpus), "mhzMedian": mhz[len(mhz)/2], "mhzMax": mhz[len(mhz)-1]}
	}
	report := map[string]any{"samples": len(samples), "policies": policies, "vmProcesses": procs, "affinity": affinity}
	for name, g := range groups {
		report[name] = g.json()
	}
	if len(vcpus) > 0 {
		v := map[string]any{}
		for name, g := range vcpus {
			v[name] = g.json()
		}
		report["vcpus"] = v
	}
	return report
}

func distinct(m map[int]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range m {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
