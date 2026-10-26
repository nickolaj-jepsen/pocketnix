# pocketnix

NixOS as a real host on your Android phone. pocketnix boots a NixOS disk image
under the Android Virtualization Framework (pKVM), with no root: an Android app
to run it and use its terminal, a NixOS module for the guest side, and a CLI
that provisions a phone over adb.

Early work in progress; nothing boots yet. See [docs/design.md](docs/design.md)
for what it will be.

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
