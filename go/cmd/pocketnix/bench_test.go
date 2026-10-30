package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAndroidSampleGroupsThreadsAndProcesses(t *testing.T) {
	out := `U 1000.50 2000.00
A 100 (gui-link) S 1 1 0 0 -1 0 0 0 0 0 30 20 0 0 20 0 1 0 1 1 1
A 101 (pocketnix.debug) S 1 1 0 0 -1 0 0 0 0 0 5 5 0 0 20 0 1 0 1 1 1
V 200 (crosvm) S 1 1 0 0 -1 0 0 0 0 0 400 100 0 0 20 0 1 0 1 1 1
S 300 (surfaceflinger) S 1 1 0 0 -1 0 0 0 0 0 7 3 0 0 20 0 1 0 1 1 1
P cpu  100 0 50 800 50 0 0 0 0 0
`
	s, err := parseAndroidSample(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"gui-link": 50, "app-other": 10, "app": 60, "vm": 500, "surfaceflinger": 10}
	for g, v := range want {
		if s.groups[g] != v {
			t.Errorf("%s = %v ticks, want %v", g, s.groups[g], v)
		}
	}
	if s.vm["crosvm"] != 500 || s.uptime != 1000.5 {
		t.Errorf("vm %v, uptime %v", s.vm, s.uptime)
	}
}

// stat is a thread's /proc stat line with the given utime and processor.
func stat(tid int, name string, utime, cpu int) string {
	f := make([]string, 37)
	for i := range f {
		f[i] = "0"
	}
	f[0], f[11], f[36] = "S", fmt.Sprint(utime), fmt.Sprint(cpu)
	return fmt.Sprintf("%d (%s) %s", tid, name, strings.Join(f, " "))
}

func TestClockReportWeighsClocksByThreadTime(t *testing.T) {
	out := strings.Join([]string{
		"C 200 /apex/com.android.virt/bin/crosvm run --host-cpu-topology",
		"M 200 Cpus_allowed_list:	0-8",
		"R policy0 0 1 2 3",
		"R policy4 4 5 6 7",
		"T 100.0", "F policy0 500000", "F policy4 400000",
		"S 100 " + stat(101, "gui-link", 0, 0),
		"S 200 " + stat(201, "crosvm_vcpu 4", 0, 4),
		"T 100.2", "F policy0 500000", "F policy4 2000000",
		"S 100 " + stat(101, "gui-link", 2, 1),
		"S 200 " + stat(201, "crosvm_vcpu 4", 10, 5),
		"T 100.4", "F policy0 1000000", "F policy4 1000000",
		"S 100 " + stat(101, "gui-link", 4, 2),
		"S 200 " + stat(201, "crosvm_vcpu 4", 15, 1),
		"T 100.6", "F policy0 1000000",
	}, "\n")
	r := clockReport(out)
	// 10 ticks at 2000 MHz on policy4, then 5 at 1000 on policy0.
	if mhz := num(dig(r, "vm", "mhz")); math.Abs(mhz-1666.7) > 0.1 {
		t.Errorf("vm at %v MHz, want 1666.7", mhz)
	}
	if p := num(dig(r, "vm", "policyPct", "policy4")); math.Abs(p-66.7) > 0.1 {
		t.Errorf("vm on policy4 %v%%, want 66.7", p)
	}
	if mhz := num(dig(r, "gui-link", "mhz")); mhz != 750 {
		t.Errorf("gui-link at %v MHz, want 750", mhz)
	}
	if dig(r, "vcpus", "crosvm_vcpu 4") == nil {
		t.Errorf("no vcpu thread in %v", r["vcpus"])
	}
	// The cut-off last sample is dropped.
	if n := r["samples"]; n != 3 {
		t.Errorf("%v samples, want 3", n)
	}
	if max := num(dig(r, "policies", "policy0", "mhzMax")); max != 1000 {
		t.Errorf("policy0 max %v MHz", max)
	}
}

func TestAndroidCPUIsPercentOfOneCore(t *testing.T) {
	a := androidSample{uptime: 10, groups: map[string]float64{"vm": 100}, busy: 100, idle: 900}
	b := androidSample{uptime: 12, groups: map[string]float64{"vm": 250}, busy: 400, idle: 1600}
	cpu := androidCPU(a, b)
	// 150 ticks of 1/100 s in 2 s.
	if cpu["vm"] != 75.0 {
		t.Errorf("vm = %v, want 75", cpu["vm"])
	}
	if cpu["phoneBusyPctOfAll"] != 30.0 {
		t.Errorf("phone busy = %v, want 30", cpu["phoneBusyPctOfAll"])
	}
}

func TestStatTicksTakesCommWithSpaces(t *testing.T) {
	name, ticks, ok := statTicks("42 (Binder:1 2) S 1 1 0 0 -1 0 0 0 0 0 3 4 0 0")
	if !ok || name != "Binder:1 2" || ticks != 7 {
		t.Errorf("got %q %v %v", name, ticks, ok)
	}
}

func TestDiffSnapshotSubtractsCountersAndBuckets(t *testing.T) {
	var before, after map[string]any
	json.Unmarshal([]byte(`{"counters":{"a":3},"histograms":{"h":{"count":2,"sumUs":10,"buckets":[[4,2]]}}}`), &before)
	json.Unmarshal([]byte(`{"counters":{"a":5,"b":1},"histograms":{"h":{"count":5,"sumUs":40,"buckets":[[4,3],[8,2]]}},"frameNs":8333333}`), &after)
	d := diffSnapshot(before, after)
	if a, b := num(dig(d, "counters", "a")), num(dig(d, "counters", "b")); a != 2 || b != 1 {
		t.Errorf("counters a=%v b=%v", a, b)
	}
	h := dig(d, "histograms", "h")
	if c := num(dig(h, "count")); c != 3 {
		t.Errorf("count %v", c)
	}
	if q := quantileMs(h, 1); q != 0.008 {
		t.Errorf("p100 %v ms, want 0.008", q)
	}
	if num(d["frameNs"]) != 8333333 {
		t.Errorf("frameNs %v", d["frameNs"])
	}
}

func TestQuantileOfAnEmptyHistogramIsNaN(t *testing.T) {
	if q := quantileMs(map[string]any{"count": 0.0}, 0.5); !math.IsNaN(q) {
		t.Errorf("got %v", q)
	}
}

func TestReportTakesTheMedianRunPerWorkload(t *testing.T) {
	dir := t.TempDir()
	for i, fps := range []float64{20, 40, 30} {
		r := map[string]any{"workload": "noise", "client": map[string]any{"fps": fps, "seconds": 10.0}}
		b, _ := json.Marshal(r)
		os.WriteFile(filepath.Join(dir, "01-noise-"+string(rune('1'+i))+".json"), b, 0o644)
	}
	runs, err := loadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].name != "noise" {
		t.Fatalf("workloads %v", runs)
	}
	if m := runs[0].median("fps"); m != 30 {
		t.Errorf("median fps %v, want 30", m)
	}
	if _, err := benchReport(dir, dir); err != nil {
		t.Errorf("report against itself: %v", err)
	}
}
