// Package image edits a pocketnix disk image on the build host: finding its
// root partition and writing files into it with debugfs, unprivileged.
package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
)

// Partition is one GPT entry.
type Partition struct {
	Name   string
	Offset int64 // bytes from the start of the image
	Size   int64
}

const sector = 512

// Partitions reads the GPT of a 512-byte-sector image.
func Partitions(r io.ReaderAt) ([]Partition, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, sector); err != nil {
		return nil, fmt.Errorf("read GPT header: %w", err)
	}
	if !bytes.Equal(hdr[:8], []byte("EFI PART")) {
		return nil, errors.New("no GPT header at LBA 1 (512-byte sectors)")
	}
	entriesLBA := int64(binary.LittleEndian.Uint64(hdr[72:]))
	count := binary.LittleEndian.Uint32(hdr[80:])
	size := binary.LittleEndian.Uint32(hdr[84:])
	if size < 128 || count > 1024 {
		return nil, fmt.Errorf("implausible GPT entry table (%d entries of %d bytes)", count, size)
	}
	table := make([]byte, int(count)*int(size))
	if _, err := r.ReadAt(table, entriesLBA*sector); err != nil {
		return nil, fmt.Errorf("read GPT entries: %w", err)
	}
	var parts []Partition
	for i := 0; i < int(count); i++ {
		e := table[i*int(size) : (i+1)*int(size)]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue // unused entry
		}
		first := int64(binary.LittleEndian.Uint64(e[32:]))
		last := int64(binary.LittleEndian.Uint64(e[40:]))
		parts = append(parts, Partition{
			Name:   decodeName(e[56:128]),
			Offset: first * sector,
			Size:   (last - first + 1) * sector,
		})
	}
	return parts, nil
}

func decodeName(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

const (
	ext4MagicOffset = 1024 + 0x38
	ext4LabelOffset = 1024 + 0x78
)

// ext4Label returns the volume label of the ext4 filesystem at p, or false
// if p doesn't hold one.
func ext4Label(r io.ReaderAt, p Partition) (string, bool) {
	sb := make([]byte, 2)
	if _, err := r.ReadAt(sb, p.Offset+ext4MagicOffset); err != nil || binary.LittleEndian.Uint16(sb) != 0xef53 {
		return "", false
	}
	label := make([]byte, 16)
	if _, err := r.ReadAt(label, p.Offset+ext4LabelOffset); err != nil {
		return "", false
	}
	return string(bytes.TrimRight(label, "\x00")), true
}

// RootPartition finds the image's ext4 root: the one labelled "nixos" (as
// the module labels it), or the only ext4 partition.
func RootPartition(path string) (Partition, error) {
	f, err := os.Open(path)
	if err != nil {
		return Partition{}, err
	}
	defer f.Close()
	parts, err := Partitions(f)
	if err != nil {
		return Partition{}, fmt.Errorf("%s: %w", path, err)
	}
	var ext4 []Partition
	for _, p := range parts {
		label, ok := ext4Label(f, p)
		if !ok {
			continue
		}
		if label == "nixos" {
			return p, nil
		}
		ext4 = append(ext4, p)
	}
	if len(ext4) == 1 {
		return ext4[0], nil
	}
	return Partition{}, fmt.Errorf("%s: no ext4 partition labelled nixos among %d ext4 partitions", path, len(ext4))
}
