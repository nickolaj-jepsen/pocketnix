package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

// diffSnapshot is after less before for a stats snapshot (the compositor's,
// the sink's or the app's, which share a shape): counters and histogram
// buckets subtract, everything else is after's.
func diffSnapshot(before, after map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range after {
		out[k] = v
	}
	if c, ok := after["counters"].(map[string]any); ok {
		was, _ := dig(before, "counters").(map[string]any)
		d := map[string]any{}
		for k, v := range c {
			d[k] = math.Max(0, num(v)-zero(num(was[k])))
		}
		out["counters"] = d
	}
	if h, ok := after["histograms"].(map[string]any); ok {
		was, _ := dig(before, "histograms").(map[string]any)
		d := map[string]any{}
		for k, v := range h {
			old, _ := was[k].(map[string]any)
			d[k] = diffHistogram(old, v.(map[string]any))
		}
		out["histograms"] = d
	}
	return out
}

func diffHistogram(before, after map[string]any) map[string]any {
	old := map[float64]float64{}
	for _, b := range buckets(before) {
		old[b[0]] = b[1]
	}
	var bs []any
	for _, b := range buckets(after) {
		if n := b[1] - old[b[0]]; n > 0 {
			bs = append(bs, []any{b[0], n})
		}
	}
	return map[string]any{
		"count":   num(after["count"]) - zero(num(before["count"])),
		"sumUs":   num(after["sumUs"]) - zero(num(before["sumUs"])),
		"buckets": bs,
	}
}

func buckets(h map[string]any) [][2]float64 {
	var out [][2]float64
	list, _ := h["buckets"].([]any)
	for _, e := range list {
		if p, ok := e.([]any); ok && len(p) == 2 {
			out = append(out, [2]float64{num(p[0]), num(p[1])})
		}
	}
	return out
}

// quantileMs is the bound, in ms, of the bucket holding quantile q; NaN
// for an empty or missing histogram.
func quantileMs(h any, q float64) float64 {
	m, _ := h.(map[string]any)
	count := num(m["count"])
	if !(count > 0) {
		return math.NaN()
	}
	target := math.Max(1, math.Ceil(count*q))
	seen := 0.0
	for _, b := range buckets(m) {
		seen += b[1]
		if seen >= target {
			return b[0] / 1000
		}
	}
	return math.NaN()
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// num is a JSON number, or NaN.
func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	if i, ok := v.(int); ok {
		return float64(i)
	}
	return math.NaN()
}

func zero(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

func per(total, n float64) float64 {
	if !(n > 0) {
		return math.NaN()
	}
	return total / n
}

// runMetrics are one run's figures for the report, by column.
func runMetrics(r map[string]any) map[string]float64 {
	c := func(path ...string) any { return dig(r, append([]string{"client"}, path...)...) }
	comp := func(name string) float64 { return num(dig(r, "client", "sources", "compositor", "counters", name)) }
	secs := num(c("seconds"))
	groups := comp("groupsSent")
	m := map[string]float64{
		"fps":         num(c("fps")),
		"lat50":       quantileMs(c("histograms", "commitToCallback"), 0.5),
		"lat95":       quantileMs(c("histograms", "commitToCallback"), 0.95),
		"input50":     quantileMs(c("histograms", "inputToCallback"), 0.5),
		"compMs":      per(comp("cpuUserNs")+comp("cpuSystemNs"), groups) / 1e6,
		"takeMs":      per(comp("takeBufferNs"), comp("buffersTaken")) / 1e6,
		"packMs":      per(comp("packNs"), groups) / 1e6,
		"compressMs":  per(comp("compressNs"), groups) / 1e6,
		"present50":   quantileMs(dig(r, "client", "sources", "compositor", "histograms", "sentToPresented"), 0.5),
		"wireMBs":     per(comp("bytesSent"), secs) / 1e6,
		"compPct":     num(c("guest", "procPctOfOne", "compositor")),
		"clientPct":   per(num(c("cpu", "userNs"))+num(c("cpu", "systemNs")), secs) / 1e7,
		"guestPct":    num(c("guest", "busyPctOfAll")) * num(c("guest", "cpus")),
		"appPct":      num(dig(r, "android", "cpu", "app")),
		"linkPct":     num(dig(r, "android", "cpu", "gui-link")),
		"vmPct":       num(dig(r, "android", "cpu", "vm")),
		"sfPct":       num(dig(r, "android", "cpu", "surfaceflinger")),
		"phonePct":    num(dig(r, "android", "cpu", "phoneBusyPctOfAll")),
		"powerMw":     num(dig(r, "android", "power", "powerMw")),
		"bufferW":     num(c("window", "bufferWidth")),
		"bufferH":     num(c("window", "bufferHeight")),
		"appInput50":  quantileMs(dig(r, "app", "histograms", "inputToCommitted"), 0.5),
		"appCommit50": quantileMs(dig(r, "app", "histograms", "applyToCommitted"), 0.5),
	}
	// The app's work a group, or the sink's on the desktop.
	if app, ok := dig(r, "app", "counters").(map[string]any); ok {
		n := num(app["groups"])
		m["appMs"] = per(num(app["decodeNs"])+num(app["applyNs"])+num(app["renderNs"]), n) / 1e6
		m["decodeMs"] = per(num(app["decodeNs"]), n) / 1e6
		m["copyMs"] = per(num(app["copyNs"]), n) / 1e6
		m["fenceMs"] = per(num(app["fenceWaitNs"]), n) / 1e6
	} else if sink, ok := dig(r, "client", "sources", "sink", "counters").(map[string]any); ok {
		n := num(sink["groups"])
		m["appMs"] = per(num(sink["decodeNs"])+num(sink["applyNs"]), n) / 1e6
		m["decodeMs"] = per(num(sink["decodeNs"]), n) / 1e6
		m["copyMs"] = per(num(sink["applyNs"]), n) / 1e6
		m["fenceMs"] = math.NaN()
	}
	if b, _ := dig(r, "android", "after", "charging").(bool); b {
		m["powerMw"] = math.NaN()
	}
	return m
}

type workloadRuns struct {
	name string
	runs []map[string]float64
	// Conditions worth a note: a warm or throttled phone, a charger.
	notes map[string]bool
}

func loadRuns(dir string) ([]*workloadRuns, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var order []*workloadRuns
	by := map[string]*workloadRuns{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r map[string]any
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		name, _ := r["workload"].(string)
		w := by[name]
		if w == nil {
			w = &workloadRuns{name: name, notes: map[string]bool{}}
			by[name] = w
			order = append(order, w)
		}
		w.runs = append(w.runs, runMetrics(r))
		if s := num(dig(r, "android", "after", "thermalStatus")); s > 0 {
			w.notes[fmt.Sprintf("thermal status %g", s)] = true
		}
		if b, _ := dig(r, "android", "after", "charging").(bool); b {
			w.notes["charging: no power figure"] = true
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("no runs in %s", dir)
	}
	return order, nil
}

func (w *workloadRuns) median(key string) float64 {
	var vs []float64
	for _, r := range w.runs {
		if v, ok := r[key]; ok && !math.IsNaN(v) {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return math.NaN()
	}
	sort.Float64s(vs)
	if len(vs)%2 == 1 {
		return vs[len(vs)/2]
	}
	return (vs[len(vs)/2-1] + vs[len(vs)/2]) / 2
}

func (w *workloadRuns) spread(key string) (float64, float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, r := range w.runs {
		if v, ok := r[key]; ok && !math.IsNaN(v) {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	return lo, hi
}

func f1(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

// benchReport is the medians of each workload's runs in dir, and their
// change from baseline's when given.
func benchReport(dir, baseline string) (string, error) {
	runs, err := loadRuns(dir)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: medians of each workload's runs\n\n", dir)
	t := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(t, "workload\tbuffer\truns\tfps (range)\tlatency p50\tp95 ms\tinput p50\tcompositor ms/f\tapp ms/f\twire MB/s\tcomp %\tclient %\tguest %\tapp %\tvm %\tsf %\tphone %\tmW\t")
	for _, w := range runs {
		lo, hi := w.spread("fps")
		fmt.Fprintf(t, "%s\t%sx%s\t%d\t%s (%s–%s)\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n",
			w.name, fmt.Sprint(w.median("bufferW")), fmt.Sprint(w.median("bufferH")), len(w.runs),
			f1(w.median("fps")), f1(lo), f1(hi), f1(w.median("lat50")), f1(w.median("lat95")), f1(w.median("input50")),
			f1(w.median("compMs")), f1(w.median("appMs")), f2(w.median("wireMBs")),
			f1(w.median("compPct")), f1(w.median("clientPct")), f1(w.median("guestPct")),
			f1(w.median("appPct")), f1(w.median("vmPct")), f1(w.median("sfPct")), f1(w.median("phonePct")), f1(w.median("powerMw")))
	}
	t.Flush()
	fmt.Fprintf(&sb, "\nstages, ms a frame (compositor: take = shm copy and scroll detection per buffer, pack, compress; app: decode, buffer copy, fence wait), and latencies in ms\n\n")
	t = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(t, "workload\ttake\tpack\tcompress\tdecode\tcopy\tfence\tsent→presented p50\tSF commit p50\tapp input→shown p50\t")
	for _, w := range runs {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", w.name,
			f1(w.median("takeMs")), f1(w.median("packMs")), f1(w.median("compressMs")),
			f1(w.median("decodeMs")), f1(w.median("copyMs")), f1(w.median("fenceMs")),
			f1(w.median("present50")), f1(w.median("appCommit50")), f1(w.median("appInput50")))
	}
	t.Flush()
	for _, w := range runs {
		var notes []string
		for n := range w.notes {
			notes = append(notes, n)
		}
		if len(notes) > 0 {
			sort.Strings(notes)
			fmt.Fprintf(&sb, "\n%s: %s", w.name, strings.Join(notes, ", "))
		}
	}
	if baseline != "" {
		base, err := loadRuns(baseline)
		if err != nil {
			return sb.String(), err
		}
		fmt.Fprintf(&sb, "\n\nagainst %s (positive is more)\n\n", baseline)
		t = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
		cols := []struct{ key, title string }{
			{"fps", "fps"}, {"lat50", "latency p50"}, {"lat95", "p95"}, {"compMs", "compositor ms/f"},
			{"appMs", "app ms/f"}, {"wireMBs", "wire MB/s"}, {"guestPct", "guest %"}, {"phonePct", "phone %"},
		}
		fmt.Fprint(t, "workload\t")
		for _, c := range cols {
			fmt.Fprint(t, c.title+"\t")
		}
		fmt.Fprintln(t)
		for _, w := range runs {
			var b *workloadRuns
			for _, x := range base {
				if x.name == w.name {
					b = x
				}
			}
			if b == nil {
				continue
			}
			fmt.Fprint(t, w.name+"\t")
			for _, c := range cols {
				now, was := w.median(c.key), b.median(c.key)
				fmt.Fprintf(t, "%s → %s (%s)\t", f1(was), f1(now), change(was, now))
			}
			fmt.Fprintln(t)
		}
		t.Flush()
	}
	sb.WriteString("\n")
	return sb.String(), nil
}

func change(was, now float64) string {
	if math.IsNaN(was) || math.IsNaN(now) || was == 0 {
		return "-"
	}
	return fmt.Sprintf("%+.0f%%", (now-was)/was*100)
}

func f2(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "-"
	}
	return fmt.Sprintf("%.2f", v)
}
