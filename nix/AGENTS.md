# Traps in the module, the image and the NixOS tests

The repository's root `AGENTS.md` has the working rules and the traps
that cross directories.

## Traps (verified on a Pixel 8 Pro, GrapheneOS, Android 17)

- Stock nixpkgs kernels (6.1, 6.12, 6.18) return corrupted virtio-blk reads
  under Pixel pKVM (ext4 checksum errors, random SEGVs). Use Google's prebuilt
  guest kernel (`nix/avf-kernel`).
- Multi-vCPU (`match_host`) guests panic on MPAM without `arm64.nompam` on the
  kernel command line.
- u-boot looks for `/EFI/BOOT/BOOTAA64.EFI`, but `bootctl` (and so every
  `switch`) wants `/EFI/systemd/systemd-bootaa64.efi`; the ESP needs both.
- Taking a guest vCPU offline works, but bringing it back can fail under pKVM
  ("CPU4: died during early boot") until the VM restarts.
- Building the image on x86_64 with the guest's own repart tools fails; assemble
  it with the build host's tools but keep the guest's `systemdRepartFlags`
  (`--architecture`). Host-tool repart dereferences top-level symlink store
  paths, so rewrite those `CopyFiles=` lines to `MakeSymlinks=` (see `nix/module.nix`).
- First-boot setup must order like sd-image (`DefaultDependencies=false`, before
  `sysinit.target`), or it cycles with `nix-daemon.socket`.
- Google's kernel has no efivarfs but has `/sys/firmware/efi`: `bootctl status`
  (the version check of a plain `switch`) exits 1 without a word, while
  `bootctl --no-variables …` works. The module makes every switch install.
- logind grants `/dev/snd` only to sessions on a seat; the console on hvc0,
  tabs and the lingering user manager (PipeWire's) have none, so without the
  `audio` group PipeWire sees no card and only root can play.
- crosvm hands AAudio S32LE as 24-bit packed, so every sample misaligns into
  static: the module pins virtio_snd to S16LE.

## Traps (verified on the desktop)

- Google's AVF kernel has no ACPI: under QEMU it needs `-M virt,acpi=off`, or
  edk2 boots it with an empty device tree and it hangs silently.
- The Nix build sandbox has no `/dev/vhost-vsock`, so the NixOS test reaches the
  agent over TCP; `nix run .#boot-example` (outside the sandbox) covers vsock.
- The CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch
  order: in NixOS tests, connect by IP.
- The Nix-built QEMU breaks if the shell leaks an `LD_LIBRARY_PATH` from a
  newer glibc; `boot-example` unsets it.
- A NixOS test node that really switches (`switch`, not `test`) needs
  `boot.loader.grub.enable = false`: the test VM's GRUB can't install. Test
  VMs also boot without a system profile; create one before testing generations.
- A guest system for a test's later generation can't be named in that guest's
  own config (its path would depend on itself): build it with `extendModules`
  into `system.build`, and put it on another node.
- With a substituter that doesn't answer, Nix retries 5 times with backoff
  (~5 s) before giving up; the update log shows the warnings.
- systemd 260's autologin no longer prints "(automatic login)": a console
  script (`avf-boot`) waits for the shell prompt instead. Console lines ending
  in `\r` show up blank in Nix logs, and expect's `log_user 0` also stops
  `log_file`.
- `/boot` is mounted umask 0077: checking it in a guest needs root.
- Tabs have no login session, so without lingering there is no user manager
  and no `/run/user/<uid>/bus`; the module lingers the user, which brings the
  session bus up with no login (the NixOS test checks it).
- NixOS gives every unit, user units too, a minimal default `PATH` that
  replaces the user manager's: a unit that must find the profiles' programs
  (the compositor's `Exec`/`TryExec`) needs `enableDefaultPath = false`.
- NixOS leaves polkit off, and without it logind refuses every sleep
  inhibitor but root's ("Access denied"). With it, a user without a logind
  session (a tab, `runuser`, a unit's `User=`) still needs authentication
  for `inhibit-block-sleep`: `wakeLocks.enable` turns polkit on and allows
  `pocketnix.guest.user`. logind (systemd 260) does signal `BlockInhibited`
  and `NCurrentInhibitors` changes.
- `gui.enable` sets `nixpkgs.overlays`, which a NixOS test's read-only pkgs
  refuse: such a test needs `node.pkgsReadOnly = false`.
- NixOS test VMs default `virtualisation.qemu.enableSharedMemory` on, whose
  memfd backend takes over guest RAM after any `-machine memory-backend=` of
  your own: a test that reads guest RAM from a file must turn it off.
- The AVF Image ships no Module.symvers: `vgpu-relinquish` takes CRCs from
  the Image's own modules (`crcs.py`), so imports none of them uses load
  with "no symbol version", and `KBUILD_MODPOST_WARN=1` lets a symbol the
  Image lacks build fine and fail only at load (boot goes on without the
  module). Refresh `sourceRev` and `kernelSource`'s hash with the Image.
