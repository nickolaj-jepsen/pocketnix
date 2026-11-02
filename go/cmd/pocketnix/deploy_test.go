package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// deployHost never resolves, should a call miss the fakes.
const deployHost = "pocketnix-test.invalid"

// The desktop's side of the model: ssh reaches only the model guest, and nix
// builds nothing but answers a store path per attribute and copies into the
// model guest's store the way a root nix-daemon over ssh would.
const fakeDesktopSSH = `#!/bin/sh
cfg=
while [ $# -gt 0 ]; do
  case $1 in
    -F) cfg=$2; shift 2 ;;
    -o|-p|-i|-l|-S) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
host=$1; shift
if [ "$host" != "$FAKE_GUEST_HOST" ] || { [ -n "$FAKE_SSH_CONFIG" ] && [ "$cfg" != "$FAKE_SSH_CONFIG" ]; }; then
  echo "ssh: Could not resolve hostname $host" >&2
  exit 255
fi
input=$(cat)
# A dropped connection takes the script that changes the unit along, and the
# guest finishes it after pending-after more calls, or with no such file, never.
if [ -e "$FAKE_GUEST/pending-after" ] && [ -e "$FAKE_GUEST/pending-script" ]; then
  n=$(cat "$FAKE_GUEST/pending-after")
  if [ "$n" -le 0 ]; then
    PATH="$FAKE_GUEST_BIN:$PATH" FAKE_UID=1000 sh "$FAKE_GUEST/pending-script"
    rm -f "$FAKE_GUEST/pending-script" "$FAKE_GUEST/pending-after"
  else
    echo $((n - 1)) > "$FAKE_GUEST/pending-after"
  fi
fi
case $input in
*daemon-reload*)
  if [ -e "$FAKE_GUEST/drop-script" ]; then
    rm -f "$FAKE_GUEST/drop-script"
    printf '%s\n' "$input" > "$FAKE_GUEST/pending-script"
    echo "client_loop: send disconnect: Broken pipe" >&2
    exit 255
  fi
  ;;
esac
printf '%s\n' "$input" | PATH="$FAKE_GUEST_BIN:$PATH" FAKE_UID=1000 sh -c "$*"
status=$?
# Restarting the agent cuts the session it rides.
if [ -e "$FAKE_GUEST/cut" ]; then
  rm -f "$FAKE_GUEST/cut"
  echo "Connection to $host closed by remote host." >&2
  exit 255
fi
exit $status
`

const fakeDesktopNix = `#!/bin/sh
# A store path's closure, one "path size" line each: canned, or the path alone.
closure() {
  if [ -e "$FAKE_DESKTOP/closures/${1##*/}" ]; then
    cat "$FAKE_DESKTOP/closures/${1##*/}"
  else
    echo "$1 1048576"
  fi
}
case $1 in
build)
  case " $* " in
  *" --expr "*)
    # nix/deploy-rewrite.nix: the cross package's binaries, rewritten.
    out=/nix/store/fakehash-pocketnix-compositor-guest-libs
    mkdir -p "$FAKE_DESKTOP$out/bin"
    touch "$FAKE_DESKTOP$out/bin/pocketnix-compositor" "$FAKE_DESKTOP$out/bin/pocketnix-gui-bench"
    echo "$out"
    exit 0
    ;;
  esac
  for a; do inst=$a; done
  echo "/nix/store/fakehash-${inst#*#}"
  ;;
path-info)
  for a; do p=$a; done
  closure "$p"
  ;;
copy)
  shift
  sigs=yes url= path=
  while [ $# -gt 0 ]; do
    case $1 in
      --no-check-sigs) sigs=no; shift ;;
      --to) url=$2; shift 2 ;;
      *) path=$1; shift ;;
    esac
  done
  case $url in
    ssh-ng://*) ;;
    *) echo "fake nix: store $url not modelled" >&2; exit 97 ;;
  esac
  rest=${url#ssh-ng://}
  host=${rest%%\?*}
  prog=$(printf %s "${rest#*remote-program=}" | sed 's/%20/ /g')
  # The guest trusts only root, and a locally built path has no signature.
  case "$sigs $prog" in
    "no sudo -n "*) ;;
    *) echo "error: cannot add path '$path' because it lacks a signature by a trusted key" >&2; exit 1 ;;
  esac
  # Nix splits NIX_SSHOPTS as a shell would; sudo -n true stands in for the daemon.
  eval "set -- $NIX_SSHOPTS"
  ssh "$@" "$host" "sudo -n true" || exit 1
  closure "$path" | while read -r p size; do
    mkdir -p "$FAKE_GUEST$p"
    [ ! -d "$FAKE_DESKTOP$p" ] || cp -R "$FAKE_DESKTOP$p/." "$FAKE_GUEST$p/"
  done
  ;;
*)
  echo "fake nix: $1 not modelled" >&2
  exit 97
  ;;
esac
`

// The guest's side: sudo with a knob for a password, nix-store's roots, and
// systemd's fragments, runtime drop-ins, what daemon-reload loaded and what a
// restart started. A unit's "fate" file (fakeGuest.unitFate) says how its next
// restart turns out, as each later look at it finds it:
//   - failed: never active.
//   - dies: active once, then failed, with no new invocation (no Restart=).
//   - crash-loop: active, but every look after the first finds a new invocation.
//   - crashed-early: active and steady, its restarts already counted at the first look.
const fakeGuestSudo = `#!/bin/sh
[ "$1" = -n ] || { echo "fake sudo: would prompt" >&2; exit 1; }
shift
if [ -e "$FAKE_GUEST/sudo-needs-password" ]; then
  echo "sudo: a password is required" >&2
  exit 1
fi
FAKE_UID=0 exec "$@"
`

const fakeGuestNixStore = `#!/bin/sh
[ ! -e "$FAKE_GUEST/nix-store-fails" ] || { echo "error: disk full" >&2; exit 1; }
if [ "$1" = --query ] && [ "$2" = --requisites ]; then
  [ -e "$FAKE_GUEST$3" ] || { echo "error: path '$3' is not valid" >&2; exit 1; }
  cat "$FAKE_GUEST/closures/${3##*/}" 2>/dev/null || echo "$3"
  exit 0
fi
[ "$FAKE_UID" = 0 ] || { echo "nix-store: permission denied" >&2; exit 1; }
path= root=
while [ $# -gt 0 ]; do
  case $1 in
    --realise|-r) shift ;;
    --add-root) root=$2; shift 2 ;;
    *) path=$1; shift ;;
  esac
done
[ -e "$FAKE_GUEST$path" ] || { echo "error: path '$path' is not valid" >&2; exit 1; }
ln -sfn "$path" "$root" && echo "$root"
`

// fakeGuestLoader is the model guest's dynamic loader: --list resolves a binary,
// unless the guest's libraries lack a symbol version it needs.
const fakeGuestLoader = `#!/bin/sh
[ "$1" = --list ] && [ -e "$2" ] || { echo "$2: cannot open shared object file" >&2; exit 127; }
if [ -e "$FAKE_GUEST/loader-lacks-version" ]; then
  echo "$2: version \` + "`GLIBC_2.99'" + ` not found (required by $2)" >&2
fi
echo "	libc.so.6 => (resolved)"
`

const fakeGuestSystemctl = `#!/bin/sh
G=$FAKE_GUEST
scope=system
[ "$1" = --user ] && { scope=user; shift; }
[ "$1" = --no-block ] && shift
if [ $scope = system ]; then
  frags=$G/etc/systemd/system dropins=$G/run/systemd/system
else
  frags=$G/etc/systemd/user dropins=$G/run/user/$(id -u)/systemd/user
fi
needroot() { [ $scope = user ] || [ "$FAKE_UID" = 0 ] || { echo "Access denied" >&2; exit 1; }; }
# The last ExecStart across the fragment and its drop-ins in order; an empty one resets.
effective() {
  v=
  for f in "$frags/$1" "$dropins/$1.d/"*.conf; do
    [ -e "$f" ] || continue
    while IFS= read -r line; do
      case $line in ExecStart=*) v=${line#ExecStart=} ;; esac
    done < "$f"
  done
  printf '%s\n' "$v"
}
s=$G/state/$scope
mkdir -p "$s"
cmd=$1; shift
case $cmd in
show)
  prop=$2 unit=$3
  [ -e "$frags/$unit" ] || { echo; exit 0; }
  case $prop in
    FragmentPath) echo "$frags/$unit" ;;
    ExecStart)
      # While it reloads, systemd can answer with nothing at all.
      if [ -e "$G/blank-execstart" ]; then
        rm -f "$G/blank-execstart"
        echo
        exit 0
      fi
      loaded=$(cat "$s/$unit.loaded")
      echo "{ path=${loaded%% *} ; argv[]=$loaded ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
      ;;
    InvocationID)
      n=$(cat "$s/$unit.invocation" 2>/dev/null || echo 0)
      if [ "$(cat "$s/$unit.now" 2>/dev/null)" = crash-loop ]; then
        if [ -e "$s/$unit.looked" ]; then
          n=$((n + 1))
          echo $n > "$s/$unit.invocation"
          echo $(($(cat "$s/$unit.restarts") + 1)) > "$s/$unit.restarts"
        fi
        touch "$s/$unit.looked"
      fi
      echo $n
      ;;
    ActiveState)
      case $(cat "$s/$unit.now" 2>/dev/null) in
        failed) echo failed ;;
        dies)
          if [ -e "$s/$unit.looked" ]; then echo failed; else touch "$s/$unit.looked"; echo active; fi
          ;;
        *) echo active ;;
      esac
      ;;
    NRestarts)
      if [ -e "$G/no-nrestarts" ]; then echo; else cat "$s/$unit.restarts" 2>/dev/null || echo 0; fi
      ;;
    *) echo "fake systemctl: property $prop not modelled" >&2; exit 1 ;;
  esac
  ;;
daemon-reload)
  needroot
  for f in "$frags"/*; do effective "${f##*/}" > "$s/${f##*/}.loaded"; done
  ;;
restart)
  needroot
  unit=$1
  n=$(cat "$s/$unit.invocation" 2>/dev/null || echo 0)
  echo $((n + 1)) > "$s/$unit.invocation"
  cp "$s/$unit.loaded" "$s/$unit.running"
  rm -f "$s/$unit.now" "$s/$unit.looked"
  # A manual restart flushes the unit's restart counter.
  echo 0 > "$s/$unit.restarts"
  if [ -e "$s/$unit.fate" ]; then
    cp "$s/$unit.fate" "$s/$unit.now"
    [ "$(cat "$s/$unit.fate")" != crashed-early ] || echo 2 > "$s/$unit.restarts"
  fi
  [ "$unit" != pocketnix-agent.service ] || touch "$G/cut"
  ;;
*)
  echo "fake systemctl: $cmd not modelled" >&2
  exit 1
  ;;
esac
`

// systemAgent is the model guest's own agent, as its unit file runs it.
const systemAgent = "/nix/store/system-pocketnix-0.1.0/bin/pocketnix-agent --guest-socket /run/pocketnix/guest.sock --listen vsock:9100"

// fakeGuest is a model of a guest, and of the desktop's ssh and nix that reach
// it, in a directory: the tests assert through its files.
type fakeGuest struct {
	t *testing.T
	// dir is the guest's root; desktop holds the desktop's canned closures and
	// the store paths that have files.
	dir, desktop string
}

func newFakeGuest(t *testing.T) *fakeGuest {
	t.Helper()
	base := t.TempDir()
	g := &fakeGuest{t: t, dir: filepath.Join(base, "guest"), desktop: filepath.Join(base, "desktop")}
	desktopBin, guestBin := filepath.Join(base, "desktop-bin"), filepath.Join(base, "guest-bin")
	for path, script := range map[string]string{
		filepath.Join(desktopBin, "ssh"):     fakeDesktopSSH,
		filepath.Join(desktopBin, "nix"):     fakeDesktopNix,
		filepath.Join(guestBin, "sudo"):      fakeGuestSudo,
		filepath.Join(guestBin, "nix-store"): fakeGuestNixStore,
		filepath.Join(guestBin, "systemctl"): fakeGuestSystemctl,
	} {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.write("etc/systemd/system/pocketnix-agent.service", "[Service]\nExecStart="+systemAgent+"\n")
	// What the guest's systemd loaded at boot.
	g.write("state/system/pocketnix-agent.service.loaded", systemAgent+"\n")
	t.Setenv("PATH", desktopBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GUEST", g.dir)
	t.Setenv("FAKE_DESKTOP", g.desktop)
	t.Setenv("FAKE_GUEST_BIN", guestBin)
	t.Setenv("FAKE_GUEST_HOST", deployHost)
	t.Setenv("FAKE_SSH_CONFIG", "")
	t.Setenv("NIX_SSHOPTS", "")
	old, oldELF := deployGuestRoot, deployReadELF
	oldReconnect, oldRetry, oldSettle := deployReconnect, deployRetry, deploySettle
	deployGuestRoot = g.dir
	deployReadELF = func(string) (elfNeeds, error) { return crossCompositorNeeds, nil }
	deployRetry, deploySettle = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		deployGuestRoot, deployReadELF = old, oldELF
		deployReconnect, deployRetry, deploySettle = oldReconnect, oldRetry, oldSettle
	})
	return g
}

// unitFate says how the unit's next restart turns out: see fakeGuestSystemctl.
func (g *fakeGuest) unitFate(scope, unit, fate string) {
	g.t.Helper()
	g.write("state/"+scope+"/"+unit+".fate", fate)
}

// crossCompositorNeeds is what the cross compositor's binaries load, as its
// build links them.
var crossCompositorNeeds = elfNeeds{
	bins:   []string{"pocketnix-compositor", "pocketnix-gui-bench"},
	interp: "ld-linux-aarch64.so.1",
	needed: []string{"libxkbcommon.so.0", "libgcc_s.so.1", "libm.so.6", "libc.so.6", "ld-linux-aarch64.so.1"},
}

// The model guest's system compositor, and the store paths of its closure.
const (
	guestCompositorPkg = "/nix/store/0000000000000000000000000000comp-pocketnix-compositor-0.1.0"
	guestGlibc         = "/nix/store/0000000000000000000000000000libc-glibc-2.42-100"
	guestXkbcommon     = "/nix/store/00000000000000000000000000000xkb-libxkbcommon-1.13.1"
	guestGccLib        = "/nix/store/00000000000000000000000000gcclib-gcc-15.3.0-lib"
	systemCompositor   = guestCompositorPkg + "/bin/pocketnix-compositor --guest-socket /run/pocketnix/guest.sock"
)

// guestCompositorClosures are canned closures of the system compositor: each
// store path with the files in its lib directory.
var guestCompositorClosures = map[string][]guestLibDir{
	"matching": {
		{guestCompositorPkg, nil},
		{guestXkbcommon, []string{"libxkbcommon.so", "libxkbcommon.so.0", "libxkbcommon.so.0.13.1"}},
		{guestGlibc, []string{"ld-linux-aarch64.so.1", "libc.so.6", "libm.so.6", "libpthread.so.0"}},
		{guestGccLib, []string{"libgcc_s.so.1", "libstdc++.so.6"}},
	},
	// The guest's libxkbcommon has another soname than the one the cross build linked.
	"missing-soname": {
		{guestCompositorPkg, nil},
		{guestXkbcommon, []string{"libxkbcommon.so.1"}},
		{guestGlibc, []string{"ld-linux-aarch64.so.1", "libc.so.6", "libm.so.6"}},
		{guestGccLib, []string{"libgcc_s.so.1"}},
	},
}

type guestLibDir struct {
	path  string
	files []string
}

// withCompositor gives the guest a system compositor unit whose package has the
// named canned closure.
func (g *fakeGuest) withCompositor(closure string) {
	g.t.Helper()
	g.write("etc/systemd/user/pocketnix-compositor.service", "[Service]\nExecStart="+systemCompositor+"\n")
	g.write("state/user/pocketnix-compositor.service.loaded", systemCompositor+"\n")
	var paths []string
	for _, d := range guestCompositorClosures[closure] {
		paths = append(paths, d.path)
		os.MkdirAll(g.path(d.path), 0o755)
		for _, f := range d.files {
			content, mode := "", os.FileMode(0o644)
			if f == "ld-linux-aarch64.so.1" {
				content, mode = fakeGuestLoader, 0o755
			}
			g.write(d.path+"/lib/"+f, content)
			os.Chmod(g.path(d.path+"/lib/"+f), mode)
		}
	}
	g.write("closures/"+path.Base(guestCompositorPkg), strings.Join(paths, "\n")+"\n")
}

// desktopClosure cans the desktop's closure of a store path, as "path size" lines.
func (g *fakeGuest) desktopClosure(p string, lines ...string) {
	g.t.Helper()
	os.MkdirAll(filepath.Join(g.desktop, "closures"), 0o755)
	if err := os.WriteFile(filepath.Join(g.desktop, "closures", path.Base(p)), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *fakeGuest) path(rel string) string { return filepath.Join(g.dir, rel) }

func (g *fakeGuest) write(rel, content string) {
	g.t.Helper()
	os.MkdirAll(filepath.Dir(g.path(rel)), 0o755)
	if err := os.WriteFile(g.path(rel), []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *fakeGuest) exists(rel string) bool {
	_, err := os.Lstat(g.path(rel))
	return err == nil
}

// running is the command line the agent unit's last restart started.
func (g *fakeGuest) running() string {
	b, _ := os.ReadFile(g.path("state/system/pocketnix-agent.service.running"))
	return strings.TrimSpace(string(b))
}

// compositorRunning is the command line the compositor unit's last restart started.
func (g *fakeGuest) compositorRunning() string {
	b, _ := os.ReadFile(g.path("state/user/pocketnix-compositor.service.running"))
	return strings.TrimSpace(string(b))
}

const (
	agentDropIn = "run/systemd/system/pocketnix-agent.service.d/pocketnix-deploy.conf"
	agentRoot   = "run/pocketnix-deploy/agent"
	// The compositor's drop-in, under the user's runtime directory.
	compositorDropIn   = "pocketnix-compositor.service.d/pocketnix-deploy.conf"
	compositorRoot     = "run/pocketnix-deploy/compositor"
	compositorLibsRoot = "run/pocketnix-deploy/compositor-libs"
)

func userDropIn(rel string) string {
	return fmt.Sprintf("run/user/%d/systemd/user/%s", os.Getuid(), rel)
}

// stderrOf runs f and returns what it wrote to os.Stderr.
func stderrOf(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan []byte)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.Bytes()
	}()
	ferr := f()
	os.Stderr = old
	w.Close()
	out := <-done
	r.Close()
	return string(out), ferr
}

// crossAgent is the store path the fake nix answers for the cross agent.
const crossAgent = "/nix/store/fakehash-pocketnix-cross-aarch64"

var deployAgent = []string{"--host", deployHost, "--flake", "/fake/flake", "agent"}

func TestDeployAgentAndRemove(t *testing.T) {
	t.Run("leaves a drop-in and a root", func(t *testing.T) {
		g := newFakeGuest(t)
		if err := runDeploy(deployAgent); err != nil {
			t.Fatal(err)
		}
		if !g.exists(agentDropIn) {
			t.Fatalf("no drop-in at %s", agentDropIn)
		}
		if root, err := os.Readlink(g.path(agentRoot)); err != nil || root != crossAgent {
			t.Errorf("GC root = %q, %v; want %s", root, err, crossAgent)
		}
		want := crossAgent + "/bin/pocketnix-agent --guest-socket /run/pocketnix/guest.sock --listen vsock:9100"
		if got := g.running(); got != want {
			t.Errorf("the agent runs %q, want %q", got, want)
		}
	})

	t.Run("warns about a leftover drop-in", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write(agentDropIn, "[Service]\nExecStart=\nExecStart=/nix/store/old-pocketnix/bin/pocketnix-agent\n")
		out, err := stderrOf(t, func() error { return runDeploy(deployAgent) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, g.path(agentDropIn)) {
			t.Errorf("no warning naming the leftover %s:\n%s", agentDropIn, out)
		}
	})

	t.Run("remove leaves neither", func(t *testing.T) {
		g := newFakeGuest(t)
		if err := runDeploy(deployAgent); err != nil {
			t.Fatal(err)
		}
		// Without a component named, --remove ends both.
		if err := runDeploy([]string{"--host", deployHost, "--remove"}); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{agentDropIn, agentRoot} {
			if g.exists(rel) {
				t.Errorf("%s survived --remove", rel)
			}
		}
		if got := g.running(); got != systemAgent {
			t.Errorf("after --remove the agent runs %q, want the system's %q", got, systemAgent)
		}
	})

	t.Run("refuses without passwordless sudo before any copy", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write("sudo-needs-password", "")
		err := runDeploy(deployAgent)
		if err == nil || !strings.Contains(err.Error(), "sudo") {
			t.Fatalf("err = %v, want a refusal naming sudo", err)
		}
		for _, rel := range []string{"nix/store", agentDropIn, agentRoot} {
			if g.exists(rel) {
				t.Errorf("a refused deploy left %s", rel)
			}
		}
	})

	t.Run("an ssh config reaches ssh and nix copy", func(t *testing.T) {
		g := newFakeGuest(t)
		cfg := filepath.Join(t.TempDir(), "ssh config")
		t.Setenv("FAKE_SSH_CONFIG", cfg)
		if err := runDeploy(append([]string{"--ssh-config", cfg}, deployAgent...)); err != nil {
			t.Fatal(err)
		}
		if !g.exists(crossAgent) {
			t.Error("the agent never reached the guest's store")
		}
	})

	t.Run("native deploys the flake's own package", func(t *testing.T) {
		g := newFakeGuest(t)
		if err := runDeploy(append([]string{"--native"}, deployAgent...)); err != nil {
			t.Fatal(err)
		}
		want := "/nix/store/fakehash-packages.aarch64-linux.pocketnix/bin/pocketnix-agent --guest-socket /run/pocketnix/guest.sock --listen vsock:9100"
		if got := g.running(); got != want {
			t.Errorf("the agent runs %q, want %q", got, want)
		}
	})
}

// One look at an active unit proves little: a binary that dies at start looks
// fine until its first crash.
func TestDeployNoticesAUnitThatDoesntStayUp(t *testing.T) {
	const (
		agentUnit      = "pocketnix-agent.service"
		compositorUnit = "pocketnix-compositor.service"
	)
	deployCompositor := []string{"--host", deployHost, "--flake", "/fake/flake", "--native", "compositor"}
	for _, tc := range []struct {
		name, scope, unit, fate string
		args                    []string
		noNRestarts             bool
		// want is the error's token.
		want string
	}{
		{name: "crash-loop", scope: "system", unit: agentUnit, fate: "crash-loop", args: deployAgent, want: "stay up"},
		{name: "crash-loop-without-nrestarts", scope: "system", unit: agentUnit, fate: "crash-loop", args: deployAgent, noNRestarts: true, want: "stay up"},
		{name: "crashed-before-the-first-look", scope: "system", unit: agentUnit, fate: "crashed-early", args: deployAgent, want: "NRestarts"},
		{name: "dies-without-a-restart", scope: "user", unit: compositorUnit, fate: "dies", args: deployCompositor, want: "stay up"},
		{name: "never-comes-back", scope: "system", unit: agentUnit, fate: "failed", args: deployAgent, want: "come back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGuest(t)
			g.withCompositor("matching")
			deployReconnect = 200 * time.Millisecond
			g.unitFate(tc.scope, tc.unit, tc.fate)
			if tc.noNRestarts {
				g.write("no-nrestarts", "")
			}
			err := runDeploy(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a failure naming %q", err, tc.want)
			}
		})
	}

	t.Run("remove", func(t *testing.T) {
		g := newFakeGuest(t)
		if err := runDeploy(deployAgent); err != nil {
			t.Fatal(err)
		}
		g.unitFate("system", agentUnit, "crash-loop")
		err := runDeploy([]string{"--host", deployHost, "--remove", "agent"})
		if err == nil || !strings.Contains(err.Error(), "stay up") {
			t.Errorf("err = %v, want a failure naming %q", err, "stay up")
		}
	})
}

// A healthy unit passes whatever the restart counter can tell: it isn't always
// readable, and its value from before the deploy says nothing about this binary.
func TestDeployTrustsAUnitThatStaysUp(t *testing.T) {
	t.Run("without-nrestarts", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write("no-nrestarts", "")
		if err := runDeploy(deployAgent); err != nil {
			t.Error(err)
		}
	})
	t.Run("after-earlier-crashes", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write("state/system/pocketnix-agent.service.restarts", "3\n")
		if err := runDeploy(deployAgent); err != nil {
			t.Error(err)
		}
	})
}

// A timeout or a dropped ssh says nothing about how far the script got: an old
// ExecStart may still change.
func TestDeployOverALostConnection(t *testing.T) {
	t.Run("the guest finishes the script later", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write("drop-script", "")
		g.write("pending-after", "2\n")
		g.write("blank-execstart", "")
		if err := runDeploy(deployAgent); err != nil {
			t.Fatal(err)
		}
		if got := g.running(); !strings.HasPrefix(got, crossAgent+"/bin/pocketnix-agent") {
			t.Errorf("the agent runs %q, want %s's", got, crossAgent)
		}
	})

	t.Run("the guest never does", func(t *testing.T) {
		g := newFakeGuest(t)
		deployReconnect = 200 * time.Millisecond
		g.write("drop-script", "")
		err := runDeploy(deployAgent)
		if err == nil || !strings.Contains(err.Error(), "come back") || !strings.Contains(err.Error(), "may still") {
			t.Errorf("err = %v, want the deadline's failure saying the guest may still apply it", err)
		}
		if got := g.running(); got != "" {
			t.Errorf("the agent was restarted, into %q", got)
		}
	})

	t.Run("remove", func(t *testing.T) {
		g := newFakeGuest(t)
		if err := runDeploy(deployAgent); err != nil {
			t.Fatal(err)
		}
		g.write("drop-script", "")
		g.write("pending-after", "2\n")
		if err := runDeploy([]string{"--host", deployHost, "--remove", "agent"}); err != nil {
			t.Fatal(err)
		}
		if got := g.running(); got != systemAgent {
			t.Errorf("after --remove the agent runs %q, want the system's %q", got, systemAgent)
		}
	})
}

// The script having answered, nothing is left to wait for.
func TestDeployFailsAtOnceWhenTheScriptDid(t *testing.T) {
	t.Run("a failing command", func(t *testing.T) {
		g := newFakeGuest(t)
		g.write("nix-store-fails", "")
		err := runDeploy(deployAgent)
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("err = %v, want the script's own failure", err)
		}
		if got := g.running(); got != "" {
			t.Errorf("the agent was restarted, into %q", got)
		}
	})

	// The compositor's restart doesn't cut ssh, so the script answers.
	t.Run("another drop-in winning", func(t *testing.T) {
		g := newFakeGuest(t)
		g.withCompositor("matching")
		const other = "/nix/store/other-compositor/bin/pocketnix-compositor"
		g.write(userDropIn("pocketnix-compositor.service.d/zz-other.conf"), "[Service]\nExecStart=\nExecStart="+other+"\n")
		start := time.Now()
		err := runDeploy([]string{"--host", deployHost, "--flake", "/fake/flake", "--native", "compositor"})
		if err == nil || !strings.Contains(err.Error(), other) {
			t.Errorf("err = %v, want it to name %s", err, other)
		}
		if waited := time.Since(start); waited >= deployReconnect {
			t.Errorf("failed after %s, having waited out deployReconnect", waited)
		}
	})
}

func TestDeployCompositor(t *testing.T) {
	deployCompositor := []string{"--host", deployHost, "--flake", "/fake/flake", "compositor"}
	const (
		crossCompositor  = "/nix/store/fakehash-pocketnix-compositor-cross-aarch64"
		crossGlibc       = "/nix/store/000000000000000000000000000cross-glibc-aarch64-unknown-linux-gnu-2.42-100"
		rewritten        = "/nix/store/fakehash-pocketnix-compositor-guest-libs"
		nativeCompositor = "/nix/store/fakehash-packages.aarch64-linux.pocketnix-compositor"
		args             = " --guest-socket /run/pocketnix/guest.sock"
	)
	newGuest := func(t *testing.T, closure string) *fakeGuest {
		g := newFakeGuest(t)
		g.withCompositor(closure)
		// The cross build links its own glibc, which the guest lacks.
		g.desktopClosure(crossCompositor, crossCompositor+" 10495240", crossGlibc+" 46706312")
		// The flake's native package shares the guest's libraries.
		g.desktopClosure(nativeCompositor, nativeCompositor+" 10485760", guestGlibc+" 46706312", guestXkbcommon+" 1520544")
		return g
	}

	t.Run("matching takes the rewrite", func(t *testing.T) {
		g := newGuest(t, "matching")
		out, err := stderrOf(t, func() error { return runDeploy(deployCompositor) })
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, want := g.compositorRunning(), rewritten+"/bin/pocketnix-compositor"+args; got != want {
			t.Errorf("the compositor runs %q, want %q", got, want)
		}
		for _, p := range []string{crossCompositor, crossGlibc} {
			if g.exists(p) {
				t.Errorf("the copy carried %s into the guest", p)
			}
		}
		if !g.exists(userDropIn(compositorDropIn)) {
			t.Errorf("no drop-in at %s", userDropIn(compositorDropIn))
		}
		for root, want := range map[string]string{compositorRoot: rewritten, compositorLibsRoot: guestCompositorPkg} {
			if got, err := os.Readlink(g.path(root)); err != nil || got != want {
				t.Errorf("GC root %s = %q, %v; want %s", root, got, err, want)
			}
		}
		if !strings.Contains(out, path.Base(rewritten)) || !strings.Contains(out, "MiB") {
			t.Errorf("the rewritten copy's size isn't logged:\n%s", out)
		}
	})

	t.Run("missing-soname takes the native fallback and says so", func(t *testing.T) {
		g := newGuest(t, "missing-soname")
		out, err := stderrOf(t, func() error { return runDeploy(deployCompositor) })
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, want := g.compositorRunning(), nativeCompositor+"/bin/pocketnix-compositor"+args; got != want {
			t.Errorf("the compositor runs %q, want %q", got, want)
		}
		if !strings.Contains(out, "libxkbcommon.so.0") || !strings.Contains(out, "native") {
			t.Errorf("no word of the missing libxkbcommon.so.0 and the native fallback:\n%s", out)
		}
		if g.exists(compositorLibsRoot) {
			t.Errorf("the native package left %s", compositorLibsRoot)
		}
	})

	t.Run("a loader refusing the rewrite takes the native fallback", func(t *testing.T) {
		g := newGuest(t, "matching")
		g.write("loader-lacks-version", "")
		out, err := stderrOf(t, func() error { return runDeploy(deployCompositor) })
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, want := g.compositorRunning(), nativeCompositor+"/bin/pocketnix-compositor"+args; got != want {
			t.Errorf("the compositor runs %q, want %q", got, want)
		}
		if !strings.Contains(out, "GLIBC_2.99") {
			t.Errorf("no word of the loader's refusal:\n%s", out)
		}
	})

	t.Run("a rewrite still referring to the cross closure refuses before the copy", func(t *testing.T) {
		g := newGuest(t, "matching")
		g.desktopClosure(rewritten, rewritten+" 10555968", crossGlibc+" 46706312")
		err := runDeploy(deployCompositor)
		if err == nil || !strings.Contains(err.Error(), crossGlibc) {
			t.Fatalf("err = %v, want a refusal naming %s", err, crossGlibc)
		}
		for _, p := range []string{rewritten, crossGlibc, userDropIn(compositorDropIn)} {
			if g.exists(p) {
				t.Errorf("a refused deploy left %s", p)
			}
		}
	})

	t.Run("a closure over deployMaxCopy refuses before the copy", func(t *testing.T) {
		g := newGuest(t, "matching")
		g.desktopClosure(nativeCompositor, nativeCompositor+" 10485760", fmt.Sprintf("/nix/store/000000000000000000000000000other-glibc-2.41 %d", deployMaxCopy+1))
		err := runDeploy(append([]string{"--native"}, deployCompositor...))
		if err == nil || !strings.Contains(err.Error(), "deployMaxCopy") {
			t.Fatalf("err = %v, want a refusal naming deployMaxCopy", err)
		}
		if g.exists(nativeCompositor) {
			t.Errorf("a refused deploy copied %s", nativeCompositor)
		}
	})

	t.Run("remove restores the system's compositor", func(t *testing.T) {
		g := newGuest(t, "matching")
		if err := runDeploy(deployCompositor); err != nil {
			t.Fatal(err)
		}
		if err := runDeploy([]string{"--host", deployHost, "--remove", "compositor"}); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{userDropIn(compositorDropIn), compositorRoot, compositorLibsRoot} {
			if g.exists(rel) {
				t.Errorf("%s survived --remove", rel)
			}
		}
		if got := g.compositorRunning(); got != systemCompositor {
			t.Errorf("after --remove the compositor runs %q, want the system's %q", got, systemCompositor)
		}
	})
}

func TestDeployCompositorLibraryMatch(t *testing.T) {
	const other = "/nix/store/00000000000000000000000000000old-glibc-2.40"
	needs := elfNeeds{
		interp: "ld-linux-aarch64.so.1",
		needed: []string{"libxkbcommon.so.0", "libgcc_s.so.1", "libm.so.6", "libc.so.6", "ld-linux-aarch64.so.1"},
	}
	libs := []guestLib{
		{other, "libc.so.6"},
		{guestXkbcommon, "libxkbcommon.so.0"},
		{guestGlibc, "ld-linux-aarch64.so.1"},
		{guestGlibc, "libc.so.6"},
		{guestGlibc, "libm.so.6"},
		{guestGccLib, "libgcc_s.so.1"},
	}
	interp, rpath, err := matchGuestLibs(needs, libs)
	if err != nil {
		t.Fatal(err)
	}
	if want := guestGlibc + "/lib/ld-linux-aarch64.so.1"; interp != want {
		t.Errorf("interpreter = %q, want %q", interp, want)
	}
	// libc comes from the loader's glibc, not the first one in the closure.
	want := []string{guestXkbcommon + "/lib", guestGccLib + "/lib", guestGlibc + "/lib"}
	slices.Sort(want)
	slices.Sort(rpath)
	if !slices.Equal(rpath, want) {
		t.Errorf("rpath = %q, want %q", rpath, want)
	}
}
