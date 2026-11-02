package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
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
			got, err := apkPackage(p)
			if tc.want == "" {
				if err == nil {
					t.Errorf("read package %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got %q, %v, want %s", got, err, tc.want)
			}
		})
	}
}

// Every damaged manifest gives an error rather than a panic.
func TestManifestPackageSurvivesTruncation(t *testing.T) {
	whole := axmlManifest(false, "package", debugPackage)
	for n := range whole {
		if got, err := manifestPackage(whole[:n]); err == nil && !strings.HasPrefix(got, "dev.") {
			t.Errorf("%d bytes: read package %q", n, got)
		}
	}
}
