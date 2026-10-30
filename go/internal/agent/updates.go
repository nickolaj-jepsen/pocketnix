package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// logLines bounds Updates.Log.
const logLines = 60

// Updater pulls finished systems from the configured sources and switches
// to them, one operation at a time (docs/updates.md). Only a managed guest's
// own flake (the local source) is evaluated and built here.
type Updater struct {
	// Commands and paths, which tests replace with fakes.
	SSH, NixStore, Nix, NixEnv string
	// SystemdRun runs switch-to-configuration in its own unit, as
	// nixos-rebuild does, so it outlives the agent; empty runs it directly.
	SystemdRun string
	// Profile is the system profile; generations are its "-<n>-link"s.
	Profile string
	// Current and Booted resolve to the running and the booted system.
	Current, Booted string
	// StoreDir is the store prefix a source's answer must have.
	StoreDir string
	// Home replaces the session user's home (tests).
	Home string
	HTTP *http.Client

	// Changed is called after every state change, with the mutex released.
	Changed func()
	// Offline, when set and true, skips a background check.
	Offline func() bool
	// Inhibit, when set, keeps the guest (and so the phone) awake until the
	// returned closer is closed; fetches and switches take it.
	Inhibit func(why string) (io.Closer, error)

	mu        sync.Mutex
	cfg       UpdatesConfig
	state     string
	op        int
	available string
	source    string
	checkedAt time.Time
	err       string
	log       []string
	nextCheck time.Time
	pushTimer *time.Timer
	// local is the last check of a managed guest's flake.
	local    *localCheck
	blocked  *protocol.Plan
	packages []string
	run      *protocol.RunResult

	// The running operation: its context and how to cancel it.
	ctx    context.Context
	cancel context.CancelFunc
	// pending is a switch that starts when the cancelled operation ends.
	pending func(UpdatesConfig) error
	// wake tells Periodic that the schedule changed.
	wake chan struct{}
}

// NewUpdater returns an Updater for a NixOS guest.
func NewUpdater() *Updater {
	return &Updater{
		SSH:        "ssh",
		NixStore:   "nix-store",
		Nix:        "nix",
		NixEnv:     "nix-env",
		SystemdRun: "systemd-run",
		Profile:    "/nix/var/nix/profiles/system",
		Current:    "/run/current-system",
		Booted:     "/run/booted-system",
		StoreDir:   "/nix/store",
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		state:      protocol.UpdateIdle,
		wake:       make(chan struct{}, 1),
	}
}

// Configure applies the module's update settings; a running operation
// finishes with the ones it started with.
func (u *Updater) Configure(c UpdatesConfig) {
	u.mu.Lock()
	u.cfg = c
	// A boot's first check waits for the network.
	if c.interval > 0 && u.nextCheck.IsZero() {
		u.nextCheck = time.Now().Add(2 * time.Minute)
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default:
	}
	u.changed()
}

// Periodic runs background checks at the configured interval until ctx ends.
// With no interval or no source it sleeps until Configure changes that: an
// idle guest's wakeups are the phone's.
func (u *Updater) Periodic(ctx context.Context) {
	for {
		u.mu.Lock()
		var due <-chan time.Time
		var t *time.Timer
		if u.cfg.interval > 0 && len(u.cfg.sources()) > 0 && !u.nextCheck.IsZero() {
			t = time.NewTimer(time.Until(u.nextCheck))
			due = t.C
		}
		u.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-u.wake:
		case now := <-due:
			u.mu.Lock()
			run := u.cfg.interval > 0 && !now.Before(u.nextCheck)
			if run {
				u.nextCheck = now.Add(u.cfg.interval)
			}
			u.mu.Unlock()
			// A busy or offline updater skips this round; the next interval tries again.
			if run && (u.Offline == nil || !u.Offline()) {
				u.Check(Options{})
			}
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (u *Updater) config() UpdatesConfig {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cfg
}

// State returns the update state, with the generations read fresh.
func (u *Updater) State() protocol.Updates {
	u.mu.Lock()
	st := protocol.Updates{
		Type:      protocol.TypeUpdates,
		Sources:   u.cfg.sources(),
		State:     u.state,
		Op:        u.op,
		Available: u.available,
		Source:    u.source,
		Error:     u.err,
		Log:       slices.Clone(u.log),
		Packages:  slices.Clone(u.packages),
		Run:       u.run,
		Blocked:   u.blocked,
	}
	if l := u.local; l != nil && st.Source == protocol.SourceLocal && st.Available == l.out {
		plan := l.plan
		st.Plan = &plan
	}
	if !u.checkedAt.IsZero() {
		st.CheckedAt = u.checkedAt.Unix()
	}
	u.mu.Unlock()
	if st.Sources == nil {
		st.Sources = []string{}
	}
	if st.Log == nil {
		st.Log = []string{}
	}
	st.Current = resolve(u.Current)
	st.Booted = resolve(u.Booted)
	st.Generations = u.generations(st.Current, st.Booted)
	return st
}

// Check looks for a newer system in the background. It fails at once when
// no source is configured or another operation runs.
func (u *Updater) Check(o Options) error {
	return u.runOp(protocol.UpdateChecking, func(c UpdatesConfig) error {
		_, _, err := u.check(c, o)
		return err
	})
}

// Apply checks, then fetches and switches to what the check found, in the
// background. A managed guest reuses its last check while its flake is
// unchanged, and stops short of building what that plans unless
// o.AllowBuilds.
func (u *Updater) Apply(o Options) error {
	return u.runOp(protocol.UpdateChecking, func(c UpdatesConfig) error {
		path, src := u.reuseLocal(c, o)
		var err error
		if path == "" {
			path, src, err = u.check(c, o)
		}
		if err != nil {
			return err
		}
		if path == "" {
			return nil
		}
		defer u.inhibit("Updating the system")()
		u.setState(protocol.UpdateFetching)
		if src == protocol.SourceLocal {
			err = u.localFetch(c, o, path)
		} else {
			err = u.fetch(c, src, path)
		}
		if err != nil {
			return err
		}
		if err := switchable(path); err != nil {
			return err
		}
		if err := sameHost(path, c.host()); err != nil {
			return err
		}
		if err := u.cmd(envTimeout, u.NixEnv, "-p", u.Profile, "--set", path); err != nil {
			return err
		}
		if err := u.switchTo(path); err != nil {
			return err
		}
		u.mu.Lock()
		u.available, u.source = "", ""
		u.mu.Unlock()
		if src == protocol.SourceLocal {
			if err := u.localSwitched(c); err != nil {
				return fmt.Errorf("switched, but the flake's lock wasn't updated: %w", err)
			}
		}
		return u.prune(c, false)
	})
}

// reuseLocal returns what the last check of a managed guest's own flake
// found, while the flake and the pin are what it checked.
func (u *Updater) reuseLocal(c UpdatesConfig, o Options) (path, src string) {
	if c.managed == nil || c.BuildHost != nil || c.Published != nil {
		return "", ""
	}
	u.mu.Lock()
	lc := u.local
	u.mu.Unlock()
	if lc == nil || lc.pin != o.Pin || lc.out == resolve(u.Current) {
		return "", ""
	}
	if stamp, err := flakeStamp(c.managed.Flake); err != nil || stamp != lc.stamp {
		return "", ""
	}
	u.logf("%s: %s, as checked", protocol.SourceLocal, lc.out)
	return lc.out, protocol.SourceLocal
}

// SwitchGeneration makes generation n of the system profile current, in the
// background. It needs no update source.
func (u *Updater) SwitchGeneration(n int) error {
	link := fmt.Sprintf("%s-%d-link", u.Profile, n)
	path := resolve(link)
	if path == "" {
		return fmt.Errorf("no generation %d", n)
	}
	if err := switchable(path); err != nil {
		return err
	}
	op := func(c UpdatesConfig) error {
		u.logf("switching to generation %d (%s)", n, path)
		defer u.inhibit(fmt.Sprintf("Switching to generation %d", n))()
		if err := u.cmd(envTimeout, u.NixEnv, "-p", u.Profile, "--switch-generation", strconv.Itoa(n)); err != nil {
			return err
		}
		if err := u.switchTo(path); err != nil {
			return err
		}
		return u.prune(c, false)
	}
	u.mu.Lock()
	if u.state == protocol.UpdateChecking || u.state == protocol.UpdateFetching {
		// A rollback can't wait for a check that a hung build host holds up,
		// and the app's request can't wait for the check to stop: the switch
		// takes over when it has.
		if u.pending != nil {
			u.mu.Unlock()
			return errors.New("a switch is already waiting")
		}
		u.pending = op
		u.cancel()
		running := u.state
		u.mu.Unlock()
		u.logf("stopping the %s to switch to generation %d", running, n)
		return nil
	}
	err := u.startLocked(protocol.UpdateSwitching, op)
	u.mu.Unlock()
	if err == nil {
		u.changed()
	}
	return err
}

// Cancel stops the running check or download. A switch already under way
// finishes: stopping one halfway would leave the system half activated.
func (u *Updater) Cancel() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state == protocol.UpdateIdle || u.cancel == nil {
		return errors.New("nothing to cancel")
	}
	if u.state == protocol.UpdateSwitching {
		return errors.New("a switch can't be cancelled")
	}
	u.pending = nil
	u.cancel()
	return nil
}

func (u *Updater) runOp(state string, op func(UpdatesConfig) error) error {
	u.mu.Lock()
	none := len(u.cfg.sources()) == 0
	u.mu.Unlock()
	if none {
		return errors.New("no update source is configured (pocketnix.guest.updates)")
	}
	return u.start(state, op)
}

// start claims the updater for op and runs it in the background.
func (u *Updater) start(state string, op func(UpdatesConfig) error) error {
	u.mu.Lock()
	err := u.startLocked(state, op)
	u.mu.Unlock()
	if err == nil {
		u.changed()
	}
	return err
}

// startLocked is start with u.mu held; the caller reports the change.
func (u *Updater) startLocked(state string, op func(UpdatesConfig) error) error {
	if u.state != protocol.UpdateIdle {
		return fmt.Errorf("busy (%s)", u.state)
	}
	u.state, u.err, u.log, u.blocked = state, "", nil, nil
	u.op++
	cfg := u.cfg
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx, u.cancel = ctx, cancel
	go func() {
		err := op(cfg)
		if ctx.Err() != nil && err != nil {
			err = errors.New("cancelled")
		}
		cancel()
		if err != nil {
			u.logf("failed: %v", err)
		}
		u.mu.Lock()
		u.state = protocol.UpdateIdle
		u.cancel = nil
		if err != nil {
			u.err = err.Error()
		}
		// A switch waiting for this operation to stop takes over at once,
		// before anything else can claim the updater.
		if next := u.pending; next != nil {
			u.pending = nil
			u.startLocked(protocol.UpdateSwitching, next)
		}
		u.mu.Unlock()
		u.changed()
	}()
	return nil
}

// check asks each source in turn for its system; the first answer wins.
// It returns "" when that is the running system.
func (u *Updater) check(c UpdatesConfig, o Options) (path, source string, err error) {
	var errs []error
	for _, src := range c.sources() {
		p, err := u.target(c, src, o)
		if err != nil {
			u.logf("%s: %v", src, err)
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		current := resolve(u.Current)
		u.mu.Lock()
		u.checkedAt = time.Now()
		if p == current {
			u.available, u.source = "", ""
		} else {
			u.available, u.source = p, src
		}
		u.mu.Unlock()
		if p == current {
			u.logf("%s: up to date (%s)", src, p)
			return "", src, nil
		}
		u.logf("%s: %s is available", src, p)
		return p, src, nil
	}
	return "", "", errors.Join(errs...)
}

var storeName = regexp.MustCompile(`^[0-9a-z]{32}-[^/\s]+$`)

// storePath validates a source's answer: a store path, and this host's
// system (not a .drv to build, nor another machine's toplevel).
func (u *Updater) storePath(c UpdatesConfig, s string) (string, error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	dir, name := filepath.Split(s)
	if filepath.Clean(dir) != u.StoreDir || !storeName.MatchString(name) {
		return "", fmt.Errorf("answered %q, not a store path", s)
	}
	host := c.host()
	if strings.HasSuffix(name, ".drv") || !isToplevel(name[33:], host) {
		return "", fmt.Errorf("answered %q, not a nixos-system-%s-… toplevel", s, host)
	}
	return s, nil
}

// isToplevel reports whether name looks like host's system,
// nixos-system-<host>-<label>. "phone" still passes "phone-vm-…", and a label
// may hold anything, so [sameHost] settles it once the path is here.
func isToplevel(name, host string) bool {
	prefix := "nixos-system-" + host + "-"
	return host != "" && strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

// sameHost checks that the system at path was built for host, by the
// /etc/hostname it would install.
func sameHost(path, host string) error {
	b, err := os.ReadFile(filepath.Join(path, "etc", "hostname"))
	if err != nil {
		return fmt.Errorf("%s has no etc/hostname: not this host's toplevel", path)
	}
	if got := strings.TrimSpace(string(b)); got != host {
		return fmt.Errorf("%s is %q's toplevel, not this host's (%q)", path, got, host)
	}
	return nil
}

// host is the hostname a toplevel must be built for.
func (c UpdatesConfig) host() string {
	if c.hostname != "" {
		return c.hostname
	}
	h, _ := os.Hostname()
	return h
}

// switchable checks that path is a system the guest can activate, before
// it becomes a generation.
func switchable(path string) error {
	fi, err := os.Stat(filepath.Join(path, "bin", "switch-to-configuration"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s has no bin/switch-to-configuration: not a NixOS system", path)
	}
	return nil
}

// httpsOnly refuses a published URL's redirect away from https.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

func (u *Updater) target(c UpdatesConfig, src string, o Options) (string, error) {
	switch src {
	case protocol.SourceLocal:
		return u.localTarget(c, o)
	case protocol.SourceBuildHost:
		b := c.BuildHost
		u.logf("building %s on %s", b.Installable, b.Host)
		args := append(sshArgs(b), b.Host, "nix --extra-experimental-features 'nix-command flakes' build --no-link --print-out-paths --refresh "+shellQuote(b.Installable))
		var out strings.Builder
		if err := u.runCmd(buildTimeout, &out, nil, u.SSH, args...); err != nil {
			return "", err
		}
		return u.storePath(c, out.String())
	case protocol.SourcePublished:
		u.logf("fetching %s", c.Published.URL)
		req, err := http.NewRequestWithContext(u.opContext(), http.MethodGet, c.Published.URL, nil)
		if err != nil {
			return "", err
		}
		cl := *u.HTTP
		cl.CheckRedirect = httpsOnly
		resp, err := cl.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: %s", c.Published.URL, resp.Status)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return "", err
		}
		return u.storePath(c, string(b))
	}
	return "", fmt.Errorf("unknown source %q", src)
}

// fetch makes path valid in the guest's store: from its binary caches, or
// else (for a build host that doesn't push to one) over ssh from the host.
func (u *Updater) fetch(c UpdatesConfig, src, path string) error {
	// Substitute only: a cache may hold the path, but nothing gets built here.
	err := u.cmd(fetchTimeout, u.NixStore, "--realise", "--max-jobs", "0", path)
	if err == nil || src != protocol.SourceBuildHost || u.opContext().Err() != nil {
		return err
	}
	b := c.BuildHost
	u.logf("not in the guest's caches; copying from %s", b.Host)
	// Unsigned is fine: the guest already trusts this host with its whole system.
	env := []string{"NIX_SSHOPTS=" + strings.Join(sshArgs(b), " ")}
	// Through the daemon, the destination substitutes what its caches have
	// (a local store ignores --substitute-on-destination), so only the
	// host's own builds cross ssh.
	return u.runCmd(fetchTimeout, nil, env, u.Nix, "--extra-experimental-features", "nix-command", "copy", "-v", "--no-check-sigs",
		"--substitute-on-destination", "--to", "daemon", "--from", "ssh-ng://"+b.Host, path)
}

// inhibit takes Inhibit's lock and returns its release. Without one the
// operation runs anyway: a phone that may sleep beats no update.
func (u *Updater) inhibit(why string) func() {
	if u.Inhibit == nil {
		return func() {}
	}
	c, err := u.Inhibit(why)
	if err != nil {
		u.logf("can't keep the phone awake: %v", err)
		return func() {}
	}
	return func() { c.Close() }
}

func (u *Updater) switchTo(path string) error {
	u.setState(protocol.UpdateSwitching)
	return u.activate(path, "switch")
}

// activate runs path's switch-to-configuration action.
func (u *Updater) activate(path, action string) error {
	stc := filepath.Join(path, "bin", "switch-to-configuration")
	if u.SystemdRun == "" {
		return u.cmd(switchTimeout, stc, action)
	}
	return u.cmd(switchTimeout, u.SystemdRun, "-E", "LOCALE_ARCHIVE", "-E", "NIXOS_INSTALL_BOOTLOADER",
		"--collect", "--no-ask-password", "--pipe", "--quiet", "--service-type=exec",
		"--unit=pocketnix-switch-to-configuration", stc, action)
}

// Deadlines per phase. They're backstops; the ssh keepalives (sshArgs) notice a
// build host that went away much sooner.
const (
	buildTimeout  = 3 * time.Hour
	fetchTimeout  = 2 * time.Hour
	switchTimeout = 30 * time.Minute
	envTimeout    = 2 * time.Minute
)

func sshArgs(b *BuildHost) []string {
	// No prompts: nobody can answer them. Unknown host keys fail too; the
	// module's users list the build host in programs.ssh.knownHosts.
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4"}
	if b.SSHKey != "" {
		args = append(args, "-i", b.SSHKey)
	}
	return args
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (u *Updater) cmd(timeout time.Duration, name string, args ...string) error {
	return u.runCmd(timeout, nil, nil, name, args...)
}

// opContext is the running operation's; Cancel ends it.
func (u *Updater) opContext() context.Context {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ctx == nil {
		return context.Background()
	}
	return u.ctx
}

// runCmd runs a command for at most timeout, logging its output line by
// line; stdout goes to out instead when out is set. A cancel ends it, except
// during a switch, which must not stop halfway.
func (u *Updater) runCmd(timeout time.Duration, out io.Writer, env []string, name string, args ...string) error {
	ctx := u.opContext()
	u.mu.Lock()
	if u.state == protocol.UpdateSwitching {
		ctx = context.WithoutCancel(ctx)
	}
	u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// A child that keeps the output pipe open mustn't hold the updater after a kill.
	cmd.WaitDelay = 5 * time.Second
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	pr, pw := io.Pipe()
	cmd.Stderr = pw
	cmd.Stdout = pw
	if out != nil {
		cmd.Stdout = out
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			u.logf("%s", sc.Text())
		}
		io.Copy(io.Discard, pr)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
	}
	return nil
}

func (u *Updater) setState(s string) {
	u.mu.Lock()
	u.state = s
	u.mu.Unlock()
	u.changed()
}

func (u *Updater) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	u.mu.Lock()
	u.log = append(u.log, line)
	if len(u.log) > logLines {
		u.log = u.log[len(u.log)-logLines:]
	}
	u.mu.Unlock()
	u.changed()
}

// changed coalesces bursts (a build's log lines) into one Changed call.
func (u *Updater) changed() {
	if u.Changed == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pushTimer != nil {
		return
	}
	u.pushTimer = time.AfterFunc(150*time.Millisecond, func() {
		u.mu.Lock()
		u.pushTimer = nil
		u.mu.Unlock()
		u.Changed()
	})
}

var generationLink = regexp.MustCompile(`^(.+)-(\d+)-link$`)

// generations lists the system profile's generations, newest first.
func (u *Updater) generations(current, booted string) []protocol.Generation {
	dir, base := filepath.Split(u.Profile)
	entries, _ := os.ReadDir(dir)
	gens := []protocol.Generation{}
	for _, e := range entries {
		m := generationLink.FindStringSubmatch(e.Name())
		if m == nil || m[1] != base {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		link := filepath.Join(dir, e.Name())
		path := resolve(link)
		if path == "" {
			continue
		}
		g := protocol.Generation{Number: n, Path: path, Current: path == current, Booted: path == booted}
		if fi, err := os.Lstat(link); err == nil {
			g.Date = fi.ModTime().Unix()
		}
		if b, err := os.ReadFile(filepath.Join(path, "nixos-version")); err == nil {
			g.Label = strings.TrimSpace(string(b))
		}
		gens = append(gens, g)
	}
	slices.SortFunc(gens, func(a, b protocol.Generation) int { return b.Number - a.Number })
	return gens
}

// resolve follows a profile or /run link to its store path, or returns "".
func resolve(link string) string {
	p, err := filepath.EvalSymlinks(link)
	if err != nil {
		return ""
	}
	return p
}
