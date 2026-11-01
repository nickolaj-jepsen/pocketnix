package agent

import "testing"

func TestBalloonClusters(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		blocks, bavail, reserved, want uint64
		reservedClusters               uint64
	}{
		// 64 GiB of 4 KiB blocks, 40 GiB of it available under ext4's own 4096.
		{"caps at the phone's space", 16777216, 10485760, 4096, 2621440, 4096 + 10485760 - 2621440},
		// The same filesystem with that balloon in place: it stays put.
		{"keeps a balloon that fits", 16777216, 2621440, 4096 + 10485760 - 2621440, 2621440, 4096 + 10485760 - 2621440},
		{"lets go when the phone has room", 16777216, 2621440, 4096 + 10485760 - 2621440, 20971520, 4096},
		// 100 MiB: 2% of it is under 4096 clusters.
		{"keeps ext4's own reserve", 25600, 10000, 512, 1 << 30, 512},
		{"never the whole filesystem", 262144, 262144, 4096, 0, 262143},
	} {
		if got := balloonClusters(tc.blocks, tc.bavail, tc.reserved, tc.want); got != tc.reservedClusters {
			t.Errorf("%s: reserved_clusters %d, want %d", tc.name, got, tc.reservedClusters)
		}
	}
}
