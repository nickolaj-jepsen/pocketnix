package agent

import (
	"os"
	"testing"
)

func TestPinFramesRefusesSizesOutOfRange(t *testing.T) {
	for _, n := range []int64{0, -1, maxFrameBuffers + 1} {
		if fb, err := pinFrames(n); err == nil {
			fb.close()
			t.Errorf("pinFrames(%d) succeeded", n)
		}
	}
}

func TestPinFramesListsEveryPage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pagemap shows page frames only to root")
	}
	page := int64(os.Getpagesize())
	fb, err := pinFrames(3*page + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.close()
	if len(fb.pages) != 4 {
		t.Fatalf("%d pages for 3 pages and a byte, want 4", len(fb.pages))
	}
	seen := map[uint64]bool{}
	for _, p := range fb.pages {
		if p%uint64(page) != 0 || seen[p] {
			t.Errorf("page address %#x: unaligned or repeated", p)
		}
		seen[p] = true
	}
}
