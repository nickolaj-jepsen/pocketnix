package protocol

import "testing"

func TestNegotiate(t *testing.T) {
	for _, tc := range []struct {
		min, max, want int
		ok             bool
	}{
		{0, Version, Version, true},
		{MinVersion, Version + 5, Version, true}, // a newer app that still speaks ours
		{Version + 1, Version + 5, 0, false},     // one that doesn't
		{0, Version + 1, 0, false},               // an old-style Hello from a newer app
		{0, MinVersion - 1, 0, false},
	} {
		v, ok := Negotiate(tc.min, tc.max)
		if ok != tc.ok || (ok && v != tc.want) {
			t.Errorf("Negotiate(%d, %d) = %d, %v; want %d, %v", tc.min, tc.max, v, ok, tc.want, tc.ok)
		}
	}
}
