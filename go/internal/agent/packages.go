package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// A managed guest's packages live in the session user's Nix profile, as
// `nixpkgs#<attr>`: the module pins the registry's nixpkgs to the system's.

var attrName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_'+-]*(\.[A-Za-z_][A-Za-z0-9_'+-]*)*$`)

// AddPackages installs nixpkgs attributes into the user's profile, in the
// background. It stops short of building on the phone unless allowBuilds.
func (u *Updater) AddPackages(attrs []string, allowBuilds bool) error {
	if err := u.checkPackages(attrs); err != nil {
		return err
	}
	return u.start(protocol.UpdateInstalling, func(c UpdatesConfig) error {
		have, err := u.profileElements(c, "")
		if err != nil {
			return err
		}
		attrs := slices.DeleteFunc(slices.Clone(attrs), func(a string) bool { _, ok := have[a]; return ok })
		if len(attrs) == 0 {
			u.refreshPackages(c)
			return nil
		}
		inst := installables(attrs)
		user := profileUser(c)
		plan, err := u.plan(c.managed, user, inst...)
		if err != nil {
			return err
		}
		if len(plan.Builds) > 0 && !allowBuilds {
			u.block(plan)
			return fmt.Errorf("would build on the phone: %s", strings.Join(plan.Builds, ", "))
		}
		u.logf("adding %s", strings.Join(attrs, ", "))
		err = u.runArgv(fetchTimeout, nil, u.throttled(c.managed, user, u.Nix, nixArgs(append([]string{"profile", "add"}, inst...)...)...))
		u.refreshPackages(c)
		return err
	})
}

// RemovePackages removes nixpkgs attributes from the user's profile, in the
// background. Attributes it doesn't hold are ignored.
func (u *Updater) RemovePackages(attrs []string) error {
	if err := u.checkPackages(attrs); err != nil {
		return err
	}
	return u.start(protocol.UpdateInstalling, func(c UpdatesConfig) error {
		elems, err := u.profileElements(c, "")
		if err != nil {
			return err
		}
		var names []string
		for _, a := range attrs {
			if n, ok := elems[a]; ok {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			u.logf("removing %s", strings.Join(attrs, ", "))
			err = u.runArgv(envTimeout, nil, u.throttled(c.managed, profileUser(c), u.Nix, nixArgs(append([]string{"profile", "remove"}, names...)...)...))
		}
		u.refreshPackages(c)
		return err
	})
}

// RefreshPackages rereads the user's profile for [protocol.Updates.Packages].
func (u *Updater) RefreshPackages() {
	c := u.config()
	if c.managed == nil {
		return
	}
	u.refreshPackages(c)
	u.changed()
}

func (u *Updater) refreshPackages(c UpdatesConfig) {
	elems, err := u.profileElements(c, "")
	if err != nil {
		u.logf("listing packages: %v", err)
		return
	}
	attrs := make([]string, 0, len(elems))
	for a := range elems {
		attrs = append(attrs, a)
	}
	slices.Sort(attrs)
	u.mu.Lock()
	u.packages = attrs
	u.mu.Unlock()
}

// Run fetches a nixpkgs attribute into the ad-hoc profile, in the
// background, and reports how to start it in [protocol.Updates.Run]. It
// stops short of building on the phone unless allowBuilds.
func (u *Updater) Run(attr string, allowBuilds bool) error {
	if err := u.checkPackages([]string{attr}); err != nil {
		return err
	}
	return u.start(protocol.UpdateInstalling, func(c UpdatesConfig) error {
		u.mu.Lock()
		u.run = nil
		u.mu.Unlock()
		inst := installables([]string{attr})
		user := profileUser(c)
		plan, err := u.plan(c.managed, user, inst...)
		if err != nil {
			return err
		}
		if len(plan.Builds) > 0 && !allowBuilds {
			u.block(plan)
			return fmt.Errorf("would build on the phone: %s", strings.Join(plan.Builds, ", "))
		}
		u.logf("fetching %s", attr)
		out, _, err := u.capture(fetchTimeout, u.throttled(c.managed, user, u.Nix, nixArgs("build", "--no-link", "--print-out-paths", inst[0])...))
		if err != nil {
			return err
		}
		paths := strings.Fields(out)
		if len(paths) == 0 {
			return fmt.Errorf("%s built nothing", attr)
		}
		if err := u.addAdhoc(c, user, paths); err != nil {
			return err
		}
		res := &protocol.RunResult{Package: attr, Apps: desktopIDs(paths)}
		if len(res.Apps) == 0 {
			main, _, _ := u.capture(envTimeout, u.throttled(c.managed, user, u.Nix, nixArgs("eval", "--raw", inst[0]+".meta.mainProgram")...))
			res.Command = mainCommand(paths, strings.TrimSpace(main), attr)
			if res.Command == "" {
				return fmt.Errorf("%s has neither a desktop file nor a program", attr)
			}
		}
		u.mu.Lock()
		u.run = res
		u.mu.Unlock()
		return nil
	})
}

// addAdhoc puts paths into the ad-hoc profile, which the session's PATH and
// XDG_DATA_DIRS include, unless it holds them already.
func (u *Updater) addAdhoc(c UpdatesConfig, user string, paths []string) error {
	home, owner, err := u.userHome(c)
	if err != nil {
		return err
	}
	profile := filepath.Join(home, c.managed.AdhocProfile)
	dir, err := configuredDir(home, filepath.Dir(profile), true, owner)
	if err != nil {
		return err
	}
	dir.Close()
	held, err := u.profileStorePaths(c, profile)
	if err != nil {
		return err
	}
	var add []string
	for _, p := range paths {
		if !held[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return nil
	}
	return u.runArgv(envTimeout, nil, u.throttled(c.managed, user, u.Nix, nixArgs(append([]string{"profile", "add", "--profile", profile}, add...)...)...))
}

type profileList struct {
	Elements map[string]struct {
		AttrPath    string   `json:"attrPath"`
		OriginalURL string   `json:"originalUrl"`
		StorePaths  []string `json:"storePaths"`
	} `json:"elements"`
}

func (u *Updater) listProfile(c UpdatesConfig, profile string) (profileList, error) {
	var l profileList
	args := []string{"profile", "list", "--json"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	out, _, err := u.capture(envTimeout, u.throttled(c.managed, profileUser(c), u.Nix, nixArgs(args...)...))
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return l, fmt.Errorf("nix profile list: %w", err)
	}
	return l, nil
}

// profileElements maps the nixpkgs attributes in the user's profile to their
// element names.
func (u *Updater) profileElements(c UpdatesConfig, profile string) (map[string]string, error) {
	l, err := u.listProfile(c, profile)
	if err != nil {
		return nil, err
	}
	elems := map[string]string{}
	for name, e := range l.Elements {
		if e.OriginalURL != "flake:nixpkgs" {
			continue
		}
		// legacyPackages.<system>.<attr>
		parts := strings.SplitN(e.AttrPath, ".", 3)
		if len(parts) == 3 {
			elems[parts[2]] = name
		}
	}
	return elems, nil
}

func (u *Updater) profileStorePaths(c UpdatesConfig, profile string) (map[string]bool, error) {
	if _, err := os.Lstat(profile); errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	l, err := u.listProfile(c, profile)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, e := range l.Elements {
		for _, p := range e.StorePaths {
			held[p] = true
		}
	}
	return held, nil
}

func (u *Updater) checkPackages(attrs []string) error {
	if u.config().managed == nil {
		return errors.New("this guest isn't managed by the phone")
	}
	if len(attrs) == 0 {
		return errors.New("no packages named")
	}
	for _, a := range attrs {
		if !attrName.MatchString(a) {
			return fmt.Errorf("%q is not a nixpkgs attribute", a)
		}
	}
	return nil
}

func installables(attrs []string) []string {
	inst := make([]string, len(attrs))
	for i, a := range attrs {
		inst[i] = "nixpkgs#" + a
	}
	return inst
}

// userHome is the session user's home, or Home when set, and who should
// own what the agent makes there.
func (u *Updater) userHome(c UpdatesConfig) (string, *fileOwner, error) {
	if u.Home != "" {
		return u.Home, nil, nil
	}
	su, err := sessionUser(c.user)
	if err != nil {
		return "", nil, err
	}
	var owner *fileOwner
	if os.Getuid() == 0 && su.uid != 0 {
		owner = &fileOwner{su.uid, su.gid}
	}
	return su.home, owner, nil
}

// profileUser is who owns the packages: the session user, or root ("").
func profileUser(c UpdatesConfig) string {
	if c.user == "root" || os.Getuid() != 0 {
		return ""
	}
	return c.user
}

// desktopIDs lists the desktop files paths bring, by id.
func desktopIDs(paths []string) []string {
	var ids []string
	for _, p := range paths {
		entries, _ := os.ReadDir(filepath.Join(p, "share", "applications"))
		for _, e := range entries {
			if id, ok := strings.CutSuffix(e.Name(), ".desktop"); ok {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// mainCommand finds the program a package runs: its mainProgram, else one
// named for the attribute, else its only one.
func mainCommand(paths []string, mainProgram, attr string) string {
	names := []string{mainProgram, attr[strings.LastIndexByte(attr, '.')+1:]}
	for _, n := range names {
		for _, p := range paths {
			if n == "" {
				continue
			}
			bin := filepath.Join(p, "bin", n)
			if fi, err := os.Stat(bin); err == nil && fi.Mode().IsRegular() {
				return bin
			}
		}
	}
	var only []string
	for _, p := range paths {
		entries, _ := os.ReadDir(filepath.Join(p, "bin"))
		for _, e := range entries {
			only = append(only, filepath.Join(p, "bin", e.Name()))
		}
	}
	if len(only) == 1 {
		return only[0]
	}
	return ""
}
