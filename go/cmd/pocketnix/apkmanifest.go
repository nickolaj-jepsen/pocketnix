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
	axmlTypeResourceMap  = 0x0180
)

// The types of an attribute's typed value (Res_value in ResourceTypes.h).
const (
	axmlValueIntDec = 0x10
	axmlValueIntHex = 0x11
	axmlValueBool   = 0x12
)

// The framework's resource ids of the attributes read here (android.R.attr).
const (
	attrVersionCode      = 0x0101021b
	attrVersionCodeMajor = 0x01010576
	attrDebuggable       = 0x0101000f
)

const (
	// axmlUTF8 is the string pool flag for UTF-8 strings, rather than UTF-16.
	axmlUTF8 = 1 << 8
	// axmlNoIndex is a string reference to nothing: an attribute without a namespace.
	axmlNoIndex = 0xffffffff
	// axmlMaxManifest bounds what's read of an APK's manifest; a real one is some KiB.
	axmlMaxManifest = 16 << 20
	// axmlTypedAttr is the size of an attribute with its typed value.
	axmlTypedAttr = 20
)

// apkManifest is what the CLI reads of an APK's binary AndroidManifest.xml.
type apkManifest struct {
	pkg string
	// versionCode is the long version code Android compares, nil when unreadable.
	versionCode *int64
	// debuggable is the application's android:debuggable, whose default is false;
	// nil when unreadable.
	debuggable *bool
}

// checkAPKPackage returns an error unless the APK at path is pkg. Installing
// another package would replace that app, killing its VM, with no backup of it.
func checkAPKPackage(path, pkg string) error {
	m, err := readAPKManifest(path)
	if err != nil {
		return fmt.Errorf("can't read the package of %s, so not installing it: %w", path, err)
	}
	got := m.pkg
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

// readAPKManifest reads the binary AndroidManifest.xml of the APK at path. It
// errors on a file that isn't an APK; a manifest that names its package but
// not the rest leaves those fields nil.
func readAPKManifest(path string) (apkManifest, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return apkManifest{}, err
	}
	defer z.Close()
	f, err := z.Open("AndroidManifest.xml")
	if err != nil {
		return apkManifest{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, axmlMaxManifest))
	if err != nil {
		return apkManifest{}, err
	}
	return parseManifest(b)
}

// parseManifest reads a binary manifest: the first element's package, which is
// the applicationId the app installs as, and version code, and the application
// element's debuggable flag. Damage after the first element only leaves fields nil.
func parseManifest(b []byte) (apkManifest, error) {
	var m apkManifest
	if len(b) < 8 || binary.LittleEndian.Uint16(b) != axmlTypeXML {
		return m, errors.New("the manifest isn't binary XML")
	}
	var pool, resMap []byte
	first := true
	for off := int(binary.LittleEndian.Uint16(b[2:])); off+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[off+4:]))
		if size < 8 || size > len(b)-off {
			if !first {
				return m, nil
			}
			return m, errors.New("the manifest has a chunk of a bad size")
		}
		chunk := b[off : off+size]
		switch binary.LittleEndian.Uint16(chunk) {
		case axmlTypeStringPool:
			pool = chunk
		case axmlTypeResourceMap:
			resMap = chunk
		case axmlTypeStartElement:
			attrs, err := elementAttrs(chunk)
			switch {
			case first && err != nil:
				return m, err
			case first:
				first = false
				if m.pkg, err = attrsPackage(attrs, pool); err != nil {
					return m, err
				}
				m.versionCode = attrsVersionCode(attrs, pool, resMap)
			case err != nil:
				return m, nil
			case elementName(chunk, pool) == "application":
				m.debuggable = attrsDebuggable(attrs, pool, resMap)
				return m, nil
			}
		}
		off += size
	}
	if first {
		return m, errors.New("the manifest has no element")
	}
	return m, nil
}

// elementName is a start element's name, empty if it can't be read.
func elementName(chunk, pool []byte) string {
	ext := int(binary.LittleEndian.Uint16(chunk[2:]))
	if ext+8 > len(chunk) {
		return ""
	}
	name, _ := poolString(pool, binary.LittleEndian.Uint32(chunk[ext+4:]))
	return name
}

// elementAttrs splits a start element into its attributes, each of 12 bytes or more.
func elementAttrs(chunk []byte) ([][]byte, error) {
	if len(chunk) < 4 {
		return nil, errors.New("the manifest has a truncated element")
	}
	// The header, then ns, name, attributeStart, attributeSize and attributeCount.
	ext := int(binary.LittleEndian.Uint16(chunk[2:]))
	if ext+14 > len(chunk) {
		return nil, errors.New("the manifest has a truncated element")
	}
	start := ext + int(binary.LittleEndian.Uint16(chunk[ext+8:]))
	size := int(binary.LittleEndian.Uint16(chunk[ext+10:]))
	count := int(binary.LittleEndian.Uint16(chunk[ext+12:]))
	if size < 12 {
		return nil, errors.New("the manifest has attributes of a bad size")
	}
	attrs := make([][]byte, 0, min(count, len(chunk)/size))
	for i := range count {
		a := start + i*size
		if a+12 > len(chunk) {
			return nil, errors.New("the manifest has a truncated element")
		}
		attrs = append(attrs, chunk[a:min(a+size, len(chunk))])
	}
	return attrs, nil
}

// attrsPackage is the value of the attribute named package without a namespace.
func attrsPackage(attrs [][]byte, pool []byte) (string, error) {
	for _, a := range attrs {
		if binary.LittleEndian.Uint32(a) != axmlNoIndex {
			continue
		}
		name, ok := poolString(pool, binary.LittleEndian.Uint32(a[4:]))
		if !ok || name != "package" {
			continue
		}
		if value, ok := poolString(pool, binary.LittleEndian.Uint32(a[8:])); ok && value != "" {
			return value, nil
		}
		return "", errors.New("the manifest's package isn't a string")
	}
	return "", errors.New("the manifest names no package")
}

// attrsVersionCode is the long version code: versionCodeMajor over versionCode.
func attrsVersionCode(attrs [][]byte, pool, resMap []byte) *int64 {
	code, ok := attrInt(findAttr(attrs, pool, resMap, attrVersionCode, "versionCode"))
	if !ok {
		return nil
	}
	long := int64(code)
	if a := findAttr(attrs, pool, resMap, attrVersionCodeMajor, "versionCodeMajor"); a != nil {
		major, ok := attrInt(a)
		if !ok {
			return nil
		}
		long |= int64(major) << 32
	}
	return &long
}

// attrsDebuggable is android:debuggable, false when it's left out.
func attrsDebuggable(attrs [][]byte, pool, resMap []byte) *bool {
	a := findAttr(attrs, pool, resMap, attrDebuggable, "debuggable")
	if a == nil {
		return new(false)
	}
	if len(a) < axmlTypedAttr || a[15] != axmlValueBool {
		return nil
	}
	return new(binary.LittleEndian.Uint32(a[16:]) != 0)
}

// attrInt is an attribute's integer value, false for a nil or any other attribute.
func attrInt(a []byte) (uint32, bool) {
	if len(a) < axmlTypedAttr || (a[15] != axmlValueIntDec && a[15] != axmlValueIntHex) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(a[16:]), true
}

// findAttr is the framework attribute id among attrs, nil without one. The
// resource map decides where it covers the attribute's name, as it does for
// Android: a shrinker can blank the names.
func findAttr(attrs [][]byte, pool, resMap []byte, id uint32, name string) []byte {
	for _, a := range attrs {
		i := binary.LittleEndian.Uint32(a[4:])
		if len(resMap) >= 8 {
			at := uint64(binary.LittleEndian.Uint16(resMap[2:])) + uint64(i)*4
			if at+4 <= uint64(len(resMap)) {
				if binary.LittleEndian.Uint32(resMap[at:]) == id {
					return a
				}
				continue
			}
		}
		if binary.LittleEndian.Uint32(a) == axmlNoIndex {
			continue
		}
		if s, ok := poolString(pool, i); ok && s == name {
			return a
		}
	}
	return nil
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
