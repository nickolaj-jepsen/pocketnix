# Getting started

## Requirements

- A phone whose AVF runs non-protected custom VMs. Tested on a Pixel 8 Pro with
  GrapheneOS (Android 17). The app's first screen lists anything missing.
- USB debugging on the phone, and `adb` on your machine (provisioning needs it
  once).
- Nix with flakes, on a machine that can build `aarch64-linux`:
  - an aarch64 Linux machine or remote builder, or
  - x86_64 with `boot.binfmt.emulatedSystems = ["aarch64-linux"];`. Most of the
    closure comes from cache.nixos.org; the agent builds under emulation.
- Free space on the phone for the disk (`--disk`, or set in the app).

## 1. Describe the guest

Add the module to an `aarch64-linux` NixOS configuration. A useful start, with
ssh through a port forward:

```nix
{pkgs, ...}: {
  pocketnix.guest = {
    enable = true;
    user = "alice"; # terminal tabs and the console log in as this user
    forwards.ssh = {guestPort = 22; hostPort = 2022;};
  };

  users.users.alice = {isNormalUser = true; extraGroups = ["wheel"];};
  security.sudo.wheelNeedsPassword = false;
  services.openssh = {enable = true; settings.PasswordAuthentication = false;};

  networking.hostName = "phone";
  system.stateVersion = "26.05";
}
```

[`example/configuration.nix`](../example/configuration.nix) is a fuller one,
and [Configuration](configuration.md) covers the rest.

## 2. Get the app

`provision` installs the release APK matching its own version from this
repo's GitHub releases. If its version has no release, or to run your own
build, build a debug APK and pass it in:

```bash
git clone https://github.com/nickolaj-jepsen/pocketnix && cd pocketnix
nix develop --command ./gradlew assembleDebug
# then add to provision: --debug --apk app/build/outputs/apk/debug/app-debug.apk
```

The debug app (`dev.nickolaj.pocketnix.debug`) installs next to the release
app with its own VM and disk.

## 3. Provision

With the phone on adb:

```bash
nix run github:nickolaj-jepsen/pocketnix#provision -- \
  --image .#nixosConfigurations.phone \
  --inject /etc/ssh/authorized_keys.d/alice=$HOME/.ssh/id_ed25519.pub --mode 0644
```

```mermaid
sequenceDiagram
  participant CLI as pocketnix provision
  participant Phone as phone (adb)
  CLI->>CLI: build the image with this machine's tools
  CLI->>CLI: copy it, write --inject files into the copy
  CLI->>Phone: install the APK
  CLI->>Phone: pm grant the AVF permissions
  CLI->>Phone: push the image in compressed chunks
  CLI->>Phone: start the VM
```

- `--inject <guest path>=<file>` puts secrets (ssh host keys for agenix,
  authorized keys) into the image without them entering the Nix store.
  Repeatable; `--mode` applies to the one before it.
- `--memory`, `--cpus one|all` and `--disk` set the VM's hardware, which the
  app's Settings can change later.
- An existing disk is never replaced without `--replace`, which destroys it.
- `pocketnix provision -h` lists every flag.

## 4. First boot

- The app opens the console tab and shows the boot. The first boot grows the
  root filesystem to fill the disk.
- Once the agent connects, a shell tab opens as `pocketnix.guest.user`.
- Start and stop the VM from the app, its notification, or the Quick Settings
  tile. Stop shuts the guest down cleanly.

## 5. Reach it from your desktop

Forwards listen on the phone's `127.0.0.1` (`lan = true` opens one to the
network). Over USB:

```bash
adb forward tcp:2022 tcp:2022
ssh -p 2022 alice@127.0.0.1
```

or use `pocketnix ssh-proxy --port 2022` as an ssh `ProxyCommand` (see
`pocketnix ssh-proxy -h`). A VPN in the guest, such as Tailscale, works too.

## Things to know

- Uninstalling the app, or clearing its data, deletes the VM's disk. Back up
  state you care about from inside the guest; the system itself rebuilds from
  your flake.
- The VM lives in the app's process. If the app crashes, the guest loses power.
- Only one networked VM can run on a phone at a time, across all apps.
- Don't build on the phone. Updates arrive pre-built ([Updates](updates.md)).
