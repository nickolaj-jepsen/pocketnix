package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// The disk image is sparse and as large as the phone's storage, so the guest's
// free space would be the phone's twice over. The storage balloon caps it at
// what the phone has: ext4 holds its reserved_clusters (a sysfs knob) back
// from everyone, root too, and returns ENOSPC instead of the write failing
// on the phone.

// balloonClusters is the reserved_clusters that leaves want blocks free on a
// filesystem of blocks blocks, bavail of them available now while reserved
// are reserved. It never goes below ext4's own reserve against ENOSPC in
// delayed allocation (2% of the filesystem, at most 4096 clusters), and
// stays below the filesystem's size, which the kernel refuses.
func balloonClusters(blocks, bavail, reserved, want uint64) uint64 {
	base := min(blocks/50, 4096)
	// bavail already excludes what's reserved: add it back to get what the
	// balloon alone holds, on top of the base.
	free := int64(bavail) + int64(reserved) - int64(base)
	balloon := max(free-int64(want), 0)
	return min(base+uint64(balloon), blocks-1)
}

// applyBalloon caps the free space of the ext4 filesystem holding path at
// avail bytes.
func applyBalloon(path string, avail int64) error {
	knob, err := ext4Knob(path, "reserved_clusters")
	if err != nil {
		return err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return err
	}
	b, err := os.ReadFile(knob)
	if err != nil {
		return err
	}
	cur, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", knob, err)
	}
	// Clusters are blocks: the image's ext4 has no bigalloc.
	bsize := uint64(st.Bsize)
	next := balloonClusters(st.Blocks, st.Bavail, cur, uint64(max(avail, 0))/bsize)
	if next == cur {
		return nil
	}
	return os.WriteFile(knob, []byte(strconv.FormatUint(next, 10)), 0o644)
}

// rootStorage is the [protocol.Storage] of the filesystem holding path.
func rootStorage(path string) (protocol.Storage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return protocol.Storage{}, err
	}
	b := st.Bsize
	return protocol.Storage{
		Type:      protocol.TypeStorage,
		Total:     int64(st.Blocks) * b,
		Used:      int64(st.Blocks-st.Bfree) * b,
		Available: int64(st.Bavail) * b,
	}, nil
}

// ext4Knob is the sysfs file name of the ext4 filesystem holding path.
func ext4Knob(path, name string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	dev, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(st.Dev), unix.Minor(st.Dev)))
	if err != nil {
		return "", err
	}
	knob := filepath.Join("/sys/fs/ext4", filepath.Base(dev), name)
	if _, err := os.Stat(knob); err != nil {
		return "", fmt.Errorf("%s isn't on ext4: %w", path, err)
	}
	return knob, nil
}
