package main

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

// The chunk types of Android's binary XML (ResourceTypes.h).
const (
	axmlTypeXML          = 0x0003
	axmlTypeStringPool   = 0x0001
	axmlTypeStartElement = 0x0102
)

const (
	// axmlUTF8 is the string pool flag for UTF-8 strings, rather than UTF-16.
	axmlUTF8 = 1 << 8
	// axmlNoIndex is a string reference to nothing: an attribute without a namespace.
	axmlNoIndex = 0xffffffff
	// axmlMaxManifest bounds what's read of an APK's manifest; a real one is some KiB.
	axmlMaxManifest = 16 << 20
)

// checkAPKPackage returns an error unless the APK at path is pkg. Installing
// another package would replace that app, killing its VM, with no backup of it.
func checkAPKPackage(path, pkg string) error {
	got, err := apkPackage(path)
	if err != nil {
		return fmt.Errorf("can't read the package of %s, so not installing it: %w", path, err)
	}
	if got == pkg {
		return nil
	}
	hint := ""
	switch got {
	case debugPackage:
		hint = "; pass --debug to target that build"
	case releasePackage:
		hint = "; leave out --debug to target that build"
	}
	return fmt.Errorf("%s is %s, not %s, so not installing it%s", path, got, pkg, hint)
}

// apkPackage returns the package the binary AndroidManifest.xml of the APK at
// path declares. It errors on a file that isn't an APK.
func apkPackage(path string) (string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer z.Close()
	f, err := z.Open("AndroidManifest.xml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, axmlMaxManifest))
	if err != nil {
		return "", err
	}
	return manifestPackage(b)
}

// manifestPackage reads the package attribute of a binary manifest's first
// element, which is the applicationId the app installs as.
func manifestPackage(b []byte) (string, error) {
	if len(b) < 8 || binary.LittleEndian.Uint16(b) != axmlTypeXML {
		return "", errors.New("the manifest isn't binary XML")
	}
	var pool []byte
	for off := int(binary.LittleEndian.Uint16(b[2:])); off+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[off+4:]))
		if size < 8 || size > len(b)-off {
			return "", errors.New("the manifest has a chunk of a bad size")
		}
		chunk := b[off : off+size]
		switch binary.LittleEndian.Uint16(chunk) {
		case axmlTypeStringPool:
			pool = chunk
		case axmlTypeStartElement:
			return elementPackage(chunk, pool)
		}
		off += size
	}
	return "", errors.New("the manifest has no element")
}

// elementPackage is a start element's package attribute, by pool's strings.
func elementPackage(chunk, pool []byte) (string, error) {
	if len(chunk) < 4 {
		return "", errors.New("the manifest has a truncated element")
	}
	// The header, then ns, name, attributeStart, attributeSize and attributeCount.
	ext := int(binary.LittleEndian.Uint16(chunk[2:]))
	if ext+14 > len(chunk) {
		return "", errors.New("the manifest has a truncated element")
	}
	start := ext + int(binary.LittleEndian.Uint16(chunk[ext+8:]))
	size := int(binary.LittleEndian.Uint16(chunk[ext+10:]))
	count := int(binary.LittleEndian.Uint16(chunk[ext+12:]))
	if size < 12 {
		return "", errors.New("the manifest has attributes of a bad size")
	}
	for i := range count {
		a := start + i*size
		if a+12 > len(chunk) {
			return "", errors.New("the manifest has a truncated element")
		}
		if binary.LittleEndian.Uint32(chunk[a:]) != axmlNoIndex {
			continue
		}
		name, ok := poolString(pool, binary.LittleEndian.Uint32(chunk[a+4:]))
		if !ok || name != "package" {
			continue
		}
		if value, ok := poolString(pool, binary.LittleEndian.Uint32(chunk[a+8:])); ok && value != "" {
			return value, nil
		}
		return "", errors.New("the manifest's package isn't a string")
	}
	return "", errors.New("the manifest names no package")
}

// poolString is string i of a string pool chunk, false if it has none or it's damaged.
func poolString(pool []byte, i uint32) (string, bool) {
	const header = 28
	if len(pool) < header || i >= binary.LittleEndian.Uint32(pool[8:]) {
		return "", false
	}
	index := int(binary.LittleEndian.Uint16(pool[2:])) + int(i)*4
	if index+4 > len(pool) {
		return "", false
	}
	off := int(binary.LittleEndian.Uint32(pool[20:])) + int(binary.LittleEndian.Uint32(pool[index:]))
	if off < 0 || off > len(pool) {
		return "", false
	}
	s := pool[off:]
	if binary.LittleEndian.Uint32(pool[16:])&axmlUTF8 != 0 {
		// The length in UTF-16 units, then in bytes: one byte each, or two with the high bit set.
		_, s, ok := poolLen8(s)
		if !ok {
			return "", false
		}
		n, s, ok := poolLen8(s)
		if !ok || n > len(s) {
			return "", false
		}
		return string(s[:n]), true
	}
	if len(s) < 2 {
		return "", false
	}
	n := int(binary.LittleEndian.Uint16(s))
	s = s[2:]
	if n&0x8000 != 0 {
		if len(s) < 2 {
			return "", false
		}
		n = (n&0x7fff)<<16 | int(binary.LittleEndian.Uint16(s))
		s = s[2:]
	}
	if n*2 > len(s) {
		return "", false
	}
	units := make([]uint16, n)
	for j := range units {
		units[j] = binary.LittleEndian.Uint16(s[j*2:])
	}
	return string(utf16.Decode(units)), true
}

// poolLen8 reads a length of a UTF-8 pool string, and returns what follows it.
func poolLen8(s []byte) (int, []byte, bool) {
	if len(s) < 1 {
		return 0, nil, false
	}
	if s[0]&0x80 == 0 {
		return int(s[0]), s[1:], true
	}
	if len(s) < 2 {
		return 0, nil, false
	}
	return int(s[0]&0x7f)<<8 | int(s[1]), s[2:], true
}
