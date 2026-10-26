package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/image"
)

const provisionUsage = `usage: pocketnix provision --image <file|installable> [flags]

Installs the app, grants its AVF permissions, pushes a disk image into the
app's storage and starts the VM.

flags:
  --image <file|installable>  the disk image, or a Nix installable that builds it
                              (e.g. .#nixosConfigurations.phone.config.system.build.pocketnixImage.x86_64-linux)
  --apk <file>                APK to install (default: the release APK of this CLI's version)
  --debug                     target the debug build (dev.nickolaj.pocketnix.debug)
  --inject <guest>=<file|->   write a file into the image's root filesystem; repeatable
  --mode <octal>              mode of the preceding --inject (default 0600)
  --replace                   replace the phone's existing disk image, destroying the guest's disk
  --no-network                start the VM without networking
  --no-start                  don't start the VM
  --serial <serial>           adb device to use (default: adb's only device)
`

const (
	releasePackage = "dev.nickolaj.pocketnix"
	activityClass  = "dev.nickolaj.pocketnix.ControlActivity"
	actionStart    = "dev.nickolaj.pocketnix.action.START"
	// chunkSize bounds one adb push: bulk transfers over USB drop the device
	// after ~100–200 MB.
	chunkSize   = 32 << 20
	deviceChunk = "/data/local/tmp/pocketnix.chunk"
)

var avfPermissions = []string{
	"android.permission.MANAGE_VIRTUAL_MACHINE",
	"android.permission.USE_CUSTOM_VIRTUAL_MACHINE",
}

type provisionOpts struct {
	image, apk, serial        string
	debug, replace, noNetwork bool
	noStart                   bool
	injects                   []image.Injection
	stdin                     io.Reader
	log                       io.Writer
	resolveInstallable        func(string) (string, error)
	downloadRelease           func(version, dst string) error
}

func parseProvision(args []string) (*provisionOpts, error) {
	o := &provisionOpts{stdin: os.Stdin, log: os.Stderr, resolveInstallable: nixBuild, downloadRelease: downloadRelease}
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, provisionUsage) }
	fs.StringVar(&o.image, "image", "", "")
	fs.StringVar(&o.apk, "apk", "", "")
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.replace, "replace", false, "")
	fs.BoolVar(&o.noNetwork, "no-network", false, "")
	fs.BoolVar(&o.noStart, "no-start", false, "")
	fs.Func("inject", "", func(v string) error {
		guest, src, ok := strings.Cut(v, "=")
		if !ok || guest == "" || src == "" {
			return fmt.Errorf("--inject %q: want <guest path>=<file|->", v)
		}
		o.injects = append(o.injects, image.Injection{Guest: guest, Source: src, Mode: 0o600})
		return nil
	})
	fs.Func("mode", "", func(v string) error {
		if len(o.injects) == 0 {
			return errors.New("--mode must follow an --inject")
		}
		m, err := strconv.ParseUint(v, 8, 32)
		if err != nil || m > 0o7777 {
			return fmt.Errorf("--mode %q: want an octal mode", v)
		}
		o.injects[len(o.injects)-1].Mode = os.FileMode(m)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.image == "" || fs.NArg() > 0 {
		fs.Usage()
		return nil, errUsage
	}
	stdinUses := 0
	for _, in := range o.injects {
		if in.Source == "-" {
			stdinUses++
		}
	}
	if stdinUses > 1 {
		return nil, errors.New("only one --inject can read stdin")
	}
	return o, nil
}

func (o *provisionOpts) pkg() string {
	if o.debug {
		return releasePackage + ".debug"
	}
	return releasePackage
}

func (o *provisionOpts) logf(format string, args ...any) {
	fmt.Fprintf(o.log, "provision: "+format+"\n", args...)
}

func runProvision(args []string) error {
	o, err := parseProvision(args)
	if err != nil {
		return err
	}
	return o.run()
}

func (o *provisionOpts) run() error {
	dev := adb.Device{Serial: o.serial}
	if state, err := dev.State(); err != nil || state != "device" {
		return fmt.Errorf("no usable adb device (state %q): %v", state, err)
	}

	img, cleanup, err := o.prepareImage()
	if err != nil {
		return err
	}
	defer cleanup()

	apk := o.apk
	if apk == "" {
		tmp, err := os.MkdirTemp("", "pocketnix-apk-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		apk = filepath.Join(tmp, "pocketnix.apk")
		o.logf("downloading the release APK for %s", version)
		if err := o.downloadRelease(version, apk); err != nil {
			return fmt.Errorf("%w (pass --apk to install a local build)", err)
		}
	}
	o.logf("installing %s", apk)
	if _, err := dev.Run("install", "-r", "-g", apk); err != nil {
		return fmt.Errorf("%w\nA signature mismatch needs an uninstall first, which deletes the VM's disk image; do that by hand if you mean it", err)
	}
	for _, p := range avfPermissions {
		if _, err := dev.Shell("pm grant " + o.pkg() + " " + p); err != nil {
			return err
		}
	}
	o.logf("granted %s", strings.Join(avfPermissions, ", "))

	prov := provider{dev: dev, authority: o.pkg() + ".provision"}
	st, err := prov.call("status", "")
	if err != nil {
		return err
	}
	if st["running"] == "true" {
		return errors.New("the VM is running; stop it in the app first")
	}
	if st["disk"] == "true" && !o.replace {
		return errors.New("the phone already has a disk image; --replace destroys it and everything in the guest")
	}
	if err := o.upload(dev, prov, img); err != nil {
		return err
	}
	commitArg := ""
	if o.replace {
		commitArg = "replace"
	}
	if _, err := prov.call("commit", commitArg); err != nil {
		return err
	}
	o.logf("disk image in place")

	if o.noStart {
		return nil
	}
	_, err = dev.Shell(fmt.Sprintf("am start -n %s/%s -a %s --ez network %t", o.pkg(), activityClass, actionStart, !o.noNetwork))
	if err == nil {
		o.logf("started the VM (network %t)", !o.noNetwork)
	}
	return err
}

// prepareImage resolves the image and, with injections, makes an edited copy.
func (o *provisionOpts) prepareImage() (string, func(), error) {
	img, err := o.resolveImage()
	if err != nil {
		return "", nil, err
	}
	if len(o.injects) == 0 {
		return img, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "pocketnix-image-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	cp := filepath.Join(tmp, "disk.img")
	o.logf("copying %s to inject %d file(s)", img, len(o.injects))
	if err := image.CopySparse(cp, img); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := image.Inject(cp, o.injects, o.stdin); err != nil {
		cleanup()
		return "", nil, err
	}
	return cp, cleanup, nil
}

// resolveImage accepts an image file, a directory holding one (a Nix build
// result), or an installable to build.
func (o *provisionOpts) resolveImage() (string, error) {
	p := o.image
	if _, err := os.Stat(p); err != nil {
		o.logf("building %s", p)
		if p, err = o.resolveInstallable(p); err != nil {
			return "", err
		}
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return p, nil
	}
	var found []string
	for _, pat := range []string{"*.raw", "*.img"} {
		m, _ := filepath.Glob(filepath.Join(p, pat))
		found = append(found, m...)
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s: want exactly one *.raw or *.img image, found %d", p, len(found))
	}
	return found[0], nil
}

func nixBuild(installable string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("nix", "build", "--no-link", "--print-out-paths", installable)
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nix build %s: %w", installable, err)
	}
	return strings.TrimSpace(out.String()), nil
}

func downloadRelease(version, dst string) error {
	if version == "dev" || version == "0.0.0" {
		return fmt.Errorf("this CLI (%s) has no release APK", version)
	}
	url := fmt.Sprintf("https://github.com/nickolaj-jepsen/pocketnix/releases/download/v%s/pocketnix-%s.apk", version, version)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// upload sends the image in chunks, skipping all-zero ones: the app sizes
// the file sparse first, and most of an image is empty ESP and free space.
func (o *provisionOpts) upload(dev adb.Device, prov provider, img string) error {
	f, err := os.Open(img)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := prov.call("begin", strconv.FormatInt(fi.Size(), 10)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "pocketnix-chunk-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	buf := make([]byte, chunkSize)
	zero := make([]byte, chunkSize)
	var sent int64
	for off := int64(0); off < fi.Size(); off += chunkSize {
		n, err := f.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			return err
		}
		if bytes.Equal(buf[:n], zero[:n]) {
			continue
		}
		if err := os.WriteFile(tmp.Name(), buf[:n], 0o600); err != nil {
			return err
		}
		if _, err := dev.Run("push", tmp.Name(), deviceChunk); err != nil {
			return err
		}
		uri := fmt.Sprintf("content://%s/disk?offset=%d", prov.authority, off)
		if _, err := dev.Shell("content write --uri " + adb.ShellQuote(uri) + " < " + deviceChunk); err != nil {
			return err
		}
		sent += int64(n)
		o.logf("pushed %d/%d MiB", (off+int64(n))>>20, fi.Size()>>20)
	}
	dev.Shell("rm -f " + deviceChunk)
	o.logf("pushed %d MiB of data (the rest is zeros)", sent>>20)
	return nil
}

// provider is the app's provisioning ContentProvider, which only the adb
// shell user may call.
type provider struct {
	dev       adb.Device
	authority string
}

var bundleField = regexp.MustCompile(`(\w+)=([^,}]*)`)

// call runs a provider method and returns its result bundle; a bundle with
// an "error" key is returned as an error.
func (p provider) call(method, arg string) (map[string]string, error) {
	cmd := fmt.Sprintf("content call --uri content://%s --method %s", p.authority, method)
	if arg != "" {
		cmd += " --arg " + adb.ShellQuote(arg)
	}
	out, err := p.dev.Shell(cmd)
	if err != nil {
		return nil, err
	}
	i := strings.Index(out, "Bundle[")
	if i < 0 {
		return nil, fmt.Errorf("provider %s: unexpected output %q", method, out)
	}
	bundle := out[i:]
	// An error message may hold commas; it runs to the bundle's end.
	if j := strings.Index(bundle, "error="); j >= 0 {
		msg := bundle[j+len("error="):]
		if k := strings.LastIndex(msg, "}]"); k >= 0 {
			msg = msg[:k]
		}
		return nil, fmt.Errorf("provider %s: %s", method, strings.TrimSpace(msg))
	}
	res := map[string]string{}
	for _, m := range bundleField.FindAllStringSubmatch(bundle, -1) {
		res[m[1]] = strings.TrimSpace(m[2])
	}
	return res, nil
}
