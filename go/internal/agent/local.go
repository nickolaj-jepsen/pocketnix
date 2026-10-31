package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// A managed guest rebuilds its own flake (pocketnix.guest.managed): the app
// writes its pocketnix.json and may pin its pocketnix release, a check
// evaluates it and plans the build, and an apply builds and switches.

// selectionsFile is the app's part of a managed guest's flake.
const selectionsFile = "pocketnix.json"

// evalTimeout bounds one evaluation of the guest's system.
const evalTimeout = 30 * time.Minute

var (
	semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// As the module's managed.selections.user and the app's Selections.USER_NAME.
	userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// pinLine is the seeded flake.nix's pocketnix input; a flake.nix without
	// it isn't pinned by the app. The other side is nix/managed/flake.nix.
	pinLine = regexp.MustCompile(`(inputs\.pocketnix\.url\s*=\s*"github:nickolaj-jepsen/pocketnix/v)([^"]*)(")`)
)

// Options are an update-check's or update-apply's.
type Options struct {
	// Pin is the pocketnix release a managed guest's flake moves to.
	Pin string
	// AllowBuilds lets an apply build what its plan lists.
	AllowBuilds bool
}

// localCheck is what a check of the guest's own flake found, kept for the
// apply that follows it while the flake doesn't change.
type localCheck struct {
	pin, stamp string
	drv, out   string
	plan       protocol.Plan
}

// WriteSelections writes a managed guest's pocketnix.json, for its next
// rebuild. It reports whether the file changed.
func (u *Updater) WriteSelections(s protocol.Selections) (bool, error) {
	u.mu.Lock()
	m := u.cfg.managed
	u.mu.Unlock()
	if m == nil {
		return false, errors.New("this guest isn't managed by the phone")
	}
	if s.User != "" && !userName.MatchString(s.User) {
		return false, fmt.Errorf("user %q is not a valid user name", s.User)
	}
	services := map[string]bool{}
	for name, on := range s.Services {
		if !slices.Contains(protocol.Services, name) {
			return false, fmt.Errorf("unknown service %q", name)
		}
		if on {
			services[name] = true
		}
	}
	if s.KeepGenerations < 0 {
		return false, fmt.Errorf("keepGenerations %d is negative", s.KeepGenerations)
	}
	b, err := json.MarshalIndent(protocol.Selections{User: s.User, Services: services, KeepGenerations: s.KeepGenerations}, "", "  ")
	if err != nil {
		return false, err
	}
	b = append(b, '\n')
	path := filepath.Join(m.Flake, selectionsFile)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// ReadSelections returns a managed guest's pocketnix.json.
func (u *Updater) ReadSelections() (protocol.Selections, error) {
	u.mu.Lock()
	m := u.cfg.managed
	u.mu.Unlock()
	s := protocol.Selections{Type: protocol.TypeSelections, Services: map[string]bool{}}
	if m == nil {
		return s, errors.New("this guest isn't managed by the phone")
	}
	b, err := os.ReadFile(filepath.Join(m.Flake, selectionsFile))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", selectionsFile, err)
	}
	s.Type = protocol.TypeSelections
	if s.Services == nil {
		s.Services = map[string]bool{}
	}
	return s, nil
}

// localTarget evaluates the guest's flake, pinned to o.Pin when set, and
// plans realising it. It returns the system's store path.
func (u *Updater) localTarget(c UpdatesConfig, o Options) (string, error) {
	m := c.managed
	if o.Pin != "" && !semver.MatchString(o.Pin) {
		return "", fmt.Errorf("pin %q is not a release version", o.Pin)
	}
	override, err := pinOverride(m.Flake, o.Pin)
	if err != nil {
		return "", err
	}
	if o.Pin != "" && override == nil {
		u.logf("flake.nix doesn't pin a pocketnix release: not moving it to %s", o.Pin)
		o.Pin = ""
	}
	stamp, err := flakeStamp(m.Flake)
	if err != nil {
		return "", err
	}
	u.logf("evaluating %s", m.Flake)
	args := append([]string{"eval", "--raw", "--no-write-lock-file",
		fmt.Sprintf("path:%s#nixosConfigurations.%s.config.system.build.toplevel.drvPath", m.Flake, m.Attr)}, override...)
	var out strings.Builder
	if err := u.runArgv(evalTimeout, &out, u.throttled(m, "", u.Nix, nixArgs(args...)...)); err != nil {
		return "", err
	}
	drv := strings.TrimSpace(out.String())
	if !strings.HasSuffix(drv, ".drv") || !strings.HasPrefix(drv, u.StoreDir+"/") {
		return "", fmt.Errorf("evaluated to %q, not a derivation", drv)
	}
	drvs, err := u.showDerivations(drv)
	if err != nil {
		return "", err
	}
	path := drvs[drv].out
	if path == "" {
		return "", fmt.Errorf("%s has no out", drv)
	}
	plan := protocol.Plan{Builds: []string{}}
	if path != resolve(u.Current) {
		if plan, err = u.plan(m, "", drv+"^*"); err != nil {
			return "", err
		}
	}
	u.mu.Lock()
	u.local = &localCheck{pin: o.Pin, stamp: stamp, drv: drv, out: path, plan: plan}
	u.mu.Unlock()
	return path, nil
}

// localFetch builds the system the last check planned, after checking it
// again when the flake changed since.
func (u *Updater) localFetch(c UpdatesConfig, o Options, path string) error {
	u.mu.Lock()
	lc := u.local
	u.mu.Unlock()
	stamp, err := flakeStamp(c.managed.Flake)
	if err != nil {
		return err
	}
	if lc == nil || lc.out != path || lc.stamp != stamp || lc.pin != o.Pin {
		return errors.New("the flake changed since the check: check again")
	}
	if len(lc.plan.Builds) > 0 && !o.AllowBuilds {
		u.block(lc.plan)
		return fmt.Errorf("would build on the phone: %s", strings.Join(lc.plan.Builds, ", "))
	}
	u.logf("building %s", lc.drv)
	return u.runArgv(fetchTimeout, nil, u.throttled(c.managed, "", u.NixStore, "--realise", lc.drv))
}

// localSwitched moves the flake to what was switched to: the pinned
// release in flake.nix, and the lock file.
func (u *Updater) localSwitched(c UpdatesConfig) error {
	u.mu.Lock()
	lc := u.local
	u.local = nil
	u.mu.Unlock()
	m := c.managed
	if lc != nil && lc.pin != "" {
		p := filepath.Join(m.Flake, "flake.nix")
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b = pinLine.ReplaceAll(b, []byte("${1}"+lc.pin+"${3}"))
		if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
			return err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return err
		}
	}
	return u.runArgv(evalTimeout, nil, u.throttled(m, "", u.Nix, nixArgs("flake", "lock", "path:"+m.Flake)...))
}

// pinOverride returns the --override-input that evaluates the flake at
// release pin, nil when it already is or doesn't pin one.
func pinOverride(flake, pin string) ([]string, error) {
	if pin == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(flake, "flake.nix"))
	if err != nil {
		return nil, err
	}
	m := pinLine.FindSubmatch(b)
	if m == nil {
		return nil, nil
	}
	if string(m[2]) == pin {
		return []string{}, nil
	}
	return []string{"--override-input", "pocketnix", "github:nickolaj-jepsen/pocketnix/v" + pin}, nil
}

// flakeStamp fingerprints the flake's files, so an apply can tell that the
// flake is still what its check evaluated.
func flakeStamp(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			fmt.Fprintf(h, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// plan dry-runs realising installables, as user (empty: root).
func (u *Updater) plan(m *ManagedConfig, user string, installables ...string) (protocol.Plan, error) {
	plan := protocol.Plan{Builds: []string{}}
	args := nixArgs(append([]string{"build", "--dry-run", "--no-link"}, installables...)...)
	_, report, err := u.capture(evalTimeout, u.throttled(m, user, u.Nix, args...))
	if err != nil {
		return plan, err
	}
	toBuild, download := parseDryRun(report)
	plan.Download = download
	if len(toBuild) == 0 {
		return plan, nil
	}
	drvs, err := u.showDerivations(toBuild...)
	if err != nil {
		return plan, err
	}
	for _, d := range toBuild {
		if drvs[d].compiles {
			plan.Builds = append(plan.Builds, drvName(d))
		}
	}
	slices.Sort(plan.Builds)
	return plan, nil
}

var (
	dryRunBuilt   = regexp.MustCompile(`^(this derivation|these \d+ derivations) will be built:$`)
	dryRunFetched = regexp.MustCompile(`^(this path|these \d+ paths) will be fetched \(([\d.]+) ([KMGT]?i?B) download`)
)

// parseDryRun reads `nix build --dry-run`'s report: the derivations it
// would build, and the bytes it would download.
func parseDryRun(out string) (drvs []string, download int64) {
	inBuilt := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "  ") {
			if p := strings.TrimSpace(line); inBuilt && strings.HasSuffix(p, ".drv") {
				drvs = append(drvs, p)
			}
			continue
		}
		inBuilt = dryRunBuilt.MatchString(line)
		if m := dryRunFetched.FindStringSubmatch(line); m != nil {
			download = parseSize(m[2], m[3])
		}
	}
	return drvs, download
}

func parseSize(n, unit string) int64 {
	f, _ := strconv.ParseFloat(n, 64)
	shift := map[string]uint{"B": 0, "KiB": 10, "MiB": 20, "GiB": 30, "TiB": 40}[unit]
	return int64(f * float64(int64(1)<<shift))
}

// drvName is a derivation's name: its store path without hash and ".drv".
func drvName(drv string) string {
	base := strings.TrimSuffix(filepath.Base(drv), ".drv")
	if _, name, ok := strings.Cut(base, "-"); ok {
		return name
	}
	return base
}

type drvInfo struct {
	out string
	// compiles is a derivation that builds a package from source, not
	// configuration (writeText, runCommand and their kin).
	compiles bool
}

// showDerivations reads derivations' out paths and what they build.
func (u *Updater) showDerivations(drvs ...string) (map[string]drvInfo, error) {
	info := map[string]drvInfo{}
	for chunk := range slices.Chunk(drvs, 200) {
		out, _, err := u.capture(envTimeout, append([]string{u.Nix}, nixArgs(append([]string{"derivation", "show"}, chunk...)...)...))
		if err != nil {
			return nil, err
		}
		var shown struct {
			Derivations map[string]json.RawMessage `json:"derivations"`
		}
		all := map[string]json.RawMessage{}
		// Nix 2.33 wraps the map and drops the store directory from its keys.
		if err := json.Unmarshal([]byte(out), &shown); err == nil && shown.Derivations != nil {
			all = shown.Derivations
		} else if err := json.Unmarshal([]byte(out), &all); err != nil {
			return nil, fmt.Errorf("nix derivation show: %w", err)
		}
		for k, raw := range all {
			var d struct {
				Env     map[string]string `json:"env"`
				Outputs map[string]struct {
					Path string `json:"path"`
				} `json:"outputs"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, fmt.Errorf("nix derivation show %s: %w", k, err)
			}
			info[u.storePathOf(k)] = drvInfo{out: u.storePathOf(d.Outputs["out"].Path), compiles: compiles(d.Env)}
		}
	}
	return info, nil
}

func (u *Updater) storePathOf(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(u.StoreDir, p)
}

// compiles tells a package built from source from a derivation that only
// writes configuration: those have no source, or say they're cheap.
func compiles(env map[string]string) bool {
	attrs := env
	if j, ok := env["__json"]; ok {
		var structured map[string]any
		if json.Unmarshal([]byte(j), &structured) == nil {
			attrs = map[string]string{}
			for k, v := range structured {
				if b, ok := v.(bool); ok {
					attrs[k] = map[bool]string{true: "1", false: ""}[b]
				} else if v != nil {
					attrs[k] = fmt.Sprint(v)
				}
			}
		}
	}
	if attrs["preferLocalBuild"] == "1" {
		return false
	}
	if v, ok := attrs["allowSubstitutes"]; ok && v == "" {
		return false
	}
	return attrs["src"] != "" || attrs["srcs"] != ""
}

func nixArgs(args ...string) []string {
	return append([]string{"--extra-experimental-features", "nix-command flakes"}, args...)
}

// throttled is the command line running name under the managed guest's
// CPU quota, as user when set; without systemd-run (tests) it's name itself.
func (u *Updater) throttled(m *ManagedConfig, user, name string, args ...string) []string {
	if u.SystemdRun == "" {
		return append([]string{name}, args...)
	}
	run := []string{u.SystemdRun, "--pipe", "--wait", "--collect", "--quiet", "--service-type=exec", "-p", "Nice=19", "-p", "IOSchedulingClass=idle"}
	if m != nil && m.CPUQuota != "" {
		run = append(run, "-p", "CPUQuota="+m.CPUQuota)
	}
	if user != "" {
		run = append(run, "--uid="+user)
	} else {
		run = append(run, "-E", "HOME=/root")
	}
	return append(append(run, "--", name), args...)
}

// capture runs a command and returns its stdout and stderr, logging stderr
// only when it fails.
func (u *Updater) capture(timeout time.Duration, argv []string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(u.opContext(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
		for _, l := range lines[max(0, len(lines)-10):] {
			u.logf("%s", l)
		}
		return "", "", fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
	}
	return out.String(), errb.String(), nil
}

// runArgv is runCmd for a command line.
func (u *Updater) runArgv(timeout time.Duration, out io.Writer, argv []string) error {
	return u.runCmd(timeout, out, nil, argv[0], argv[1:]...)
}

// block records what an operation stopped short of building.
func (u *Updater) block(p protocol.Plan) {
	u.mu.Lock()
	u.blocked = &p
	u.mu.Unlock()
}
