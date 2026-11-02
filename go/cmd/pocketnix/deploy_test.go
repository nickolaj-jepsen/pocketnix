package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
PATH="$FAKE_GUEST_BIN:$PATH" FAKE_UID=1000 sh -c "$*"
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
case $1 in
build)
  for a; do inst=$a; done
  echo "/nix/store/fakehash-${inst#*#}"
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
  mkdir -p "$FAKE_GUEST/store/${path##*/}"
  ;;
*)
  echo "fake nix: $1 not modelled" >&2
  exit 97
  ;;
esac
`

// The guest's side: sudo with a knob for a password, nix-store's roots, and
// systemd's fragments, runtime drop-ins, what daemon-reload loaded and what a
// restart started.
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
[ "$FAKE_UID" = 0 ] || { echo "nix-store: permission denied" >&2; exit 1; }
path= root=
while [ $# -gt 0 ]; do
  case $1 in
    --realise|-r) shift ;;
    --add-root) root=$2; shift 2 ;;
    *) path=$1; shift ;;
  esac
done
[ -e "$FAKE_GUEST/store/${path##*/}" ] || { echo "error: path '$path' is not valid" >&2; exit 1; }
ln -sfn "$path" "$root" && echo "$root"
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
      loaded=$(cat "$s/$unit.loaded")
      echo "{ path=${loaded%% *} ; argv[]=$loaded ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
      ;;
    InvocationID) cat "$s/$unit.invocation" 2>/dev/null || echo 0 ;;
    ActiveState) echo active ;;
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
	t   *testing.T
	dir string
}

func newFakeGuest(t *testing.T) *fakeGuest {
	t.Helper()
	base := t.TempDir()
	g := &fakeGuest{t: t, dir: filepath.Join(base, "guest")}
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
	t.Setenv("FAKE_GUEST_BIN", guestBin)
	t.Setenv("FAKE_GUEST_HOST", deployHost)
	t.Setenv("FAKE_SSH_CONFIG", "")
	t.Setenv("NIX_SSHOPTS", "")
	old := deployGuestRoot
	deployGuestRoot = g.dir
	t.Cleanup(func() { deployGuestRoot = old })
	return g
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

const (
	agentDropIn = "run/systemd/system/pocketnix-agent.service.d/pocketnix-deploy.conf"
	agentRoot   = "run/pocketnix-deploy/agent"
)

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

func TestDeployAgentAndRemove(t *testing.T) {
	deployAgent := []string{"--host", deployHost, "--flake", "/fake/flake", "agent"}
	const crossAgent = "/nix/store/fakehash-pocketnix-cross-aarch64"

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
		for _, rel := range []string{"store", agentDropIn, agentRoot} {
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
		if !g.exists("store/fakehash-pocketnix-cross-aarch64") {
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
