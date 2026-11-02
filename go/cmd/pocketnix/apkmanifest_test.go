package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"unicode/utf16"
)

func u16(n int) []byte { return binary.LittleEndian.AppendUint16(nil, uint16(n)) }

// axmlChunk is a binary XML chunk: its type and header size, its total size,
// the rest of its header, and its body.
func axmlChunk(typ int, header, body []byte) []byte {
	return bytes.Join([][]byte{u16(typ), u16(8 + len(header)), u32(8 + len(header) + len(body)), header, body}, nil)
}

// axmlPool is a string pool chunk in the encoding aapt2 picks, UTF-16 unless utf8.
func axmlPool(strs []string, utf8 bool) []byte {
	var offsets, data []byte
	for _, s := range strs {
		offsets = append(offsets, u32(len(data))...)
		if utf8 {
			// Both lengths, then the bytes and a NUL.
			data = append(append(append(data, byte(len(s)), byte(len(s))), s...), 0)
			continue
		}
		data = append(data, u16(len(s))...)
		for _, c := range utf16.Encode([]rune(s)) {
			data = append(data, u16(int(c))...)
		}
		data = append(data, 0, 0)
	}
	flags := 0
	if utf8 {
		flags = 1 << 8
	}
	const poolHeader = 28
	header := bytes.Join([][]byte{u32(len(strs)), u32(0), u32(flags), u32(poolHeader + len(offsets)), u32(0)}, nil)
	return axmlChunk(0x0001, header, append(offsets, data...))
}

const axmlNone = -1

// axmlAttr is an attribute of string indices; a value that isn't a string has axmlNone.
func axmlAttr(ns, name, value int) []byte {
	return bytes.Join([][]byte{u32(ns), u32(name), u32(value), u16(8), {0, 3}, u32(value)}, nil)
}

// axmlManifest is a compiled manifest's shape: a pool, a resource map, the
// android namespace and the manifest element, with these attributes after versionCode.
func axmlManifest(utf8 bool, extra ...string) []byte {
	strs := append([]string{"versionCode", "android", "http://schemas.android.com/apk/res/android", "manifest"}, extra...)
	attrs := axmlAttr(2, 0, axmlNone)
	for i := 0; i+1 < len(extra); i += 2 {
		attrs = append(attrs, axmlAttr(axmlNone, 4+i, 4+i+1)...)
	}
	element := bytes.Join([][]byte{u32(axmlNone), u32(3), u16(20), u16(20), u16(1 + len(extra)/2), u16(0), u16(0), u16(0), attrs}, nil)
	body := bytes.Join([][]byte{
		axmlPool(strs, utf8),
		axmlChunk(0x0180, nil, u32(0x0101021b)),
		axmlChunk(0x0100, append(u32(1), u32(axmlNone)...), append(u32(1), u32(2)...)),
		axmlChunk(0x0102, append(u32(1), u32(axmlNone)...), element),
	}, nil)
	return axmlChunk(0x0003, nil, body)
}

// The framework's ids for the attributes the CLI reads, as aapt2 writes them.
const (
	axmlIDVersionCode      = 0x0101021b
	axmlIDVersionCodeMajor = 0x01010576
	axmlIDDebuggable       = 0x0101000f
)

// axmlTyped is an attribute with a typed value: no raw string.
func axmlTyped(ns, name int, typ byte, data uint32) []byte {
	return bytes.Join([][]byte{u32(ns), u32(name), u32(axmlNone), u16(8), {0, typ}, binary.LittleEndian.AppendUint32(nil, data)}, nil)
}

// axmlStart is a start element's chunk.
func axmlStart(name int, attrs ...[]byte) []byte {
	body := bytes.Join([][]byte{u32(axmlNone), u32(name), u16(20), u16(20), u16(len(attrs)), u16(0), u16(0), u16(0), bytes.Join(attrs, nil)}, nil)
	return axmlChunk(0x0102, append(u32(1), u32(axmlNone)...), body)
}

// axmlSpec describes a compiled manifest: a package, then what the manifest and
// application elements hold. A zero codeType leaves versionCode out.
type axmlSpec struct {
	pkg               string
	code, major       uint32
	codeType          byte
	debuggable        *uint32
	debuggableType    byte
	noApplication     bool
	blankAttrNames    bool
}

// axmlManifestOf is the manifest spec describes, with attributes and resource
// map in the order aapt2 writes them.
func axmlManifestOf(spec axmlSpec) []byte {
	const (
		strCode = iota
		strMajor
		strDebuggable
		strAndroid
		strNS
		strManifest
		strUsesSDK
		strApplication
		strPackage
		strPkg
	)
	names := []string{"versionCode", "versionCodeMajor", "debuggable"}
	if spec.blankAttrNames {
		// Resource shrinkers can leave only the map to go by.
		names = []string{"", "", ""}
	}
	strs := append(names, "android", "http://schemas.android.com/apk/res/android", "manifest", "uses-sdk", "application", "package", spec.pkg)
	var attrs [][]byte
	if spec.codeType != 0 {
		attrs = append(attrs, axmlTyped(strNS, strCode, spec.codeType, spec.code))
	}
	if spec.major != 0 {
		attrs = append(attrs, axmlTyped(strNS, strMajor, 0x10, spec.major))
	}
	attrs = append(attrs, axmlAttr(axmlNone, strPackage, strPkg))
	var appAttrs [][]byte
	if spec.debuggable != nil {
		appAttrs = append(appAttrs, axmlTyped(strNS, strDebuggable, spec.debuggableType, *spec.debuggable))
	}
	chunks := [][]byte{
		axmlPool(strs, false),
		axmlChunk(0x0180, nil, bytes.Join([][]byte{u32(axmlIDVersionCode), u32(axmlIDVersionCodeMajor), u32(axmlIDDebuggable)}, nil)),
		axmlChunk(0x0100, append(u32(1), u32(axmlNone)...), append(u32(strAndroid), u32(strNS)...)),
		axmlStart(strManifest, attrs...),
		axmlStart(strUsesSDK),
	}
	if !spec.noApplication {
		chunks = append(chunks, axmlStart(strApplication, appAttrs...))
	}
	return axmlChunk(0x0003, nil, bytes.Join(chunks, nil))
}

// fakeVersionedAPK is an APK of pkg at versionCode code, with the marks the fake
// device reads, as a release build is (android:debuggable left out) unless debuggable.
func fakeVersionedAPK(pkg, build string, code uint32, debuggable bool) []byte {
	spec := axmlSpec{pkg: pkg, code: code, codeType: 0x10}
	files := map[string][]byte{"build": []byte(build), fakeVersionCode: []byte(strconv.Itoa(int(code)))}
	if debuggable {
		spec.debuggable, spec.debuggableType = new(uint32(0xffffffff)), 0x12
		files[fakeDebuggable] = nil
	}
	files["AndroidManifest.xml"] = axmlManifestOf(spec)
	return zipOf(files)
}

// fakeAPK is an APK of pkg that holds build, which fakeDevice.installedAPK reads back.
func fakeAPK(pkg, build string) []byte {
	return zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(false, "package", pkg), "build": []byte(build)})
}

func zipOf(files map[string][]byte) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, content := range files {
		f, _ := w.Create(name)
		f.Write(content)
	}
	w.Close()
	return b.Bytes()
}

func TestAPKPackage(t *testing.T) {
	for _, tc := range []struct {
		name string
		apk  []byte
		want string
	}{
		{"utf-16", zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(false, "package", debugPackage)}), debugPackage},
		{"utf-8", zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(true, "package", releasePackage)}), releasePackage},
		{"after-other-attributes", zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(false, "platformBuildVersionCode", "37", "package", debugPackage)}), debugPackage},
		{"not-a-zip", []byte("a dev build"), ""},
		{"no-manifest", zipOf(map[string][]byte{"classes.dex": []byte("dex")}), ""},
		{"text-manifest", zipOf(map[string][]byte{"AndroidManifest.xml": []byte(`<manifest package="dev.nickolaj.pocketnix"/>`)}), ""},
		{"no-package", zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(false)}), ""},
		{"truncated", zipOf(map[string][]byte{"AndroidManifest.xml": axmlManifest(false, "package", debugPackage)[:40]}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "app.apk")
			os.WriteFile(p, tc.apk, 0o644)
			m, err := readAPKManifest(p)
			if tc.want == "" {
				if err == nil {
					t.Errorf("read package %q, want an error", m.pkg)
				}
				return
			}
			if err != nil || m.pkg != tc.want {
				t.Errorf("got %q, %v, want %s", m.pkg, err, tc.want)
			}
		})
	}
}

// shown is a pointer's value for a test's message.
func shown[T any](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func TestManifestVersionAndDebuggable(t *testing.T) {
	trueValue, falseValue := new(uint32(0xffffffff)), new(uint32(0))
	for _, tc := range []struct {
		name       string
		spec       axmlSpec
		code       *int64
		debuggable *bool
	}{
		{name: "release-build", spec: axmlSpec{code: 10203, codeType: 0x10}, code: new(int64(10203)), debuggable: new(false)},
		{name: "debug-build", spec: axmlSpec{code: 10203, codeType: 0x10, debuggable: trueValue, debuggableType: 0x12}, code: new(int64(10203)), debuggable: new(true)},
		{name: "debuggable-false", spec: axmlSpec{code: 1, codeType: 0x10, debuggable: falseValue, debuggableType: 0x12}, code: new(int64(1)), debuggable: new(false)},
		{name: "hex-version", spec: axmlSpec{code: 0x10, codeType: 0x11}, code: new(int64(16)), debuggable: new(false)},
		{name: "major-version", spec: axmlSpec{code: 7, major: 2, codeType: 0x10}, code: new(int64(2<<32 | 7)), debuggable: new(false)},
		{name: "names-blanked", spec: axmlSpec{code: 7, codeType: 0x10, debuggable: trueValue, debuggableType: 0x12, blankAttrNames: true}, code: new(int64(7)), debuggable: new(true)},
		{name: "no-version", spec: axmlSpec{}, debuggable: new(false)},
		{name: "version-by-reference", spec: axmlSpec{code: 0x7f010001, codeType: 0x01}, debuggable: new(false)},
		{name: "debuggable-by-reference", spec: axmlSpec{code: 7, codeType: 0x10, debuggable: trueValue, debuggableType: 0x01}, code: new(int64(7))},
		{name: "no-application", spec: axmlSpec{code: 7, codeType: 0x10, noApplication: true}, code: new(int64(7))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.pkg = debugPackage
			m, err := parseManifest(axmlManifestOf(tc.spec))
			if err != nil {
				t.Fatal(err)
			}
			if m.pkg != debugPackage {
				t.Errorf("package = %q", m.pkg)
			}
			if !reflect.DeepEqual(m.versionCode, tc.code) {
				t.Errorf("versionCode = %s, want %s", shown(m.versionCode), shown(tc.code))
			}
			if !reflect.DeepEqual(m.debuggable, tc.debuggable) {
				t.Errorf("debuggable = %s, want %s", shown(m.debuggable), shown(tc.debuggable))
			}
		})
	}
}

// A manifest compiled before this read versionCode as a string has none to give.
func TestManifestWithoutTypedVersion(t *testing.T) {
	m, err := parseManifest(axmlManifest(false, "package", debugPackage))
	if err != nil || m.pkg != debugPackage || m.versionCode != nil {
		t.Errorf("got %+v, %v", m, err)
	}
}

// Every damaged manifest gives an error or fewer fields, rather than a panic or
// a package it doesn't name.
func TestManifestSurvivesDamage(t *testing.T) {
	whole := axmlManifestOf(axmlSpec{pkg: debugPackage, code: 7, major: 2, codeType: 0x10, debuggable: new(uint32(1)), debuggableType: 0x12})
	for n := range whole {
		if m, err := parseManifest(whole[:n]); err == nil && m.pkg != debugPackage {
			t.Errorf("%d bytes: read package %q", n, m.pkg)
		}
	}
	for i := range whole {
		for _, v := range []byte{0x00, 0x7f, 0xff} {
			damaged := slices.Clone(whole)
			damaged[i] = v
			parseManifest(damaged)
		}
	}
}
