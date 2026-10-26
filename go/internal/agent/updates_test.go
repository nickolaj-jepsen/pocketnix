package agent_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// fakeGuest is a store, a system profile and /run links in a temp dir, with
// fake ssh, nix-store, nix and nix-env that act on them.
type fakeGuest struct {
	dir, store, profile, current string
	systems                      []string // toplevels; the first is generation 1 and running
}

func newFakeGuest(t *testing.T) *fakeGuest {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGuest{dir: dir, store: filepath.Join(dir, "store"), profile: filepath.Join(dir, "profiles", "system"), current: filepath.Join(dir, "run", "current-system")}
	for _, d := range []string{g.store, filepath.Dir(g.profile), filepath.Dir(g.current), filepath.Join(dir, "bin")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	for i, c := range []string{"a", "b"} {
		top := filepath.Join(g.store, strings.Repeat(c, 32)+fmt.Sprintf("-nixos-system-%d", i+1))
		must(t, os.MkdirAll(filepath.Join(top, "bin"), 0o755))
		must(t, os.WriteFile(filepath.Join(top, "nixos-version"), fmt.Appendf(nil, "26.05.%d\n", i+1), 0o644))
		g.script(t, filepath.Join(top, "bin", "switch-to-configuration"), fmt.Sprintf(`
[ "$1" = switch ] || exit 2
echo "activating %[1]s"
ln -sfn %[1]s %[2]s
`, top, g.current))
		g.systems = append(g.systems, top)
	}
	must(t, os.Symlink(g.systems[0], g.profile+"-1-link"))
	must(t, os.Symlink(g.profile+"-1-link", g.profile))
	must(t, os.Symlink(g.systems[0], g.current))
	must(t, os.Symlink(g.systems[0], g.booted()))

	// ssh prints what $dir/answer says, or fails like an unreachable host.
	g.script(t, g.bin("ssh"), fmt.Sprintf(`
case "$*" in *BatchMode=yes*) ;; *) echo "no BatchMode" >&2; exit 2 ;; esac
[ -f %[1]s/answer ] || { echo "ssh: connect to host builder port 22: No route to host" >&2; exit 255; }
echo "evaluating on the build host" >&2
cat %[1]s/answer
`, dir))
	// The guest's caches have a path when $dir/cached lists it.
	g.script(t, g.bin("nix-store"), fmt.Sprintf(`
[ "$1" = --realise ] || exit 2
grep -qxF "$2" %[1]s/cached 2>/dev/null || { echo "error: don't know how to build these paths: $2" >&2; exit 1; }
echo "copying path '$2' from 'https://cache.example'" >&2
`, dir))
	g.script(t, g.bin("nix"), fmt.Sprintf(`
[ "$3" = copy ] || exit 2
echo "$*" > %[1]s/copied
echo "copying path from ssh-ng: $NIX_SSHOPTS" >&2
`, dir))
	g.script(t, g.bin("nix-env"), fmt.Sprintf(`
p=$2
case "$3" in
--set)
  n=$(ls %[1]s | sed -n 's/^system-\([0-9]*\)-link$/\1/p' | sort -n | tail -n1)
  n=$((n + 1))
  ln -s "$4" "$p-$n-link" && ln -sfn "$p-$n-link" "$p" ;;
--switch-generation) ln -sfn "$p-$4-link" "$p" ;;
*) exit 2 ;;
esac
`, filepath.Dir(g.profile)))
	return g
}

func (g *fakeGuest) bin(name string) string { return filepath.Join(g.dir, "bin", name) }
func (g *fakeGuest) booted() string         { return filepath.Join(g.dir, "run", "booted-system") }

func (g *fakeGuest) script(t *testing.T, path, body string) {
	t.Helper()
	must(t, os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body), 0o755))
}

func (g *fakeGuest) answer(t *testing.T, s string) {
	must(t, os.WriteFile(filepath.Join(g.dir, "answer"), []byte(s+"\n"), 0o644))
}

func (g *fakeGuest) cache(t *testing.T, path string) {
	must(t, os.WriteFile(filepath.Join(g.dir, "cached"), []byte(path+"\n"), 0o644))
}

func (g *fakeGuest) copied() bool {
	_, err := os.Stat(filepath.Join(g.dir, "copied"))
	return err == nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// startUpdates serves updates (the config's "updates" object) on a fake guest.
func startUpdates(t *testing.T, g *fakeGuest, updates map[string]any) *client.Control {
	t.Helper()
	cfg := baseConfig()
	cfg["updates"] = updates
	h := start(t, cfg, unixAddr)
	u := h.srv.Updates
	u.SSH, u.NixStore, u.Nix, u.NixEnv, u.SystemdRun = g.bin("ssh"), g.bin("nix-store"), g.bin("nix"), g.bin("nix-env"), ""
	u.Profile, u.Current, u.Booted, u.StoreDir = g.profile, g.current, g.booted(), g.store
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	return ctl
}

var buildHost = map[string]any{"buildHost": map[string]any{"host": "nix@builder", "installable": "github:alice/nixos#nixosConfigurations.phone.config.system.build.toplevel", "sshKey": "/etc/ssh/ssh_host_ed25519_key"}}

func currentGeneration(u protocol.Updates) int {
	for _, g := range u.Generations {
		if g.Current {
			return g.Number
		}
	}
	return 0
}

func TestUpdateFromBuildHost(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	g.answer(t, g.systems[1])

	u, err := ctl.Run(ctl.UpdateCheck)
	if err != nil {
		t.Fatalf("check: %v (log %q)", err, u.Log)
	}
	if u.Available != g.systems[1] || u.Source != protocol.SourceBuildHost {
		t.Fatalf("check found %q from %q, want %q from the build host", u.Available, u.Source, g.systems[1])
	}
	if u.Current != g.systems[0] || u.CheckedAt == 0 {
		t.Errorf("current %q, checkedAt %d", u.Current, u.CheckedAt)
	}

	u, err = ctl.Run(ctl.UpdateApply)
	if err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	if u.Current != g.systems[1] || u.Available != "" {
		t.Errorf("after apply: current %q, available %q", u.Current, u.Available)
	}
	// Not in the guest's caches, so it came over ssh, with the configured key.
	if !g.copied() {
		t.Error("nothing copied from the build host")
	}
	if !strings.Contains(strings.Join(u.Log, "\n"), "-i /etc/ssh/ssh_host_ed25519_key") {
		t.Errorf("ssh key not passed to nix copy: %q", u.Log)
	}
	if n := currentGeneration(u); n != 2 || len(u.Generations) != 2 || u.Generations[0].Label != "26.05.2" {
		t.Errorf("generations = %+v", u.Generations)
	}
	if u.Generations[1].Booted != true {
		t.Errorf("generation 1 not marked booted: %+v", u.Generations)
	}
}

func TestUpdateUpToDate(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	g.answer(t, g.systems[0])
	u, err := ctl.Run(ctl.UpdateApply)
	if err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	if u.Available != "" || currentGeneration(u) != 1 || len(u.Generations) != 1 {
		t.Errorf("an up-to-date apply changed something: %+v", u)
	}
}

func TestUpdatePublishedSubstitutes(t *testing.T) {
	g := newFakeGuest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, g.systems[1])
	}))
	defer srv.Close()
	g.cache(t, g.systems[1])
	ctl := startUpdates(t, g, map[string]any{"published": map[string]any{"url": srv.URL + "/phone"}})
	u, err := ctl.Run(ctl.UpdateApply)
	if err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	if u.Current != g.systems[1] || g.copied() {
		t.Errorf("current %q, copied %v; want substituted %q", u.Current, g.copied(), g.systems[1])
	}
}

func TestUpdateFallsBackToPublished(t *testing.T) {
	g := newFakeGuest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, g.systems[1])
	}))
	defer srv.Close()
	// No answer file: the build host is unreachable.
	ctl := startUpdates(t, g, map[string]any{"buildHost": buildHost["buildHost"], "published": map[string]any{"url": srv.URL}})
	u, err := ctl.Run(ctl.UpdateCheck)
	if err != nil {
		t.Fatalf("check: %v (log %q)", err, u.Log)
	}
	if u.Available != g.systems[1] || u.Source != protocol.SourcePublished {
		t.Errorf("found %q from %q, want the published path", u.Available, u.Source)
	}
	if !strings.Contains(strings.Join(u.Log, "\n"), "No route to host") {
		t.Errorf("the build host's failure isn't logged: %q", u.Log)
	}
}

func TestUpdateRejectsNonStorePath(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	g.answer(t, "/tmp/evil")
	u, err := ctl.Run(ctl.UpdateApply)
	if err == nil || !strings.Contains(u.Error, "not a store path") {
		t.Fatalf("err %v, state error %q", err, u.Error)
	}
	if currentGeneration(u) != 1 {
		t.Errorf("switched anyway: %+v", u.Generations)
	}
}

func TestSwitchGenerationRollsBack(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	g.answer(t, g.systems[1])
	if u, err := ctl.Run(ctl.UpdateApply); err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	u, err := ctl.Run(func() error { return ctl.SwitchGeneration(1) })
	if err != nil {
		t.Fatalf("rollback: %v (log %q)", err, u.Log)
	}
	if u.Current != g.systems[0] || currentGeneration(u) != 1 || len(u.Generations) != 2 {
		t.Errorf("after rollback: current %q, generations %+v", u.Current, u.Generations)
	}
	if err := ctl.SwitchGeneration(7); err == nil {
		t.Error("switched to a generation that doesn't exist")
	}
}

func TestUpdatesUnconfigured(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, map[string]any{})
	u, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Sources) != 0 || len(u.Generations) != 1 {
		t.Errorf("state = %+v", u)
	}
	if err := ctl.UpdateCheck(); err == nil {
		t.Error("check without a source succeeded")
	}
	// Generations still work without a source.
	if _, err := ctl.Run(func() error { return ctl.SwitchGeneration(1) }); err != nil {
		t.Errorf("switch-generation: %v", err)
	}
}

func TestUpdatesBadConfig(t *testing.T) {
	for name, updates := range map[string]map[string]any{
		"no installable": {"buildHost": map[string]any{"host": "builder"}},
		"not http":       {"published": map[string]any{"url": "file:///etc/passwd"}},
		"bad interval":   {"checkInterval": "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			cfg["updates"] = updates
			h := &harness{t: t, cfg: filepath.Join(t.TempDir(), "agent.json")}
			h.write(cfg)
			if _, err := agent.New(h.cfg); err == nil {
				t.Error("accepted")
			}
		})
	}
}
