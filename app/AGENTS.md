# Traps in the app

The repository's root `AGENTS.md` has the working rules and the traps
that cross directories.

## Traps (verified on a Pixel 8 Pro, GrapheneOS, Android 17)

- Raw VM configs need `bootloader` = `/apex/com.android.virt/etc/u-boot.bin`.
- virtmgr can't open files on FUSE (`/sdcard`): disk images live in app-private storage.
- The balloon needs `useAutoMemoryBalloon` in the custom-image config;
  virtmgr also requires `hypervisor.memory_reclaim.supported` (the Pixel 8
  Pro has it), and logs "Memory balloon not enabled" otherwise.
- The AVF API is `@SystemApi` and `VirtualMachineCustomImageConfig` is `@hide`:
  reach them by reflection with in-process HiddenApiBypass. Its
  `GpuConfig.Builder` takes boxed `Boolean`s on GrapheneOS 17, so a
  `boolean.class` lookup throws (`AvfReflect.set` tries both).
- A third-party VM can have the phone's GPU, but nothing yet composites on it.
  `GpuConfig(backend=gfxstream, vulkan=true, egl=false, gles=false, glx=false,
  surfaceless=true, context-types=[gfxstream-vulkan, gfxstream-composer])`
  (the Terminal app's own config) reaches crosvm as `--gpu=…`, and the guest's Mesa gfxstream ICD then runs
  `vulkaninfo` against the real GPU ("Virtio-GPU GFXStream (Mali-G715)").
  Leaving `egl`/`gles` unset is not equivalent: virtmgr defaults them true and
  the guest's ASG stream then dies on `mmap64: Invalid argument`. Two blockers
  stop a compositor anyway: the ICD lacks `VK_EXT_physical_device_drm`
  (wlroots' Vulkan renderer refuses the device) and zink needs robustness2's
  `nullDescriptor` (also absent). `backend=virglrenderer` answers "No virgl
  contexts available on host", and adding `venus` panics crosvm's `v_gpu`
  thread (SIGABRT).
- Only the display *surface* handoff is sealed to `vmlauncher_app` (sepolicy),
  which is why the guest gets a render node but no scanout. GrapheneOS's own
  Terminal app is not a better reference: its `GraphicsManager` offers only
  Lavapipe (software) and Gfxstream, and `gfxstream_supported` is a device
  overlay. On the Pixel 8 Pro its GUI renders in software, while our app still
  gets a working gfxstream device.
- A guest `reboot` stops crosvm with `STOP_REASON_REBOOT`; the app must start the
  VM again then, and leave it stopped on `poweroff`.
- Forwards share the phone's ports with other apps: another app holding
  `[::]:<port>` makes ours fail to listen, and `adb forward` reaches theirs.
- Termux's `ByteQueue` throws on a zero-length write; keep empty writes out.
- An app can't `FIONREAD` a vsock fd, so `available()` on its stream throws,
  and a `BufferedInputStream` calls it after every short read: big PTY output
  and forwards died. `AgentLink`'s buffer hides it; TCP tests can't catch it.

## Traps (verified on the desktop)

- Android creates content providers before `Application.onCreate`: anything the
  provision provider reaches must not depend on onCreate having run.
- An unsigned release build must have no `release` signing config at all;
  `build.gradle.kts` only makes one when a keystore is given.
- Any uncaught exception on an app thread kills the process and so the VM;
  `PocketnixApp` logs worker-thread ones instead, and guest output goes
  through a fuzzed emulator (`FuzzTest`).
- "The default network is validated" overreports: a VPN that never goes away
  (Tailscale) stays the app's validated default network with no underlying
  link (`UnderlyingNetworks: []`), so with Wi-Fi and mobile data off
  `host-state` keeps saying online (and metered, from the last network) and
  `pocketnix-online.target` stays active until the VPN app is stopped. The
  VM's own `avf_tap_fixed` is a validated ethernet network without INTERNET
  too.
