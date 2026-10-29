# Configuration

Two places hold configuration:

- The NixOS module (`pocketnix.guest.*`) owns what changes with the system.
  The agent sends it to the app on connect, and a `switch` applies it live.
- The app's Settings own what must be known before boot (the VM's hardware
  and network) and what only the phone can name (which Android folders to
  share).

Every option, with its default and description, is in
[Options](options.md), generated from [`nix/module.nix`](../nix/module.nix).
This page shows them by topic.

## Terminal

```nix
pocketnix.guest = {
  user = "alice";
  terminal = {
    # Kinds of tab, in the "+" menu. Each runs `$SHELL -l [-c command]`.
    profiles = {
      shell = {};
      tmux.command = "tmux new-session -A -s main";
    };
    defaultProfile = "shell";

    # Rows above the keyboard, in Termux's extra-keys names. Swipe up for `popup`.
    extraKeys = [
      ["ESC" {key = "/"; popup = "\\";} "HOME" "UP" "END" "PGUP"]
      ["TAB" "CTRL" "ALT" "LEFT" "DOWN" "RIGHT" "PGDN"]
    ];

    # Termux color names: foreground, background, cursor, color0..color15.
    theme.colors = {background = "#1e1e2e"; foreground = "#cdd6f4";};
    theme.fontFamily = {package = pkgs.jetbrains-mono; family = "JetBrains Mono";};

    # The command palette lists these above the shell history.
    snippets."Follow the journal" = "journalctl -f";
  };
};
```

On the phone:

- Volume-down held is Ctrl, volume-up held is Termux's Fn (W/A/S/D arrows,
  1 to 0 for F1 to F10).
- The composer sends text through the phone keyboard with autocorrect and
  dictation, as a bracketed paste.
- A bell, OSC 9 or OSC 777 notification in a background tab badges it and
  posts an Android notification.

## Port forwards

```nix
pocketnix.guest.forwards = {
  ssh = {guestPort = 22; hostPort = 2022;};
  web = {guestPort = 8080; lan = true;};          # every interface, not just 127.0.0.1
  mosh = {guestPort = 60001; protocol = "udp";};
};
```

- Forwards share the phone's ports with every other app. One that can't
  listen shows in the app and is retried.
- A UDP forward keeps one stream per phone-side client, dropped once idle.

## Android integration

All on by default.

| In the guest | On Android |
|---|---|
| `notify-send`, any libnotify program | notification, with actions |
| `xdg-open <url>`, `gio open` | opens in an Android app |
| `xdg-open <file>` (under `files.root`) | opens through the Files provider |
| `pocketnix-guest copy` / `paste`, OSC 52 | the clipboard |
| `pocketnix-guest share <file>` | the share sheet |
| `pocketnix-guest pick` / `save`, the FileChooser portal | the document pickers |
| `files.root` | the Files app and every file picker |
| `share.directory` | where the share sheet drops files |

```nix
pocketnix.guest = {
  share.command = "notify-send shared"; # runs on each shared file, its path last
  files.root = "/home/alice/shared";
  integrations.xdgOpen = false;         # keep xdg-utils' own
};
```

From the background, Android won't let an app open an activity, so `open` and
`share` become a notification to tap.

## Shared Android folders

Pick folders in the app's settings. They mount in the guest under
`hostFolders.mountPoint`.

- Only the user can enter them, not even root.
- While the app is gone, operations fail with EIO and the mount stays.
- No symlinks; permission and owner changes are accepted and ignored.

## Phone state

The app tells the guest about the phone:

- systemd targets that are active while each holds:
  `pocketnix-online.target`, `pocketnix-unmetered.target`,
  `pocketnix-charging.target`, `pocketnix-power-save.target`. Bind units to them:

  ```nix
  systemd.services.backup = {
    bindsTo = ["pocketnix-unmetered.target" "pocketnix-charging.target"];
    after = ["pocketnix-unmetered.target" "pocketnix-charging.target"];
  };
  ```

- `pocketnix-guest host` and `/run/pocketnix/host.json` show the same state.
- The timezone follows the phone's while `time.timeZone` is null
  (`host.timeZone`).
- Without the app connected, the guest counts as offline and on battery.

## Power

- The VM runs until stopped. Android may deep-sleep the phone meanwhile.
- A logind sleep or idle inhibitor in the guest (`systemd-inhibit
  --what=sleep ...`) makes the app hold a wake lock, named in its notification.
  `power.wakeLocks.ignore` lists daemons to disregard; the app has a setting
  to refuse wake locks altogether.
- Free guest memory returns to Android through AVF's memory balloon.

## What the module trims on AVF

To keep the image small and idle wake-ups rare, the module's AVF settings
(under `avf.enable` in `nix/module.nix`) change some NixOS defaults with
`mkDefault`; set them back in your config. The ones you'll notice:

- No flake registry or `NIX_PATH` entry pinning nixpkgs to the system's.
- No NixOS manual, info or doc outputs (man pages stay), no
  `environment.defaultPackages`.
- zram swap on, systemd-oomd off.

## Other hypervisors

`pocketnix.guest.avf.enable = false` drops the AVF platform and keeps the
agent. This is how the NixOS tests run the guest under QEMU.

## Secrets

Write them into the image at provision time instead of the store:

```bash
pocketnix provision --image .#nixosConfigurations.phone \
  --inject /etc/ssh/ssh_host_ed25519_key=./host_key
```

With a known host key, agenix or sops-nix can decrypt on the first boot.
