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
- A third-party VM gets the phone's GPU through gfxstream (`AvfReflect.GPU_GFXSTREAM`),
  but the Pixel's android14-6.1 pKVM unmaps nothing from a running VM: deleting
  a memslot is EPERM, and a guest page stays on the host page it first
  faulted on. With crosvm's default per-blob memslots the first GPU context
  after boot works and every later one fails (crosvm: `ResourceUnmapBlob:
  ErrUnspec`, then `File exists`; the guest: `mmap64 failed with (Invalid
  argument)`). `fixed-blob-mapping=true` registers the window once,
  `system-blob=true` gives Vulkan memory the shm fd that needs (pKVM 6.1 also
  maps only swap-backed pages), and the guest's `vgpu_relinquish` drops the
  stale pages. GpuConfig has no field for either option; virtmgr pastes
  `backend` into `--gpu=` unchecked. android15-6.6 and later appear to
  restrict only protected VMs (read from kernel source, not tried on a device).
- crosvm and gfxstream log to logcat (tags `crosvm`, `GFXSTREAM`).
- zink refuses gfxstream: let `VK_EXT_robustness2` through the guest ICD's
  allowlist (Mesa 26.3 does) and `nullDescriptor` still reads false in the
  guest, though the Mali driver has it. `backend=virglrenderer` answers "No
  virgl contexts available on host", and adding `venus` panics crosvm's
  `v_gpu` thread (SIGABRT).
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
- The Java config never sets several `VirtualMachineRawConfig` fields
  (`customMemoryBackingFiles`, `boostUclamp`); `AvfReflect.editRawConfig`
  reaches them through the binder `VirtualMachine.run` uses.
- crosvm maps app-made memory as guest RAM (`--file-backed-mapping …,ram`), as
  a memory region of its own: a virtio buffer across its edge kills the device
  ("invalid descriptor chain memory regions"), and u-boot's kernel load
  straddles a range at the top of RAM. Only all of RAM, from `0x8000_0000`,
  has no edge.
- AVF's sound device (`crosvm_vu_snd`, vhost-user) must map guest RAM, and
  sepolicy denies it the app's descriptor (`avc: denied { use }`): it dies and
  the VM with it. App-made guest RAM and the AVF speaker exclude each other.
- The balloon's free-page reporting fails on app-made (ashmem) guest RAM
  ("failed to handle the inflate balloon event … I/O error"), so with it the
  guest keeps the memory it touched.
- The host refuses crosvm's own `sched_setattr` and `sched_setaffinity` for
  its vCPUs ("Failed to boost vcpu util", "Failed to set CPU affinity":
  EPERM), so `--host-cpu-topology`'s pinning and `boostUclamp` do nothing; the
  vCPUs run anywhere, often on little cores. The app can't reach crosvm's
  threads either.
- `SharedMemory.create` takes an `Int` size and hides its descriptor; the
  NDK's `ASharedMemory_create` takes a `size_t`.
- The app's vsock fds carry virtmgr's label
  (`untrusted_app_all_virtualizationmanager`), and sepolicy denies the app
  `setopt` and `shutdown` on them: `SO_VM_SOCKETS_BUFFER_SIZE` can't be
  raised, so every stream keeps the kernel's 256 KiB window.

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
