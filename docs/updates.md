# Updates

The phone never evaluates or builds Nix: a real config needs more memory and
time than the guest has. Every update arrives as a finished system path, from
a build host, a URL your CI publishes, or both. The app's System menu checks,
updates, rolls back and lists generations.

```mermaid
flowchart TD
  check[Check for update] --> bh{build host set?}
  bh -- yes --> build[ssh: build host evaluates and builds]
  build -- ok --> fetch
  build -- unreachable --> pub
  bh -- no --> pub{published URL set?}
  pub -- yes --> url[fetch the path from the URL]
  url --> fetch[substitute from caches,<br/>copy the rest over ssh]
  fetch --> verify[name and hostname match this host?]
  verify --> switch[nix-env --set + switch-to-configuration]
```

## Sources

```nix
pocketnix.guest.updates = {
  # Builds <flake>#nixosConfigurations.<hostName> on the build host.
  buildHost = {host = "pocketnix-build@desktop"; flake = "github:alice/nixos";};
  # Fallback: a URL whose body is the toplevel store path, in a cache the guest trusts.
  published.url = "https://ci.example.com/phone/toplevel";
  checkInterval = "6h"; # background checks only report; null disables them
};
# The agent ssh's in BatchMode with the guest's host key: the build host must be known.
programs.ssh.knownHosts.desktop.publicKey = "ssh-ed25519 ...";
```

- Build host: any machine with Nix that accepts the guest's ssh host key.
  It runs `nix build --refresh --print-out-paths <installable>`. The guest
  then substitutes what its caches have and copies only the rest from the
  build host.
- Published path: CI builds the system, pushes it to a binary cache, and
  publishes the toplevel path as one line of text at an https URL. The guest
  fetches it with `--max-jobs 0`, substituting only. No build host needs to be
  up.
- A check asks the build host first and falls back to the published path.
- An answer must be named `nixos-system-<host>-...`, and the fetched system's
  `etc/hostname` must match, before it becomes a generation.

## The build host's side

`nixosModules.buildHost` adds an untrusted user whose ssh key, per guest, can
only build that guest's system and serve the copy. A stolen phone gets no
shell on your desktop.

```nix
imports = [pocketnix.nixosModules.buildHost];
pocketnix.buildHost = {
  enable = true;
  guests.phone = {
    publicKey = "ssh-ed25519 ..."; # the guest's /etc/ssh/ssh_host_ed25519_key.pub
    installable = "github:alice/nixos#nixosConfigurations.phone.config.system.build.toplevel";
  };
};
```

Inject the guest's host key at provision time so you know its public key in
advance (see [Configuration](configuration.md#secrets)).

## Applying

- Nothing applies on its own. Background checks show "update available"; you
  tap Update.
- On a metered network the app asks first, and checks skip while the phone is
  offline.
- The switch runs in its own unit (`systemd-run`), as nixos-rebuild does, so
  closing a tab can't interrupt it. A switch reloads the agent rather than
  restarting it, so open tabs survive.
- A new agent binary takes over at the next boot. The app offers a reboot
  when the running system isn't the booted one.

## Rollback

Generations come from the system profile. Rolling back switches to an older
one (`nix-env --switch-generation`, then `switch`).

## A generation that won't boot

- systemd-boot doesn't wait (the module's `boot.loader.timeout`).
- "Start with boot menu" in the app taps a key on the console during boot, so
  systemd-boot shows its menu in the console tab. Pick an older generation
  there.
- The app offers this when the agent is late to connect.

## Pushing from a desktop

`nixos-rebuild --target-host` works as usual, over the network or over USB
with `pocketnix ssh-proxy` as the ssh `ProxyCommand`:

```
Host phone-usb
  HostKeyAlias phone
  ProxyCommand pocketnix ssh-proxy --port 2022
```

```bash
nixos-rebuild switch --flake .#phone --target-host alice@phone-usb --sudo
```
