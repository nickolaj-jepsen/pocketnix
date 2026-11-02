package main

import (
	"strings"
	"testing"
)

// pixelLimiter is what `memory-limiter status` printed on a Pixel 8 Pro; the
// test adds its ignored field.
func pixelLimiter(ignored string) string {
	return `Memory limiter
  enabled                  limits=true              monitoring=true          killing=true             ignored=` + ignored + `
  visibleMem=6144MB        visibleSwap=3072MB       notVisibleMem=3072MB     notVisibleSwap=3072MB
  started=511              watched=538              watch-failed=0           events=30                false-events=1
`
}

// memoryPhone is a Pixel with the release app, whose VM has memoryMb MiB.
func memoryPhone(t *testing.T, memoryMb, ignored string) *fakeDevice {
	t.Helper()
	d := newFakeDevice(t)
	cannedPixel(d)
	d.installAPK([]byte("apk"))
	d.setCaps(pixelCaps + ", memoryMb=" + memoryMb)
	d.can(limiterStatus, pixelLimiter(ignored), 0)
	return d
}

func hasReason(v doctorVerdict, token string) bool {
	for _, r := range v.Reasons {
		if strings.Contains(r, token) {
			return true
		}
	}
	return false
}

func TestDoctorReadsTheLimiter(t *testing.T) {
	memoryPhone(t, "2048", "none")
	r, raw, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	l := r.Memory.Limiter
	if l == nil || !l.Enabled || !l.Limits || !l.Killing || l.Ignored != "" {
		t.Fatalf("limiter = %+v", l)
	}
	for name, got := range map[string]*int{"visible": l.VisibleMiB, "notVisible": l.NotVisibleMiB, "visibleSwap": l.VisibleSwapMiB, "notVisibleSwap": l.NotVisibleSwapMiB} {
		want := 3072
		if name == "visible" {
			want = 6144
		}
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %d", name, got, want)
		}
	}
	if field(raw, "memory.limiter.notVisibleMiB") != 3072.0 {
		t.Errorf("JSON memory.limiter.notVisibleMiB = %v", field(raw, "memory.limiter.notVisibleMiB"))
	}
	if r.Verdict.Level != levelOK {
		t.Errorf("a 2048 MiB VM under a 3072 MiB limit: verdict %+v", r.Verdict)
	}
}

func TestDoctorWarnsOfAVMOverTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name, memoryMb, ignored string
		warns                   bool
	}{
		{"over", "4096", "none", true},
		{"too close for the overhead", "3000", "none", true},
		{"a fitting VM", "2800", "none", false},
		{"over, the release app's uid ignored", "4096", "10100", false},
		{"over, every uid ignored", "4096", "all", false},
		{"over, another app's uid ignored", "4096", "10999", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memoryPhone(t, tc.memoryMb, tc.ignored)
			r, _, err := doctorJSON(t)
			if err != nil {
				t.Fatal(err)
			}
			if got := hasReason(r.Verdict, "freeze"); got != tc.warns {
				t.Errorf("warned %v, want %v: %+v", got, tc.warns, r.Verdict)
			}
		})
	}
}

func TestDoctorNeedsNoLimiter(t *testing.T) {
	t.Run("an older Android", func(t *testing.T) {
		d := memoryPhone(t, "8192", "none")
		d.can(limiterStatus, "Error: unknown sub-command\n", 255)
		r, raw, err := doctorJSON(t)
		if err != nil {
			t.Fatal(err)
		}
		if r.Memory.Limiter != nil || hasReason(r.Verdict, "freeze") {
			t.Errorf("limiter %+v, verdict %+v", r.Memory.Limiter, r.Verdict)
		}
		if _, ok := field(raw, "memory").(map[string]any)["limiter"]; !ok {
			t.Errorf("the JSON leaves out memory.limiter: %v", field(raw, "memory"))
		}
	})
	t.Run("disabled by a build", func(t *testing.T) {
		d := memoryPhone(t, "8192", "none")
		d.can(limiterStatus, "Memory limiter\n  disabled\n", 0)
		r, _, err := doctorJSON(t)
		if err != nil {
			t.Fatal(err)
		}
		if l := r.Memory.Limiter; l == nil || l.Enabled || hasReason(r.Verdict, "freeze") {
			t.Errorf("limiter %+v, verdict %+v", l, r.Verdict)
		}
	})
	t.Run("an app from before memoryMb", func(t *testing.T) {
		d := memoryPhone(t, "4096", "none")
		d.setCaps(pixelCaps)
		r, _, err := doctorJSON(t)
		if err != nil {
			t.Fatal(err)
		}
		if hasReason(r.Verdict, "freeze") {
			t.Errorf("verdict %+v", r.Verdict)
		}
	})
}

func TestDoctorReadsTheAppsMemory(t *testing.T) {
	d := memoryPhone(t, "2048", "none")
	d.can(appMemoryCommand(releasePackage), "state=2 (TOP)\nhigh=6442450944\nswapMax=3221225472\ncurrent=2067349504\npeak=5392592896\nhighEvents=17446\n", 0)
	r, _, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Memory.Process
	if p == nil || p.State != "2 (TOP)" || p.High != "6442450944" || p.Peak != "5392592896" || p.HighEvents != "17446" {
		t.Errorf("process = %+v", p)
	}
	out, err := stdoutOf(t, func() error { return runDoctor(nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"3072", "5392592896"} {
		if !strings.Contains(out, token) {
			t.Errorf("the text lacks %q:\n%s", token, out)
		}
	}
}

func TestDoctorNamesALimiterKill(t *testing.T) {
	d := memoryPhone(t, "2048", "none")
	d.can("dumpsys activity exit-info "+releasePackage, `  package: dev.nickolaj.pocketnix
        ApplicationExitInfo #0:
          timestamp=2026-11-03 00:01:53.746 pid=22792 realUid=10100 packageUid=10100 definingUid=10100 user=0
          process=dev.nickolaj.pocketnix reason=13 (OTHER) subreason=0 (UNKNOWN) status=0
          description=MemoryLimiter:AnonSwap
        ApplicationExitInfo #1:
          timestamp=2026-11-02 21:29:40.517 pid=17855 realUid=10100 packageUid=10100 definingUid=10100 user=0
          process=dev.nickolaj.pocketnix reason=16 (PACKAGE UPDATED) subreason=0 (UNKNOWN) status=0
          description=stop dev.nickolaj.pocketnix due to killDueToPackageUpdate
`, 0)
	r, _, err := doctorJSON(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Memory.LimiterKills) != 1 || !strings.Contains(r.Memory.LimiterKills[0], "2026-11-03 00:01:53.746") {
		t.Errorf("limiterKills = %q", r.Memory.LimiterKills)
	}
	if r.Verdict.Level != levelWarn || !hasReason(r.Verdict, "MemoryLimiter:AnonSwap") {
		t.Errorf("verdict %+v", r.Verdict)
	}
}
