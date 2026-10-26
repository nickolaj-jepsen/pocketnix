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
// to them, one operation at a time (design.md, Updates and recovery). The
// guest never evaluates: every source yields a toplevel store path.
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
	HTTP     *http.Client

	// Changed is called after every state change, with the mutex released.
	Changed func()

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

	// The running operation: its context, how to cancel it, closed when it ends.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
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
	u.changed()
}

// Periodic runs background checks at the configured interval until ctx ends.
func (u *Updater) Periodic(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			u.mu.Lock()
			due := u.cfg.interval > 0 && !u.nextCheck.IsZero() && now.After(u.nextCheck)
			if due {
				u.nextCheck = now.Add(u.cfg.interval)
			}
			u.mu.Unlock()
			if due {
				// A busy updater skips this round; the next interval tries again.
				u.Check()
			}
		}
	}
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
func (u *Updater) Check() error {
	return u.run(protocol.UpdateChecking, func(c UpdatesConfig) error {
		_, _, err := u.check(c)
		return err
	})
}

// Apply checks, then fetches and switches to what the check found, in the
// background.
func (u *Updater) Apply() error {
	return u.run(protocol.UpdateChecking, func(c UpdatesConfig) error {
		path, src, err := u.check(c)
		if err != nil {
			return err
		}
		if path == "" {
			return nil
		}
		u.setState(protocol.UpdateFetching)
		if err := u.fetch(c, src, path); err != nil {
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
		return nil
	})
}

// SwitchGeneration makes generation n of the system profile current, in the
// background. It needs no update source.
func (u *Updater) SwitchGeneration(n int) error {
	link := fmt.Sprintf("%s-%d-link", u.Profile, n)
	path := resolve(link)
	if path == "" {
		return fmt.Errorf("no generation %d", n)
	}
	// A rollback can't wait for a check that a hung build host holds up.
	u.mu.Lock()
	checking, done := u.state == protocol.UpdateChecking || u.state == protocol.UpdateFetching, u.done
	u.mu.Unlock()
	if checking {
		u.Cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
	return u.start(protocol.UpdateSwitching, func(UpdatesConfig) error {
		u.logf("switching to generation %d (%s)", n, path)
		if err := u.cmd(envTimeout, u.NixEnv, "-p", u.Profile, "--switch-generation", strconv.Itoa(n)); err != nil {
			return err
		}
		return u.switchTo(path)
	})
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
	u.cancel()
	return nil
}

func (u *Updater) run(state string, op func(UpdatesConfig) error) error {
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
	if u.state != protocol.UpdateIdle {
		busy := u.state
		u.mu.Unlock()
		return fmt.Errorf("busy (%s)", busy)
	}
	u.state, u.err, u.log = state, "", nil
	u.op++
	cfg := u.cfg
	ctx, cancel := context.WithCancel(context.Background())
	u.ctx, u.cancel, u.done = ctx, cancel, make(chan struct{})
	done := u.done
	u.mu.Unlock()
	u.changed()
	go func() {
		err := op(cfg)
		if ctx.Err() != nil && err != nil {
			err = errors.New("cancelled")
		}
		cancel()
		u.mu.Lock()
		u.state = protocol.UpdateIdle
		u.cancel = nil
		if err != nil {
			u.err = err.Error()
		}
		close(done)
		u.mu.Unlock()
		if err != nil {
			u.logf("failed: %v", err)
		}
		u.changed()
	}()
	return nil
}

// check asks each source in turn for its system; the first answer wins.
// It returns "" when that is the running system.
func (u *Updater) check(c UpdatesConfig) (path, source string, err error) {
	var errs []error
	for _, src := range c.sources() {
		p, err := u.target(c, src)
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
	if strings.HasSuffix(name, ".drv") || c.hostname != "" && !strings.HasPrefix(name[33:], "nixos-system-"+c.hostname+"-") {
		return "", fmt.Errorf("answered %q, not a nixos-system-%s-… toplevel", s, c.hostname)
	}
	return s, nil
}

func (u *Updater) target(c UpdatesConfig, src string) (string, error) {
	switch src {
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
		resp, err := u.HTTP.Do(req)
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
	return u.runCmd(fetchTimeout, nil, env, u.Nix, "--extra-experimental-features", "nix-command", "copy", "-v", "--no-check-sigs", "--from", "ssh-ng://"+b.Host, path)
}

func (u *Updater) switchTo(path string) error {
	u.setState(protocol.UpdateSwitching)
	stc := filepath.Join(path, "bin", "switch-to-configuration")
	if u.SystemdRun == "" {
		return u.cmd(switchTimeout, stc, "switch")
	}
	return u.cmd(switchTimeout, u.SystemdRun, "-E", "LOCALE_ARCHIVE", "-E", "NIXOS_INSTALL_BOOTLOADER",
		"--collect", "--no-ask-password", "--pipe", "--quiet", "--service-type=exec",
		"--unit=pocketnix-switch-to-configuration", stc, "switch")
}

// Deadlines per phase. They're backstops; the ssh keepalives below notice a
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
