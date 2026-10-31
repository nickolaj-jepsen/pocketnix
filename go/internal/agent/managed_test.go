package agent_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// managedGuest is a fakeGuest whose nix acts on a flake directory and a
// user profile the way the real one reports them.
type managedGuest struct {
	*fakeGuest
	flake, home string
}

const adhocProfile = ".local/state/nix/profiles/pocketnix-adhoc"

func newManagedGuest(t *testing.T) *managedGuest {
	t.Helper()
	g := &managedGuest{fakeGuest: newFakeGuest(t)}
	g.flake = filepath.Join(g.dir, "flake")
	g.home = filepath.Join(g.dir, "home")
	must(t, os.MkdirAll(g.flake, 0o755))
	must(t, os.MkdirAll(g.home, 0o755))
	must(t, os.WriteFile(filepath.Join(g.flake, "flake.nix"), []byte(`{
  inputs.pocketnix.url = "github:nickolaj-jepsen/pocketnix/v0.1.0";
  outputs = _: {};
}
`), 0o644))
	must(t, os.WriteFile(filepath.Join(g.flake, "pocketnix.json"), []byte(`{"user": "", "services": {}}`), 0o644))
	// A system's drv in the store, and `nix derivation show`'s view of it and
	// of what its build needs (Nix 2.34's shape: keys without the store).
	drv := filepath.Join(g.store, strings.Repeat("d", 32)+"-nixos-system-phone-26.05.2.drv")
	g.write(t, "eval.drv", drv)
	g.write(t, "show.json", fmt.Sprintf(`{"derivations":{
  %q: {"env": {}, "outputs": {"out": {"path": %q}}},
  "%s-portal-1.0.drv": {"env": {"src": "/nix/store/x-portal.tar.gz"}, "outputs": {"out": {"path": "p"}}},
  "%s-etc.drv": {"env": {"preferLocalBuild": "1", "src": "x"}, "outputs": {"out": {"path": "e"}}},
  "%s-unit-foo.service.drv": {"env": {"buildCommand": "x"}, "outputs": {"out": {"path": "u"}}},
  "%s-hello-x.drv": {"env": {"__json": "{\"src\": \"/nix/store/h.tar.gz\", \"preferLocalBuild\": false}"}, "outputs": {"out": {"path": "h"}}}
}}`, filepath.Base(drv), filepath.Base(g.systems[1]), strings.Repeat("p", 32), strings.Repeat("e", 32), strings.Repeat("u", 32), strings.Repeat("h", 32)))
	g.script(t, g.bin("nix"), fmt.Sprintf(`
dir=%[1]s
shift 2  # --extra-experimental-features "nix-command flakes"
case "$1" in
eval)
  echo "$*" >> $dir/evals
  case "$*" in
  *drvPath*) cat $dir/eval.drv ;;
  *mainProgram*) cat $dir/main 2>/dev/null || { echo "error: attribute 'mainProgram' missing" >&2; exit 1; } ;;
  esac ;;
derivation) cat $dir/show.json ;;
build)
  case "$*" in
  *--dry-run*) cat $dir/dryrun >&2 2>/dev/null || true ;;
  *) cat $dir/built ;;
  esac ;;
flake) [ "$2" = lock ] && echo locked > %[2]s/flake.lock ;;
profile)
  sub=$2; shift 2
  list=$dir/profile
  prev=
  for a; do [ "$prev" = --profile ] && { list=$dir/adhoc; [ "$sub" = add ] && touch "$a"; }; prev=$a; done
  touch $list
  case "$sub" in
  add)
    for a; do case "$a" in nixpkgs#*) echo "${a#nixpkgs#}" >> $list ;; /*) echo "$a" >> $list ;; esac; done ;;
  remove)
    for a; do grep -vxF "$a" $list > $list.new || true; mv $list.new $list; done ;;
  list)
    printf '{"elements":{'
    sep=
    while read -r e; do
      case "$e" in
      /*) printf '%%s"%%s":{"storePaths":["%%s"]}' "$sep" "$(basename "$e")" "$e" ;;
      *) printf '%%s"%%s":{"attrPath":"legacyPackages.aarch64-linux.%%s","originalUrl":"flake:nixpkgs","storePaths":[]}' "$sep" "$e" "$e" ;;
      esac
      sep=,
    done < $list
    printf '},"version":3}\n' ;;
  esac ;;
*) exit 2 ;;
esac
`, g.dir, g.flake))
	g.script(t, g.bin("nix-store"), fmt.Sprintf(`
dir=%[1]s
case "$1" in
--realise) echo "$2" > $dir/realised ;;
--gc)
  [ -e $dir/gc-fails ] && { echo "error: the store is locked" >&2; exit 1; }
  echo gc >> $dir/gc
  echo "deleting '/nix/store/oldgarbage-1'" >&2
  echo "1 store paths deleted, 1.0 MiB freed" ;;
*) exit 2 ;;
esac
`, g.dir))
	g.script(t, g.bin("nix-env"), fmt.Sprintf(`
p=$2
case "$3" in
--set)
  n=$(ls %[1]s | sed -n 's/^system-\([0-9]*\)-link$/\1/p' | sort -n | tail -n1)
  n=$((n + 1))
  ln -s "$4" "$p-$n-link" && ln -sfn "$p-$n-link" "$p" ;;
--switch-generation) ln -sfn "$p-$4-link" "$p" ;;
--delete-generations) shift 3; for n; do rm "$p-$n-link"; done ;;
*) exit 2 ;;
esac
`, filepath.Dir(g.profile)))
	// The managed systems switch and install their boot loader.
	for _, top := range g.systems {
		g.script(t, filepath.Join(top, "bin", "switch-to-configuration"), fmt.Sprintf(`
case "$1" in
switch) ln -sfn %[1]s %[2]s ;;
boot) echo %[1]s > %[3]s/boot ;;
*) exit 2 ;;
esac
`, top, g.current, g.dir))
	}
	return g
}

func (g *managedGuest) write(t *testing.T, name, s string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(g.dir, name), []byte(s+"\n"), 0o644))
}

func (g *managedGuest) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// startManaged serves a managed guest; the harness's server is returned too.
func startManaged(t *testing.T, g *managedGuest) (*client.Control, *harness) {
	t.Helper()
	cfg := baseConfig()
	cfg["hostname"] = "phone"
	cfg["updates"] = map[string]any{"keepGenerations": 0}
	cfg["managed"] = map[string]any{"flake": g.flake, "attr": "phone", "adhocProfile": adhocProfile}
	h := start(t, cfg, unixAddr)
	u := h.srv.Updates
	u.SSH, u.NixStore, u.Nix, u.NixEnv, u.SystemdRun = g.bin("ssh"), g.bin("nix-store"), g.bin("nix"), g.bin("nix-env"), ""
	u.Profile, u.Current, u.Booted, u.StoreDir = g.profile, g.current, g.booted(), g.store
	u.Inhibit = g.inhibit
	u.Home = g.home
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	return ctl, h
}

// dryRun is `nix build --dry-run`'s report, in the store at $store.
const dryRun = `these 4 derivations will be built:
  $store/pppppppppppppppppppppppppppppppp-portal-1.0.drv
  $store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-etc.drv
  $store/uuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu-unit-foo.service.drv
  $store/dddddddddddddddddddddddddddddddd-nixos-system-phone-26.05.2.drv
these 3 paths will be fetched (1.5 MiB download, 6.0 MiB unpacked):
  $store/ycj6knm38piypkxn00zjwjcdvnfd5xaj-cowsay-3.8.4
  $store/wj7phsmi7ncidl8k00p489krqss7n9sd-hello-2.12.3.tar.gz
  $store/yf1g1hjv24m2slww43c85ag2qdirczvq-version-check-hook`

func (g *managedGuest) dryRun(t *testing.T, report string) {
	t.Helper()
	g.write(t, "dryrun", strings.ReplaceAll(report, "$store", g.store))
}

func TestManagedConfigAnnounced(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	cfg := <-ctl.Configs()
	if cfg.Managed == nil || cfg.Managed.Flake != g.flake {
		t.Errorf("config.managed = %+v, want the flake %s", cfg.Managed, g.flake)
	}
	u, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(u.Sources, protocol.SourceLocal) {
		t.Errorf("sources %q lack the local flake", u.Sources)
	}
}

func TestManagedSelections(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	got, err := ctl.Selections(&protocol.Selections{User: "alice", Services: map[string]bool{"ssh": true, "gui": false}})
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "alice" || !got.Services["ssh"] || got.Services["gui"] || !got.Changed {
		t.Errorf("answered %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(g.flake, "pocketnix.json")); !strings.Contains(string(b), `"ssh": true`) {
		t.Errorf("pocketnix.json = %s", b)
	}
	// Reading, or writing the same, leaves it be.
	if got, err := ctl.Selections(nil); err != nil || got.User != "alice" || got.Changed {
		t.Errorf("read back %+v, %v", got, err)
	}
	if got, err := ctl.Selections(&protocol.Selections{User: "alice", Services: map[string]bool{"ssh": true}}); err != nil || got.Changed {
		t.Errorf("rewriting the same selections: %+v, %v", got, err)
	}
	for _, bad := range []protocol.Selections{
		{User: "Root;rm", Services: map[string]bool{}},
		{User: "alice", Services: map[string]bool{"telnet": true}},
	} {
		if _, err := ctl.Selections(&bad); err == nil {
			t.Errorf("selections %+v accepted", bad)
		}
	}
}

func TestManagedRebuild(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.dryRun(t, dryRun)

	u, err := ctl.Run(func() error { return ctl.UpdateCheckPin("0.2.0") })
	if err != nil {
		t.Fatalf("check: %v (log %q)", err, u.Log)
	}
	if u.Available != g.systems[1] || u.Source != protocol.SourceLocal {
		t.Fatalf("check found %q from %q, want %q from the flake", u.Available, u.Source, g.systems[1])
	}
	// Only the packages compile; the configuration's own derivations don't count.
	if u.Plan == nil || u.Plan.Download != 1572864 || !slices.Equal(u.Plan.Builds, []string{"portal-1.0"}) {
		t.Errorf("plan = %+v", u.Plan)
	}
	if evals := g.read(t, "evals"); !strings.Contains(evals, "--override-input pocketnix github:nickolaj-jepsen/pocketnix/v0.2.0") {
		t.Errorf("the check didn't evaluate the pinned release: %q", evals)
	}

	u, err = ctl.Run(func() error { return ctl.UpdateApplyWith("0.2.0", false) })
	var re *protocol.RemoteError
	if !errors.As(err, &re) || u.Blocked == nil || !slices.Equal(u.Blocked.Builds, []string{"portal-1.0"}) {
		t.Fatalf("apply without allowBuilds: err %v, blocked %+v", err, u.Blocked)
	}
	if u.Current != g.systems[0] {
		t.Fatalf("a blocked apply switched to %s", u.Current)
	}

	u, err = ctl.Run(func() error { return ctl.UpdateApplyWith("0.2.0", true) })
	if err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	if u.Current != g.systems[1] || u.Blocked != nil {
		t.Errorf("after apply: current %q, blocked %+v", u.Current, u.Blocked)
	}
	if evals := strings.Count(g.read(t, "evals"), "drvPath"); evals != 1 {
		t.Errorf("evaluated %d times; the apply should reuse the check", evals)
	}
	if b, _ := os.ReadFile(filepath.Join(g.flake, "flake.nix")); !strings.Contains(string(b), `pocketnix/v0.2.0"`) {
		t.Errorf("flake.nix isn't pinned to 0.2.0:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(g.flake, "flake.lock")); err != nil {
		t.Errorf("no lock file after the switch: %v", err)
	}
}

func TestManagedSwitchSucceedsWhenPruningFails(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.write(t, "dryrun", "")
	g.write(t, "gc-fails", "")
	// Generation 2 is neither booted nor running once the apply adds 3: the prune deletes it, then collects.
	old := filepath.Join(g.store, strings.Repeat("c", 32)+"-nixos-system-phone-26.05.0")
	must(t, os.MkdirAll(old, 0o755))
	must(t, os.Symlink(old, g.profile+"-2-link"))
	u, err := ctl.Run(func() error { return ctl.UpdateApplyWith("", false) })
	if err != nil || u.Error != "" {
		t.Fatalf("apply: %v %q (log %q)", err, u.Error, u.Log)
	}
	if u.Current != g.systems[1] {
		t.Errorf("current %q, want %q", u.Current, g.systems[1])
	}
}

func TestManagedApplyRechecksAChangedFlake(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.write(t, "dryrun", "")
	if u, err := ctl.Run(func() error { return ctl.UpdateCheckPin("") }); err != nil {
		t.Fatalf("check: %v (log %q)", err, u.Log)
	}
	if _, err := ctl.Selections(&protocol.Selections{User: "bob", Services: map[string]bool{}}); err != nil {
		t.Fatal(err)
	}
	if u, err := ctl.Run(func() error { return ctl.UpdateApplyWith("", false) }); err != nil {
		t.Fatalf("apply: %v (log %q)", err, u.Log)
	}
	if evals := strings.Count(g.read(t, "evals"), "drvPath"); evals != 2 {
		t.Errorf("evaluated %d times; changed selections need a new evaluation", evals)
	}
}

func TestManagedPackages(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.write(t, "dryrun", "this path will be fetched (12.5 KiB download, 46.9 KiB unpacked):\n  /nix/store/x-hello")
	u, err := ctl.Run(func() error { return ctl.PackageAdd([]string{"hello", "python3Packages.rich"}, false) })
	if err != nil {
		t.Fatalf("add: %v (log %q)", err, u.Log)
	}
	if !slices.Equal(u.Packages, []string{"hello", "python3Packages.rich"}) {
		t.Errorf("packages = %q", u.Packages)
	}
	// A fresh disk's sync adds what the app keeps; what's there stays once.
	if u, err := ctl.Run(func() error { return ctl.PackageAdd([]string{"hello"}, false) }); err != nil {
		t.Fatalf("adding again: %v (log %q)", err, u.Log)
	}
	if n := strings.Count(g.read(t, "profile"), "hello\n"); n != 1 {
		t.Errorf("the profile holds hello %d times", n)
	}

	// One that compiles waits for the user's yes.
	g.dryRun(t, "this derivation will be built:\n  $store/pppppppppppppppppppppppppppppppp-portal-1.0.drv")
	u, err = ctl.Run(func() error { return ctl.PackageAdd([]string{"portal"}, false) })
	if err == nil || u.Blocked == nil || slices.Contains(u.Packages, "portal") {
		t.Errorf("a compiling package was added without asking: err %v, blocked %+v, packages %q", err, u.Blocked, u.Packages)
	}

	u, err = ctl.Run(func() error { return ctl.PackageRemove([]string{"hello", "absent"}) })
	if err != nil {
		t.Fatalf("remove: %v (log %q)", err, u.Log)
	}
	if !slices.Equal(u.Packages, []string{"python3Packages.rich"}) {
		t.Errorf("after remove, packages = %q", u.Packages)
	}
	if err := ctl.PackageAdd([]string{"hello; rm -rf /"}, false); err == nil {
		t.Error("a package name with shell in it was accepted")
	}
}

func TestManagedRunApp(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.write(t, "dryrun", "")
	app := filepath.Join(g.store, strings.Repeat("g", 32)+"-gnome-calculator-48")
	must(t, os.MkdirAll(filepath.Join(app, "share", "applications"), 0o755))
	must(t, os.WriteFile(filepath.Join(app, "share", "applications", "org.gnome.Calculator.desktop"), nil, 0o644))
	g.write(t, "built", app)

	u, err := ctl.Run(func() error { return ctl.RunPackage("gnome-calculator", false) })
	if err != nil {
		t.Fatalf("run: %v (log %q)", err, u.Log)
	}
	if u.Run == nil || !slices.Equal(u.Run.Apps, []string{"org.gnome.Calculator"}) || u.Run.Command != "" {
		t.Errorf("run = %+v, want the calculator's desktop file", u.Run)
	}
	if adhoc := g.read(t, "adhoc"); !strings.Contains(adhoc, app) {
		t.Errorf("the ad-hoc profile doesn't hold %s: %q", app, adhoc)
	}
	// Running it again doesn't add it twice.
	if u, err := ctl.Run(func() error { return ctl.RunPackage("gnome-calculator", false) }); err != nil {
		t.Fatalf("run again: %v (log %q)", err, u.Log)
	}
	if n := strings.Count(g.read(t, "adhoc"), app); n != 1 {
		t.Errorf("the ad-hoc profile holds it %d times", n)
	}
}

func TestManagedRunProgram(t *testing.T) {
	g := newManagedGuest(t)
	ctl, _ := startManaged(t, g)
	g.write(t, "dryrun", "")
	tool := filepath.Join(g.store, strings.Repeat("r", 32)+"-ripgrep-15")
	must(t, os.MkdirAll(filepath.Join(tool, "bin"), 0o755))
	must(t, os.WriteFile(filepath.Join(tool, "bin", "rg"), nil, 0o755))
	g.write(t, "built", tool)
	g.write(t, "main", "rg")
	u, err := ctl.Run(func() error { return ctl.RunPackage("ripgrep", false) })
	if err != nil {
		t.Fatalf("run: %v (log %q)", err, u.Log)
	}
	if u.Run == nil || u.Run.Command != filepath.Join(tool, "bin", "rg") || len(u.Run.Apps) != 0 {
		t.Errorf("run = %+v, want its rg", u.Run)
	}
}

func TestPruneAfterBoot(t *testing.T) {
	g := newManagedGuest(t)
	// Generations 1 (booted), 2, 3 and 4 (running), and an ad-hoc profile.
	for i, c := range []string{"c", "e"} {
		top := filepath.Join(g.store, strings.Repeat(c, 32)+fmt.Sprintf("-nixos-system-phone-26.05.%d", i+3))
		must(t, os.MkdirAll(filepath.Join(top, "bin"), 0o755))
		g.script(t, filepath.Join(top, "bin", "switch-to-configuration"), fmt.Sprintf(`echo %s > %s/boot`, top, g.dir))
		g.systems = append(g.systems, top)
	}
	for n := 2; n <= 4; n++ {
		must(t, os.Symlink(g.systems[n-1], fmt.Sprintf("%s-%d-link", g.profile, n)))
	}
	must(t, os.Remove(g.current))
	must(t, os.Symlink(g.systems[3], g.current))
	profiles := filepath.Join(g.home, filepath.Dir(adhocProfile))
	must(t, os.MkdirAll(profiles, 0o755))
	for _, n := range []string{"pocketnix-adhoc", "pocketnix-adhoc-1-link", "profile"} {
		must(t, os.Symlink(g.systems[0], filepath.Join(profiles, n)))
	}

	ctl, h := startManaged(t, g)
	before, _ := ctl.Updates()
	h.srv.Updates.PruneAfterBoot(context.Background(), 0, "")
	u, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.Op > before.Op && u.State == protocol.UpdateIdle })
	if err != nil || u.Error != "" {
		t.Fatalf("prune: %v %s (log %q)", err, u.Error, u.Log)
	}
	var kept []int
	for _, gen := range u.Generations {
		kept = append(kept, gen.Number)
	}
	if !slices.Equal(kept, []int{4, 1}) {
		t.Errorf("kept generations %v, want the running 4 and the booted 1", kept)
	}
	if g.read(t, "gc") == "" {
		t.Error("no garbage collection")
	}
	// The collection's summary is logged; the paths it deleted aren't.
	if !slices.ContainsFunc(u.Log, func(l string) bool { return strings.HasSuffix(l, " freed") }) ||
		slices.ContainsFunc(u.Log, func(l string) bool { return strings.Contains(l, "oldgarbage") }) {
		t.Errorf("log %q: want the garbage collection's summary, without its deleted paths", u.Log)
	}
	if boot := strings.TrimSpace(g.read(t, "boot")); boot != g.systems[3] {
		t.Errorf("boot loader installed from %q, want the running system", boot)
	}
	left, _ := os.ReadDir(profiles)
	if len(left) != 1 || left[0].Name() != "profile" {
		t.Errorf("profiles left: %v; want only the user's own", left)
	}
}

func TestPruneAfterBootOncePerMarker(t *testing.T) {
	g := newManagedGuest(t)
	ctl, h := startManaged(t, g)
	marker := filepath.Join(t.TempDir(), "pruned")
	before, _ := ctl.Updates()
	h.srv.Updates.PruneAfterBoot(context.Background(), 0, marker)
	u, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.Op > before.Op && u.State == protocol.UpdateIdle })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("no marker after the prune: %v", err)
	}
	// An agent started again in the same boot finds the marker.
	h.srv.Updates.PruneAfterBoot(context.Background(), 0, marker)
	if again, _ := ctl.Updates(); again.Op != u.Op {
		t.Errorf("pruned again: op %d, then %d", u.Op, again.Op)
	}
}

// A cancel stops the transient unit a throttled command runs in: killing
// systemd-run's client alone leaves the command running.
func TestManagedCancelStopsTheCommandsUnit(t *testing.T) {
	g := newManagedGuest(t)
	g.script(t, g.bin("nix"), fmt.Sprintf("echo $$ > %s/nix.pid\nexec sleep 600\n", g.dir))
	// The unit's command outlives the client, as under systemd.
	g.script(t, g.bin("systemd-run"), fmt.Sprintf(`
for a; do
  shift
  case "$a" in --unit=*) unit=${a#--unit=} ;; --) break ;; esac
done
"$@" &
echo $! > %[1]s/unit-$unit
wait $!
`, g.dir))
	g.script(t, g.bin("systemctl"), fmt.Sprintf(`
[ "$1" = stop ] || exit 2
for unit; do :; done
pid=$(cat %[1]s/unit-$unit)
kill $pid
while kill -0 $pid 2>/dev/null; do sleep 0.1; done
`, g.dir))
	ctl, h := startManaged(t, g)
	h.srv.Updates.SystemdRun, h.srv.Updates.Systemctl = g.bin("systemd-run"), g.bin("systemctl")

	if err := ctl.UpdateCheck(); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.State == protocol.UpdateChecking }); err != nil {
		t.Fatal(err)
	}
	var pid int
	eventually(t, "the evaluation to start", func() bool {
		_, err := fmt.Sscan(g.read(t, "nix.pid"), &pid)
		return err == nil
	})
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if err := ctl.UpdateCancel(); err != nil {
		t.Fatal(err)
	}
	u, err := ctl.WaitUpdates(func(u protocol.Updates) bool { return u.State == protocol.UpdateIdle })
	if err != nil || u.Error != "cancelled" {
		t.Fatalf("after cancel: %q, %v", u.Error, err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the cancelled evaluation still runs (kill -0: %v)", err)
	}
}
