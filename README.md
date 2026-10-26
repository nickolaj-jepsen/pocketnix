# pocketnix

NixOS as a real host on your Android phone. pocketnix boots a NixOS disk image
under the Android Virtualization Framework (pKVM), with no root: an Android app
to run it and use its terminal, a NixOS module for the guest side, and a CLI
that provisions a phone over adb.

Early work in progress. See [docs/design.md](docs/design.md) for what it will
be, and [example/configuration.nix](example/configuration.nix) for a guest.

## Use

Add `pocketnix.nixosModules.default` to an `aarch64-linux` NixOS configuration
and set `pocketnix.guest.enable = true` (see the module for the terminal's
profiles, extra keys, theme and port forwards). Then, with the phone on adb:

```bash
nix run github:nickolaj-jepsen/pocketnix#provision -- \
  --image .#nixosConfigurations.<host>.config.system.build.pocketnixImage.x86_64-linux \
  --inject /etc/ssh/ssh_host_ed25519_key=./host_key
```

The phone never evaluates or builds. Updates come finished, from a build host
over ssh, or from a store path your CI publishes, and the app's System menu
updates, rolls back and lists generations:

```nix
pocketnix.guest.updates = {
  # Builds `<flake>#nixosConfigurations.<hostName>`; trusts the guest's host key.
  buildHost = { host = "alice@desktop"; flake = "github:alice/nixos"; };
  # Fallback: a URL whose body is the toplevel path, in a cache the guest trusts.
  published.url = "https://ci.example.com/phone/toplevel";
};
programs.ssh.knownHosts.desktop.publicKey = "ssh-ed25519 …";
```

To push from a desktop over USB instead, use `pocketnix ssh-proxy` as an ssh
ProxyCommand (`pocketnix ssh-proxy -h`).

## Requirements

- A phone with AVF and non-protected VM support (tested on a Pixel 8 Pro running
  GrapheneOS, Android 17).
- adb, once, to grant the app the two AVF permissions.

## Building

```bash
nix develop
./gradlew assembleDebug
nix build .#pocketnix
```

## License

GPL-3.0-or-later. Parts of the AVF engine are ported from
[Podroid](https://github.com/ExTV/Podroid) (GPL-2.0-or-later); the terminal
libraries come from [Termux](https://github.com/termux/termux-app) (Apache-2.0).
