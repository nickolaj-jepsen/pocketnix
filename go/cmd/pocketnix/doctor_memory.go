package main

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// limiterOverheadMiB is roughly what the app and crosvm hold beside guest RAM,
// in the limit's cgroup: a 2867 MiB VM rode a Pixel 8 Pro's 3072 MiB limit
// without freezing the phone.
const limiterOverheadMiB = 200

const limiterStatus = "cmd activity memory-limiter status"

var (
	limiterLimits  = regexp.MustCompile(`\blimits=(true|false)\b`)
	limiterKilling = regexp.MustCompile(`\bkilling=(true|false)\b`)
	limiterIgnored = regexp.MustCompile(`\bignored=(\S+)`)
	limiterMiB     = func(name string) *regexp.Regexp { return regexp.MustCompile(`\b` + name + `=(\d+)MB\b`) }
	limiterSizes   = map[string]*regexp.Regexp{
		"visibleMem":     limiterMiB("visibleMem"),
		"notVisibleMem":  limiterMiB("notVisibleMem"),
		"visibleSwap":    limiterMiB("visibleSwap"),
		"notVisibleSwap": limiterMiB("notVisibleSwap"),
	}
	exitTimestamp   = regexp.MustCompile(`^\s*timestamp=(\S+ \S+)`)
	exitDescription = regexp.MustCompile(`^\s*description=(.*)$`)
)

// appMemoryCommand prints pkg's running process's proc state and cgroup files,
// one key=value per line, and exits non-zero when it isn't running.
func appMemoryCommand(pkg string) string {
	return "p=$(pidof " + adb.ShellQuote(pkg) + ") && d=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/$p/cgroup) && " +
		`echo "state=$(cmd activity get-uid-state $(stat -c %u /proc/$p))" && ` +
		`echo "high=$(cat $d/memory.high)" && echo "swapMax=$(cat $d/memory.swap.max)" && ` +
		`echo "current=$(cat $d/memory.current)" && echo "peak=$(cat $d/memory.peak)" && ` +
		`echo "highEvents=$(sed -n 's/^high //p' $d/memory.events)"`
}

// probeMemory reads Android's memory limits and pkg's standing against them.
// Only the app's own cgroup and exit records are read, never another app's.
func probeMemory(dev adb.Device, pkg string) doctorMemory {
	m := doctorMemory{Limiter: probeLimiter(dev, pkg), LimiterKills: []string{}}
	if out := shellOK(dev, appMemoryCommand(pkg)); out != nil {
		m.Process = parseAppMemory(*out)
	}
	if out := shellOK(dev, "dumpsys activity exit-info "+adb.ShellQuote(pkg)); out != nil {
		m.LimiterKills = parseLimiterKills(*out)
	}
	return m
}

func probeLimiter(dev adb.Device, pkg string) *doctorLimiter {
	out := shellOK(dev, limiterStatus)
	if out == nil {
		return nil
	}
	l := parseLimiter(*out)
	if l != nil && l.Ignored != "" {
		uids, err := appUID(dev, pkg)
		l.AppIgnored = l.Ignored == "all" || (err == nil && slices.Contains(strings.Split(uids, ","), l.Ignored))
	}
	return l
}

// parseLimiter reads MemoryLimiter's dump; nil when it isn't one.
func parseLimiter(status string) *doctorLimiter {
	if strings.Contains(status, "Error:") || !strings.Contains(status, "Memory limiter") {
		return nil
	}
	l := &doctorLimiter{}
	m := limiterLimits.FindStringSubmatch(status)
	if m == nil { // The disabled controller prints only that word.
		return l
	}
	l.Enabled = true
	l.Limits = m[1] == "true"
	if k := limiterKilling.FindStringSubmatch(status); k != nil {
		l.Killing = k[1] == "true"
	}
	if i := limiterIgnored.FindStringSubmatch(status); i != nil && i[1] != "none" {
		l.Ignored = i[1]
	}
	read := func(name string) *int {
		s := limiterSizes[name].FindStringSubmatch(status)
		if s == nil {
			return nil
		}
		n, err := strconv.Atoi(s[1])
		if err != nil {
			return nil
		}
		return &n
	}
	l.VisibleMiB, l.NotVisibleMiB = read("visibleMem"), read("notVisibleMem")
	l.VisibleSwapMiB, l.NotVisibleSwapMiB = read("visibleSwap"), read("notVisibleSwap")
	return l
}

func parseAppMemory(out string) *doctorAppMemory {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	return &doctorAppMemory{
		State:      values["state"],
		High:       values["high"],
		SwapMax:    values["swapMax"],
		Current:    values["current"],
		Peak:       values["peak"],
		HighEvents: values["highEvents"],
	}
}

// parseLimiterKills lists the exit records MemoryLimiter caused, as
// "<timestamp> <description>". They carry REASON_OTHER, so only the description
// tells them apart.
func parseLimiterKills(exitInfo string) []string {
	kills := []string{}
	var timestamp string
	for _, line := range strings.Split(exitInfo, "\n") {
		if m := exitTimestamp.FindStringSubmatch(line); m != nil {
			timestamp = m[1]
		} else if m := exitDescription.FindStringSubmatch(line); m != nil && strings.Contains(m[1], "MemoryLimiter") {
			kills = append(kills, timestamp+" "+strings.TrimSpace(m[1]))
		}
	}
	return kills
}

// memoryVerdict adds the memory facts to v. They can only warn: a VM over the
// limit is fine until the guest touches that much with the app out of front.
func memoryVerdict(v *doctorVerdict, m doctorMemory, a doctorApp) {
	for _, kill := range m.LimiterKills {
		v.warn("Android's memory limiter killed the app, and its VM with it: %s", kill)
	}
	l := m.Limiter
	if l == nil || !l.Enabled || !l.Limits || l.AppIgnored || l.NotVisibleMiB == nil {
		return
	}
	vm, err := strconv.Atoi(a.Caps["memoryMb"])
	if err != nil {
		return
	}
	if vm+limiterOverheadMiB > *l.NotVisibleMiB {
		v.warn("the VM has %d MiB, and Android limits the app to %d MiB out of front: a guest that uses that much can freeze the phone until the app comes back to the front", vm, *l.NotVisibleMiB)
	}
}

// printMemory writes m for a person, through w's plainWriter.
func printMemory(w io.Writer, m doctorMemory) {
	mib := func(n *int) string {
		if n == nil {
			return "?"
		}
		return strconv.Itoa(*n)
	}
	switch l := m.Limiter; {
	case l == nil:
		fmt.Fprintf(w, "limiter      none answered\n")
	case !l.Enabled:
		fmt.Fprintf(w, "limiter      disabled\n")
	default:
		ignored := ""
		if l.Ignored != "" {
			ignored = ", ignoring " + l.Ignored
		}
		fmt.Fprintf(w, "limiter      limits %t, killing %t, %s MiB in front and %s MiB out of front, swap %s and %s MiB%s\n",
			l.Limits, l.Killing, mib(l.VisibleMiB), mib(l.NotVisibleMiB), mib(l.VisibleSwapMiB), mib(l.NotVisibleSwapMiB), ignored)
	}
	if p := m.Process; p != nil {
		fmt.Fprintf(w, "app memory   state %s, memory.high %s, memory.swap.max %s, current %s, peak %s, high events %s\n",
			p.State, p.High, p.SwapMax, p.Current, p.Peak, p.HighEvents)
	}
}
