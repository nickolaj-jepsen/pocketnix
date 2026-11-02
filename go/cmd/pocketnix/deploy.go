package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

const deployUsage = `usage: pocketnix deploy [--host <ssh host>] [--ssh-config <file>] [--flake <ref>] [--native] agent|compositor
       pocketnix deploy [--host <ssh host>] [--ssh-config <file>] --remove [agent|compositor]

Builds a component on this machine and runs it in a phone guest until the
guest reboots, through a runtime systemd drop-in; the system's own build
comes back on reboot or with --remove. Nothing is built in the guest.

flags:
  --host <ssh host>    the guest, as ssh names it (default phone-usb)
  --ssh-config <file>  an ssh config for every ssh and nix copy call
  --flake <ref>        the flake to build from (default: this checkout's git toplevel)
  --native             build the flake's own aarch64 package instead of cross-building
  --remove             end a deploy early (both components without one named)
`

const deployDefaultHost = "phone-usb"

const (
	// deployRoots holds the guest's GC roots for deployed paths: the agent collects
	// garbage after each boot and switch, and the roots go with /run on reboot.
	deployRoots = "/run/pocketnix-deploy"
	// deployDropIn names every drop-in deploy writes, so a leftover one is recognised.
	deployDropIn = "pocketnix-deploy.conf"
	// deployRemoteProgram is the guest's nix-daemon as root, over sudo -n: the guest
	// trusts only root (nix.settings.trusted-users), and -n fails instead of prompting.
	// %20 separates its words in a store URL; + doesn't.
	deployRemoteProgram = "sudo%20-n%20/run/current-system/sw/bin/nix-daemon"
	// deployReconnect bounds the wait for the unit after a restart, which for the
	// agent cuts ssh: ssh rides the agent's port forward.
	deployReconnect = 90 * time.Second
	deployRetry     = 2 * time.Second
	// deployAttempt bounds one ssh call; deployConnectSecs is ssh's ConnectTimeout.
	deployAttempt     = 30 * time.Second
	deployConnectSecs = 10
)

// deployGuestRoot prefixes the guest paths deploy names itself (the roots and the
// drop-ins): empty on a real guest, a model guest's directory in the tests.
var deployGuestRoot = ""

// deployComponent is a unit deploy swaps a binary in.
type deployComponent struct {
	name string
	unit string
	// user is a unit of the session user's manager, not the system's.
	user bool
	// exe is the binary the unit's ExecStart runs, in the package's bin.
	exe string
	// cross and native are the flake attributes that build it.
	cross, native string
}

var deployComponents = []deployComponent{
	{
		name: "agent", unit: "pocketnix-agent.service", exe: "pocketnix-agent",
		cross: "pocketnix-cross-aarch64", native: "packages.aarch64-linux.pocketnix",
	},
	{name: "compositor", unit: "pocketnix-compositor.service", user: true, exe: "pocketnix-compositor"},
}

func deployComponentNamed(name string) deployComponent {
	for _, c := range deployComponents {
		if c.name == name {
			return c
		}
	}
	panic("no deploy component " + name)
}

// systemctl is the shell words that reach the component's manager.
func (c deployComponent) systemctl() string {
	if c.user {
		return "systemctl --user"
	}
	return "sudo -n systemctl"
}

// sudo prefixes a command that writes the component's drop-in.
func (c deployComponent) sudo() string {
	if c.user {
		return ""
	}
	return "sudo -n "
}

// dropInDir is a shell word for the component's runtime drop-in directory.
func (c deployComponent) dropInDir() string {
	if c.user {
		return adb.ShellQuote(deployGuestRoot+"/run/user/") + `"$(id -u)"` + adb.ShellQuote("/systemd/user/"+c.unit+".d")
	}
	return adb.ShellQuote(deployGuestRoot + "/run/systemd/system/" + c.unit + ".d")
}

func (c deployComponent) dropIn() string { return c.dropInDir() + "/" + deployDropIn }

func (c deployComponent) root() string {
	return adb.ShellQuote(deployGuestRoot + deployRoots + "/" + c.name)
}

type deployOpts struct {
	host, sshConfig, flake string
	native, remove         bool
	// components is what to deploy or remove: "agent", "compositor".
	components []string
	log        io.Writer
}

func parseDeploy(args []string) (*deployOpts, error) {
	o := &deployOpts{log: os.Stderr}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, deployUsage) }
	fs.StringVar(&o.host, "host", deployDefaultHost, "")
	fs.StringVar(&o.sshConfig, "ssh-config", "", "")
	fs.StringVar(&o.flake, "flake", "", "")
	fs.BoolVar(&o.native, "native", false, "")
	fs.BoolVar(&o.remove, "remove", false, "")
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return nil, errUsage
	}
	switch {
	case fs.NArg() == 1 && (fs.Arg(0) == "agent" || fs.Arg(0) == "compositor"):
		o.components = []string{fs.Arg(0)}
	case fs.NArg() == 0 && o.remove:
		o.components = []string{"agent", "compositor"}
	default:
		fs.Usage()
		return nil, errUsage
	}
	return o, nil
}

func runDeploy(args []string) error {
	o, err := parseDeploy(args)
	if err != nil {
		return err
	}
	g := deployGuest{host: o.host, sshConfig: o.sshConfig}
	if !o.remove && deployComponentNamed(o.components[0]).cross == "" {
		return fmt.Errorf("deploy %s: not implemented yet", o.components[0])
	}
	if err := g.checkSudo(); err != nil {
		return err
	}
	if o.remove {
		for _, name := range o.components {
			if err := removeDeploy(o, g, deployComponentNamed(name)); err != nil {
				return err
			}
		}
		return nil
	}
	return deploy(o, g, deployComponentNamed(o.components[0]))
}

func deploy(o *deployOpts, g deployGuest, c deployComponent) error {
	st, err := g.state(c)
	if err != nil {
		return err
	}
	if st.execStart == "" {
		return fmt.Errorf("%s isn't loaded on %s", c.unit, o.host)
	}
	for _, l := range st.leftovers {
		fmt.Fprintf(o.log, "warning: a leftover deploy of the %s is in place (%s); it lasts until the guest reboots or pocketnix deploy --remove %s\n", l.component, l.dropIn, l.component)
	}
	fmt.Fprintf(o.log, "warning: the deployed %s keeps %s's current ExecStart arguments: a switch that changes them takes effect only after a reboot or --remove\n", c.name, c.unit)

	installable, err := deployInstallable(o, c)
	if err != nil {
		return err
	}
	out, err := nixBuild(installable)
	if err != nil {
		return err
	}
	exe := out + "/bin/" + c.exe
	execStart, err := swapExecStart(st.execStart, c, exe)
	if err != nil {
		return err
	}
	if err := g.copy(out); err != nil {
		return err
	}

	dropIn := strings.Join([]string{
		"# pocketnix deploy: until the guest reboots, or pocketnix deploy --remove " + c.name,
		"[Service]",
		"ExecStart=",
		"ExecStart=" + execStart,
	}, "\n") + "\n"
	script := fmt.Sprintf(`set -e
sudo -n mkdir -p %[1]s
sudo -n nix-store --realise %[2]s --add-root %[3]s >/dev/null
%[4]smkdir -p %[5]s
printf %%s %[6]s | %[4]stee %[7]s >/dev/null
%[8]s daemon-reload
%[8]s --no-block restart %[9]s
`, adb.ShellQuote(deployGuestRoot+deployRoots), adb.ShellQuote(out), c.root(), c.sudo(),
		c.dropInDir(), adb.ShellQuote(dropIn), c.dropIn(), c.systemctl(), adb.ShellQuote(c.unit))
	// The restart may cut this session before it answers.
	if _, err := g.run(script); err != nil && !errors.Is(err, errGuestUnreachable) {
		return err
	}
	if err := g.awaitRestart(c, st.invocation, exe); err != nil {
		return err
	}
	fmt.Fprintf(o.log, "%s: %s runs %s until the guest reboots; pocketnix deploy --remove %s ends it\n", o.host, c.unit, exe, c.name)
	return nil
}

func removeDeploy(o *deployOpts, g deployGuest, c deployComponent) error {
	st, err := g.state(c)
	if err != nil {
		return err
	}
	found := false
	for _, l := range st.leftovers {
		found = found || l.component == c.name
	}
	script := fmt.Sprintf("set -e\nsudo -n rm -f %s\n", c.root())
	if !found {
		_, err := g.run(script)
		if err == nil {
			fmt.Fprintf(o.log, "%s: no deploy of the %s to remove\n", o.host, c.name)
		}
		return err
	}
	script += fmt.Sprintf("%[1]srm -f %[2]s\n%[3]s daemon-reload\n", c.sudo(), c.dropIn(), c.systemctl())
	if st.execStart == "" {
		// Not loaded, so not running: the drop-in only needs to go.
		_, err := g.run(script)
		return err
	}
	script += fmt.Sprintf("%s --no-block restart %s\n", c.systemctl(), adb.ShellQuote(c.unit))
	if _, err := g.run(script); err != nil && !errors.Is(err, errGuestUnreachable) {
		return err
	}
	system, _, _ := strings.Cut(st.execStart, " ")
	if err := g.awaitRestart(c, st.invocation, system); err != nil {
		return err
	}
	fmt.Fprintf(o.log, "%s: %s runs the system's %s again\n", o.host, c.unit, system)
	return nil
}

func deployInstallable(o *deployOpts, c deployComponent) (string, error) {
	flake := o.flake
	if flake == "" {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "", fmt.Errorf("--flake: this directory isn't in a git checkout: %w", err)
		}
		flake = strings.TrimSpace(string(out))
	}
	if o.native {
		return flake + "#" + c.native, nil
	}
	return flake + "#" + c.cross, nil
}

// swapExecStart is a unit file's ExecStart value with its binary replaced by exe.
func swapExecStart(line string, c deployComponent, exe string) (string, error) {
	bin, args, _ := strings.Cut(line, " ")
	if !strings.HasPrefix(bin, "/nix/store/") || !strings.HasSuffix(bin, "/"+c.exe) {
		return "", fmt.Errorf("%s's ExecStart doesn't start with a store path's %s: %q", c.unit, c.exe, line)
	}
	if args == "" {
		return exe, nil
	}
	return exe + " " + args, nil
}

var errGuestUnreachable = errors.New("ssh couldn't reach the guest")

// deployGuest runs shell scripts in the guest over ssh.
type deployGuest struct{ host, sshConfig string }

func (g deployGuest) sshArgs() []string {
	var args []string
	if g.sshConfig != "" {
		args = append(args, "-F", g.sshConfig)
	}
	return append(args, "-o", "BatchMode=yes", "-o", "ConnectTimeout="+strconv.Itoa(deployConnectSecs), g.host)
}

// run runs a POSIX sh script as the ssh user, fed on stdin so the user's login
// shell never parses it, and returns its stdout. An ssh failure (status 255)
// or a call over deployAttempt wraps errGuestUnreachable.
func (g deployGuest) run(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deployAttempt)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(g.sshArgs(), "sh -s")...)
	// SIGTERM lets ssh end its ProxyCommand, which removes its adb forward.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("ssh %s: %w: no answer in %s", g.host, errGuestUnreachable, deployAttempt)
	case errors.As(err, &exit) && exit.ExitCode() == 255:
		return "", fmt.Errorf("ssh %s: %w: %s", g.host, errGuestUnreachable, strings.TrimSpace(stderr.String()))
	default:
		return stdout.String(), fmt.Errorf("ssh %s: %w: %s", g.host, err, strings.TrimSpace(stderr.String()))
	}
}

// checkSudo refuses a guest whose ssh user lacks passwordless sudo: the copy
// runs a root nix-daemon that skips signature checks, which grants nothing new
// only to a user who can be root already.
func (g deployGuest) checkSudo() error {
	_, err := g.run("sudo -n true\n")
	if err != nil && !errors.Is(err, errGuestUnreachable) {
		return fmt.Errorf("deploy needs passwordless sudo for the ssh user on %s: %w", g.host, err)
	}
	return err
}

// copy copies a store path and its closure into the guest's store, as root.
func (g deployGuest) copy(path string) error {
	cmd := exec.Command("nix", "copy", "--no-check-sigs", "--to", "ssh-ng://"+g.host+"?remote-program="+deployRemoteProgram, path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if g.sshConfig != "" {
		// Nix splits NIX_SSHOPTS as a shell would.
		cmd.Env = append(os.Environ(), "NIX_SSHOPTS="+strings.TrimSpace(os.Getenv("NIX_SSHOPTS")+" -F "+adb.ShellQuote(g.sshConfig)))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix copy %s to %s: %w", path, g.host, err)
	}
	return nil
}

type deployLeftover struct{ component, dropIn string }

// deployState is what deploy reads from the guest before it changes anything.
type deployState struct {
	// leftovers are the deploys in place, of every component.
	leftovers []deployLeftover
	// execStart is the unit file's own ExecStart value, without any drop-in;
	// empty when the unit isn't loaded.
	execStart string
	// invocation is the unit's InvocationID, which a restart changes.
	invocation string
}

func (g deployGuest) state(c deployComponent) (deployState, error) {
	var b strings.Builder
	for _, other := range deployComponents {
		fmt.Fprintf(&b, "f=%s; [ ! -e \"$f\" ] || echo \"leftover %s $f\"\n", other.dropIn(), other.name)
	}
	fmt.Fprintf(&b, `frag=$(%[1]s show -P FragmentPath %[2]s)
[ -z "$frag" ] || grep '^ExecStart=' "$frag" | sed 's/^ExecStart=/execstart /'
echo "invocation $(%[1]s show -P InvocationID %[2]s)"
`, c.systemctl(), adb.ShellQuote(c.unit))
	out, err := g.run(b.String())
	if err != nil {
		return deployState{}, err
	}
	var st deployState
	var execStarts []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), " ")
		switch key {
		case "leftover":
			name, path, _ := strings.Cut(val, " ")
			st.leftovers = append(st.leftovers, deployLeftover{name, path})
		case "execstart":
			if val != "" {
				execStarts = append(execStarts, val)
			}
		case "invocation":
			st.invocation = val
		}
	}
	if len(execStarts) > 1 {
		return deployState{}, fmt.Errorf("%s's unit file has %d ExecStart lines, not one", c.unit, len(execStarts))
	}
	if len(execStarts) == 1 {
		st.execStart = execStarts[0]
	}
	return st, nil
}

// awaitRestart waits until the unit runs a new invocation of exe, reconnecting
// while the restart has cut ssh.
func (g deployGuest) awaitRestart(c deployComponent, before, exe string) error {
	script := fmt.Sprintf("%[1]s show -P ExecStart %[2]s\necho \"invocation $(%[1]s show -P InvocationID %[2]s)\"\necho \"state $(%[1]s show -P ActiveState %[2]s)\"\n",
		c.systemctl(), adb.ShellQuote(c.unit))
	deadline := time.Now().Add(deployReconnect)
	for {
		out, err := g.run(script)
		if err != nil && !errors.Is(err, errGuestUnreachable) {
			return err
		}
		if err == nil {
			var path, invocation, state string
			if m := execStartPath.FindStringSubmatch(out); m != nil {
				path = m[1]
			}
			for _, line := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(line, "invocation "); ok {
					invocation = v
				} else if v, ok := strings.CutPrefix(line, "state "); ok {
					state = v
				}
			}
			if path != exe {
				return fmt.Errorf("%s's ExecStart runs %q, not %s", c.unit, path, exe)
			}
			if invocation != before && state == "active" {
				return nil
			}
			err = fmt.Errorf("%s is %s, invocation %s", c.unit, state, invocation)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't come back running %s in %s: %w", c.unit, exe, deployReconnect, err)
		}
		time.Sleep(deployRetry)
	}
}
