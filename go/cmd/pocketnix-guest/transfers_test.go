package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestFilterTypes(t *testing.T) {
	// As godbus decodes them off the wire: structs are []any.
	rules := func(r ...[]any) [][]any { return r }
	filters := dbus.MakeVariantWithSignature([][]any{
		{"Images", rules([]any{uint32(1), "image/png"}, []any{uint32(0), "*.jpg"})},
		{"Text", rules([]any{uint32(1), "text/plain"})},
	}, dbus.ParseSignatureMust("a(sa(us))"))
	if got := filterTypes(map[string]dbus.Variant{"filters": filters}); !slices.Equal(got, []string{"image/png", "image/jpeg", "text/plain"}) {
		t.Errorf("all filters = %q", got)
	}
	current := dbus.MakeVariantWithSignature([]any{"Text", rules([]any{uint32(1), "text/plain"})}, dbus.ParseSignatureMust("(sa(us))"))
	if got := filterTypes(map[string]dbus.Variant{"filters": filters, "current_filter": current}); !slices.Equal(got, []string{"text/plain"}) {
		t.Errorf("current filter = %q", got)
	}
	// A pattern without a type can't be said to Android's picker: show everything.
	all := dbus.MakeVariantWithSignature([][]any{{"All", rules([]any{uint32(0), "*"})}}, dbus.ParseSignatureMust("a(sa(us))"))
	if got := filterTypes(map[string]dbus.Variant{"filters": all}); got != nil {
		t.Errorf("a bare glob = %q", got)
	}
}

func TestSaveName(t *testing.T) {
	if n := saveName(map[string]dbus.Variant{"current_name": dbus.MakeVariant("a.txt")}); n != "a.txt" {
		t.Errorf("current_name gave %q", n)
	}
	if n := saveName(map[string]dbus.Variant{"current_file": dbus.MakeVariant([]byte("/home/u/b.odt\x00"))}); n != "b.odt" {
		t.Errorf("current_file gave %q", n)
	}
	if n := saveName(nil); n != "untitled" {
		t.Errorf("nothing gave %q", n)
	}
}

func TestMoveIntoKeepsExistingFiles(t *testing.T) {
	inbox, dest := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dest, "a.png"), []byte("old"), 0o644))
	// Named as it came, not as it landed in the inbox.
	src := filepath.Join(inbox, "a (3).png")
	must(os.WriteFile(src, []byte("new"), 0o644))
	got, err := moveInto(src, dest, "a.png")
	must(err)
	if got != filepath.Join(dest, "a (1).png") {
		t.Errorf("moved to %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.png")); string(b) != "old" {
		t.Errorf("replaced the existing file: %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("the source stayed: %v", err)
	}
}
