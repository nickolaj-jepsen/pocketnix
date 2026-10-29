package agent_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		top := filepath.Join(g.store, strings.Repeat(c, 32)+fmt.Sprintf("-nixos-system-phone-26.05.%d", i+1))
		must(t, os.MkdirAll(filepath.Join(top, "bin"), 0o755))
		must(t, os.MkdirAll(filepath.Join(top, "etc"), 0o755))
		must(t, os.WriteFile(filepath.Join(top, "etc", "hostname"), []byte("phone\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(top, "nixos-version"), fmt.Appendf(nil, "26.05.%d\n", i+1), 0o644))
		g.script(t, filepath.Join(top, "bin", "switch-to-configuration"), fmt.Sprintf(`
[ "$1" = switch ] || exit 2
[ -f %[3]s/inhibited ] || { echo "switching while the phone may sleep" >&2; exit 3; }
echo "activating %[1]s"
ln -sfn %[1]s %[2]s
`, top, g.current, dir))
		g.systems = append(g.systems, top)
	}
	must(t, os.Symlink(g.systems[0], g.profile+"-1-link"))
	must(t, os.Symlink(g.profile+"-1-link", g.profile))
	must(t, os.Symlink(g.systems[0], g.current))
	must(t, os.Symlink(g.systems[0], g.booted()))

	// ssh prints what $dir/answer says, or fails like an unreachable host.
	g.script(t, g.bin("ssh"), fmt.Sprintf(`
case "$*" in *BatchMode=yes*) ;; *) echo "no BatchMode" >&2; exit 2 ;; esac
[ -f %[1]s/hang ] && exec sleep 600
[ -f %[1]s/answer ] || { echo "ssh: connect to host builder port 22: No route to host" >&2; exit 255; }
echo "evaluating on the build host" >&2
cat %[1]s/answer
`, dir))
	// The guest's caches have a path when $dir/cached lists it.
	g.script(t, g.bin("nix-store"), fmt.Sprintf(`
[ "$1" = --realise ] || exit 2
[ -f %[1]s/inhibited ] || { echo "fetching while the phone may sleep" >&2; exit 3; }
case "$*" in *"--max-jobs 0"*) ;; *) echo "realise may build" >&2; exit 2 ;; esac
for p; do :; done
grep -qxF "$p" %[1]s/cached 2>/dev/null || { echo "error: don't know how to build these paths: $p" >&2; exit 1; }
echo "copying path '$p' from 'https://cache.example'" >&2
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

// inhibit stands in for logind: $dir/inhibited exists while held, and the
// fake fetch and switch fail without it.
func (g *fakeGuest) inhibit(why string) (io.Closer, error) {
	f := filepath.Join(g.dir, "inhibited")
	if err := os.WriteFile(f, []byte(why), 0o644); err != nil {
		return nil, err
	}
	return closeFunc(func() error { return os.Remove(f) }), nil
}

func (g *fakeGuest) inhibited() bool {
	_, err := os.Stat(filepath.Join(g.dir, "inhibited"))
	return err == nil
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

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

// startUpdates serves updates (the config's "updates" object) on a fake
// guest named "phone"; tls, when set, is the published path's server.
func startUpdates(t *testing.T, g *fakeGuest, updates map[string]any, tls ...*httptest.Server) *client.Control {
	t.Helper()
	cfg := baseConfig()
	cfg["hostname"] = "phone"
	cfg["updates"] = updates
	h := start(t, cfg, unixAddr)
	u := h.srv.Updates
	u.SSH, u.NixStore, u.Nix, u.NixEnv, u.SystemdRun = g.bin("ssh"), g.bin("nix-store"), g.bin("nix"), g.bin("nix-env"), ""
	u.Profile, u.Current, u.Booted, u.StoreDir = g.profile, g.current, g.booted(), g.store
	u.Inhibit = g.inhibit
	if len(tls) > 0 {
		u.HTTP = tls[0].Client()
	}
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
	if g.inhibited() {
		t.Error("the update still keeps the phone awake")
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, g.systems[1])
	}))
	defer srv.Close()
	g.cache(t, g.systems[1])
	ctl := startUpdates(t, g, map[string]any{"published": map[string]any{"url": srv.URL + "/phone"}}, srv)
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, g.systems[1])
	}))
	defer srv.Close()
	// No answer file: the build host is unreachable.
	ctl := startUpdates(t, g, map[string]any{"buildHost": buildHost["buildHost"], "published": map[string]any{"url": srv.URL}}, srv)
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
	// Another machine's system, or a derivation to build, isn't this host's toplevel.
	for _, other := range []string{"-nixos-system-desktop-26.05.1", "-nixos-system-phone-26.05.drv-x.drv", "-hello-2.12",
		"-nixos-system-phone-"} {
		g.answer(t, filepath.Join(g.store, strings.Repeat("c", 32)+other))
		if u, err := ctl.Run(ctl.UpdateApply); err == nil || !strings.Contains(u.Error, "toplevel") {
			t.Errorf("%s: err %v, state error %q", other, err, u.Error)
		}
	}
	if currentGeneration(u) != 1 {
		t.Errorf("switched anyway: %+v", u.Generations)
	}
}

// A store path that isn't a NixOS system never becomes a generation.
func TestUpdateRejectsUnswitchablePath(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	bogus := filepath.Join(g.store, strings.Repeat("c", 32)+"-nixos-system-phone-26.05.3")
	must(t, os.MkdirAll(bogus, 0o755))
	g.cache(t, bogus)
	g.answer(t, bogus)
	u, err := ctl.Run(ctl.UpdateApply)
	if err == nil || !strings.Contains(u.Error, "not a NixOS system") {
		t.Fatalf("err %v, state error %q", err, u.Error)
	}
	if len(u.Generations) != 1 {
		t.Errorf("became a generation: %+v", u.Generations)
	}
}

// Host "phone" can't tell "phone-vm"'s toplevel by its name; its etc/hostname
// tells. A custom label is fine.
func TestUpdateRejectsAnotherHostsSystem(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	for host, label := range map[string]string{"phone-vm": "vm-26.05.3", "phone": "main-abc123"} {
		top := filepath.Join(g.store, strings.Repeat("d", 32)+"-nixos-system-phone-"+label)
		must(t, os.MkdirAll(filepath.Join(top, "etc"), 0o755))
		must(t, os.MkdirAll(filepath.Join(top, "bin"), 0o755))
		must(t, os.WriteFile(filepath.Join(top, "etc", "hostname"), []byte(host+"\n"), 0o644))
		g.script(t, filepath.Join(top, "bin", "switch-to-configuration"), fmt.Sprintf("ln -sfn %s %s\n", top, g.current))
		g.cache(t, top)
		g.answer(t, top)
		u, err := ctl.Run(ctl.UpdateApply)
		if host == "phone" {
			if err != nil || u.Current != top {
				t.Errorf("label %s: err %v, current %q", label, err, u.Current)
			}
		} else if err == nil || !strings.Contains(u.Error, "toplevel") || u.Current == top {
			t.Errorf("%s's system: err %v, state error %q, current %q", host, err, u.Error, u.Current)
		}
	}
}

// Whoever answers the published URL picks the system: a redirect mustn't
// drop to plain http.
func TestPublishedRefusesHTTPRedirect(t *testing.T) {
	g := newFakeGuest(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://ci.example/phone", http.StatusFound)
	}))
	defer srv.Close()
	ctl := startUpdates(t, g, map[string]any{"published": map[string]any{"url": srv.URL + "/phone"}}, srv)
	u, err := ctl.Run(ctl.UpdateCheck)
	if err == nil || !strings.Contains(u.Error, "refusing a redirect") {
		t.Fatalf("err %v, state error %q", err, u.Error)
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
	if g.inhibited() {
		t.Error("the rollback still keeps the phone awake")
	}
	if err := ctl.SwitchGeneration(7); err == nil {
		t.Error("switched to a generation that doesn't exist")
	}
}

// A build host that hangs mid-build mustn't hold the updater: a cancel ends
// the check, and a rollback doesn't wait for it.
func TestHungBuildHost(t *testing.T) {
	g := newFakeGuest(t)
	ctl := startUpdates(t, g, buildHost)
	must(t, os.WriteFile(filepath.Join(g.dir, "hang"), nil, 0o644))
	hung := func() {
		t.Helper()
		if err := ctl.UpdateCheck(); err != nil {
			t.Fatal(err)
		}
		if _, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.State == protocol.UpdateChecking }); err != nil {
			t.Fatal(err)
		}
	}

	hung()
	start := time.Now()
	if err := ctl.UpdateCancel(); err != nil {
		t.Fatal(err)
	}
	u, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.State == protocol.UpdateIdle })
	if err != nil || u.Error != "cancelled" || time.Since(start) > 10*time.Second {
		t.Fatalf("after cancel: %+v, %v, %v", u.Error, err, time.Since(start))
	}
	if err := ctl.UpdateCancel(); err == nil {
		t.Error("cancelled while idle")
	}

	hung()
	u, err = ctl.Run(func() error {
		// Answered at once: the app gives up on a slow request.
		start := time.Now()
		err := ctl.SwitchGeneration(1)
		if d := time.Since(start); d > time.Second {
			t.Errorf("switch-generation answered after %v", d)
		}
		return err
	})
	if err != nil || currentGeneration(u) != 1 {
		t.Fatalf("rollback during a hung check: %v, %+v", err, u)
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
		"plain http":     {"published": map[string]any{"url": "http://ci.example/phone"}},
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
