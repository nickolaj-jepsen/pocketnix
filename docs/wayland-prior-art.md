# Prior art: guest Wayland windows on Android

Research for `handoff-wayland-on-android.md`, from reading source, issues and
docs on 2026-10-26. Nothing below was built or run. Read the handoff first;
this picks between its options and corrects a few of its premises.

## Corrections to the handoff

- **Its baseline is the weakest pixel path.** The 4.6 fps comes from a grim
  screenshot loop. The `wayland-poc` branch (sway headless, wayvnc, an RFB
  client) reached about 57 fps at 1280x720 on the desktop, with about 2 ms
  from keypress to changed pixels and no traffic at idle. It hasn't been
  measured on the phone.
- **vsock can't pass fds.** AF_VSOCK has no SCM_RIGHTS, so no buffer can be
  shared across the boundary, only copied.
- **The guest never needs to dial the app.** The agent can listen in the guest
  and splice each connection onto a stream that the app opens on request. That
  covers X11 and waypipe as well.
- **`libwayland-server` isn't needed in the app.** Smithay's `wayland-backend`
  is a pure-Rust server, and Local Desktop ships it through the NDK.

## Three independent layers

| Layer | Choice |
|---|---|
| Presentation on Android | One Activity per toplevel, with one SurfaceControl per Wayland surface (anland's recipe, below). This works whichever server and transport are chosen. |
| Where the Wayland server lives | In the guest (a remote compositor). See below for why not in the app. |
| Transport | A forward stream to a guest loopback port for the PoCs, which needs no agent changes. A stream kind of its own later. |

## The projects

### anland: the presentation reference

[SuperTurtleDev/anland](https://github.com/SuperTurtleDev/anland) (GPL-3.0-only).

- **Process model:** the server is a root daemon. The Activity half
  (`libawl`: `AwlWindowActivity`, `Awl.attachWindow`) uses only public API in
  the consuming app.
- **Launching:**
  - Each toplevel is a document task: `NEW_TASK|NEW_DOCUMENT`, with a unique
    data URI per window.
  - The manifest sets `documentLaunchMode=intoExisting`,
    `autoRemoveFromRecents` and `persistableMode=persistNever`.
  - A per-id in-flight lock stops duplicate tasks.
  - Stale Recents cards are swept through `getAppTasks()`.
  - Title and icon come from `setTaskDescription`.
- **Content:** every Wayland layer (subsurfaces, popups, cursor) is a sibling
  SurfaceControl under the window. Buffer and geometry go in one transaction,
  and frame callbacks fire at the vsync tick. Popups stay inside the window,
  clamped to it.
- **Teardown trap:** a layer must be hidden and reparented to null before it's
  released, or it stays on screen.
- **Input:**
  - Touch, mouse, touchpad gestures and pointer capture all work.
  - Keys are sent as evdev scancodes.
  - IME is a hidden EditText whose InputConnection is bridged to
    text-input-v3.
  - There is a clipboard bridge.
- **Licence:** GPL-3.0-only. Copying its code would make pocketnix GPL-3.0-only
  as well. Use it as a reference unless that's decided.

### wlroots-android-bridge

[Xtr126/wlroots-android-bridge](https://github.com/Xtr126/wlroots-android-bridge)
(GPL-3; the native side lives in Xtr126/labwc-android).

- **Process model:** labwc runs under `app_process` from Termux or adb. It
  starts activities through a hidden-API `am`, and the app's Surface is passed
  over binder.
- **Buffers:**
  - It needs minigbm gralloc tricks, so it won't work on a Pixel from a
    normal app.
  - There's one buffer per view, with no fences, no damage and no popups.
- **Input:** no touch, no IME.
- **Verdict:** the idea that started this; reference only.

### Wawona

[github.com/Wawona](https://github.com/Wawona): a Rust/Smithay compositor for
Apple platforms, Android and Linux.

- **Android:** one `SessionActivity` per toplevel, each in its own task.
  - A host id is reserved before the client launches, and the first toplevel
    claims it.
  - Rendering is Vulkan into a SurfaceView.
- **waypipe:** built in as a staticlib (`wwn-waypipe` patches waypipe 0.11).
- Its `docs/2026-nixos-vm-bridge.md` describes our topology, guest
  `waypipe --vsock server` to host `waypipe client` to the compositor. Only the
  macOS version is built.
- **Maturity:** started 2025-11, very active, largely agent-written and
  changing fast.
- **Verdict:** worth reading; not a dependency.

### wprs: the guest-half reference

[wayland-transpositor/wprs](https://github.com/wayland-transpositor/wprs)
(Apache-2, Smithay 0.7).

- **What it is:** wprsd is a compositor that never composites.
  - Each commit sends a surface's full state: its role (toplevel, popup with a
    raw positioner, subsurface with an offset, cursor), its buffer and its
    metadata.
  - The client recreates the windows locally.
  - Reconnecting replays every surface's last state.
- **The direction already fits pocketnix:** wprsd listens and the client
  dials.
- **Size:** the server, serialization and compression together are about 8k
  lines.
- **Gaps that matter here:**
  - **Whole buffers every commit.** Damage is sent as metadata only. The
    compression (transpose, delta, YUV-like, zstd) buys little over vsock and
    competes with guest software rendering for CPU.
  - **No backpressure.** The send queue is unbounded; issue #90 reports memory
    growing 30–80 MB/s.
  - **An rkyv protocol with no version.** Kotlin can't read it, so a phone
    side that keeps it must be Rust.
  - **The client solves popup placement.**
  - **Missing input and scaling:** no touch, no text-input, no fractional
    scale.
  - **Open bugs:** #140 (a subsurface "no parent" crash) and #138, #148
    (reattach).

### Local Desktop: pieces worth lifting

[localdesktop/localdesktop](https://github.com/localdesktop/localdesktop) (GPL-3).

- **Architecture:** a Smithay 0.5 compositor (vendored, with the pure-Rust
  backend) in a NativeActivity, rendering with GLES.
- **Single window only:**
  - It serves one fullscreen client, a nested labwc that draws the whole
    desktop.
  - The first toplevel takes all input, and the popup handlers are stubs.
- **Missing:** an Android IME (the on-screen keyboard is Squeekboard in the
  guest), a clipboard bridge and wl_touch.
- **Worth lifting:**
  - `hardware_buffer.rs`:
    - two RGBA AHardwareBuffers per surface, bound through
      `eglGetNativeClientBufferANDROID`
    - only the damaged rows copied, since that buffer's last fill
    - EGL fences against overwriting a buffer the GPU is still reading
  - `frame_clock.rs`: AChoreographer pacing, presenting nothing while idle.
  - `keymap.rs`: key mapping.
  - Its touch gesture state machine.

### waypipe

[mstoeckl/waypipe](https://gitlab.freedesktop.org/mstoeckl/waypipe) (Rust
0.11, GPL-3.0+).

- **Who dials:** `waypipe client` listens on the display side and
  `waypipe server` dials once per Wayland client. `--vsock` makes the guest
  dial, which AVF forbids, so it would need the agent's splice.
- **How it diffs:** it uses wl_surface damage (a 7-commit history per surface)
  and compares those spans against a full mirror of every shm pool. That
  roughly doubles client buffer memory in the guest. SIMD is AVX2-only, so
  aarch64 runs the scalar loop.
- **Two version couplings:**
  - waypipe↔waypipe: negotiated, but the compression setting must match
    exactly.
  - Guest apps' protocols ↔ whatever compositor sits behind `waypipe client`.
- **Not a remote-compositor transport:** it replays the full protocol into a
  real Wayland server on the receiving side.

### Termux:X11

[termux/termux-x11](https://github.com/termux/termux-x11).

- **History:** "lorie" was a minimal Wayland server that only hosted Xwayland.
  It became a built-in X server in 2023-05, after Xwayland updates kept
  breaking it.
- **Rendering:** the X server shares AHardwareBuffers or memfds with the
  Activity, which draws them with GLES. Offload is disabled when the
  AHardwareBuffer isn't dma-buf backed, because virtualized gralloc broke it.
- **IME:** a `BaseInputConnection` with no editable text. Committed text is
  turned into keysyms, and the keymap is extended on the fly.

### Not picked

- **WSLg's RDP RAIL:**
  - Weston [!1667](https://gitlab.freedesktop.org/wayland/weston/-/merge_requests/1667)
    has been stalled since 2025-05.
  - Its shared-memory path is tied to Hyper-V and Microsoft's client.
  - aFreeRDP's RAIL support draws every window inside one Activity and copies
    whole surfaces.
- **wheatley/libwlb** (2014): one nested compositor fullscreen. Historical.
- **Cuttlefish's `host/libs/wayland`:** a host-side, one-surface server on
  AOSP's `external/wayland` that ignores damage.
- **ANativeComp-Wayland, WayLandIE:** root-only or minimum-viable quality.
  WayLandIE (MIT) shows a Java `SurfaceControl` child with a native
  `setBufferWithRelease`.

## Where the server lives: the guest

A compositor in the app (for example waypipe into a Smithay compositor, which
is the handoff's main plan) has to support every protocol version that guest
apps from nixpkgs use. The APK ships on its own schedule, so the two drift
apart: the problem Termux:X11 hit. It also needs a reverse stream per client,
a mirror of every buffer in the guest, scalar diffing and two or more extra
copies per frame.

A compositor in the guest avoids all of that:
- it's built from the same flake as the apps, so protocol support never drifts
- the app-to-guest protocol is pocketnix's own and versioned like the rest
- clients report their damage exactly, so nothing needs a mirror or a diff

## Recipe: one Activity per guest toplevel

- **Manifest:**
  - `documentLaunchMode=intoExisting`, `autoRemoveFromRecents`,
    `persistableMode=persistNever`
  - `configChanges` covering size, density, uiMode, keyboard and navigation,
    so resizing a desktop window doesn't recreate the activity
- **Launching:**
  - `NEW_TASK|NEW_DOCUMENT` with `pocketnix://win/<boot id>/<toplevel id>`,
    deduped per id
  - start activities only while a pocketnix window is visible; otherwise queue
    them for the next `onStart`, or post a notification
- **Binding:**
  - `onCreate` looks the id up in a process-wide registry; if the id is
    unknown, `finishAndRemoveTask`
  - at app start, remove stale `getAppTasks()` cards, since the VM dies with
    the process
- **Back and close:**
  - Back minimizes (`moveTaskToBack`).
  - The close button and a Recents swipe send `xdg_toplevel.close`. If the
    toplevel is still mapped afterwards (a save dialog), relaunch it.
  - When the guest unmaps the toplevel, `finishAndRemoveTask`.
- **Content:**
  - a SurfaceView with a child SurfaceControl per Wayland surface
  - merge subsurfaces into the parent's buffer when that's cheap, since every
    layer may push SurfaceFlinger off hardware overlays
  - detach on `onStop`, not `onPause`: visible desktop windows all stay resumed
- **Buffers:**
  - 2–3 AHardwareBuffers per surface, locked through the NDK (Java has no CPU
    lock for `HardwareBuffer`)
  - copy damage only, tracking each buffer's age
  - present with `ASurfaceTransaction_setBufferWithRelease` and
    `setDamageRegion`
  - `ANativeWindow_lock` on the Surface is the simple first version
- **Cursor:** `PointerIcon.create`.
- **Unchecked:**
  - whether freeform popups are clipped to the task bounds
  - TaskDescription icons on Android 17
  - the Pixel 8 Pro's overlay plane count
  - how Recents behaves with desktop windowing on an external display

## Proofs of concept

0. **The baseline.** Measure `wayland-poc` on the phone against the handoff's
   acceptance criteria. Every PoC is judged against it.
1. **The Activity shell on the existing transport.** Turn `wayland-poc`'s window
   tabs into one Activity per window using the recipe above, still fed by the
   sway broker with one wayvnc per window. This tests Recents, back and close,
   background launches, IME and external-display windowing without waiting on
   the server decision. The renderer underneath is swapped later.
2. **wprs as a remote compositor, desktop first.**
   - a. Run wprsd in the guest behind
     `socat TCP-LISTEN:<port>,bind=127.0.0.1 UNIX-CONNECT:<wprsd socket>`,
     reached as a forward stream. Point the stock wprs client on the desktop
     at it through a desktop agent, and measure bytes per second while typing
     in foot, at idle and while scrolling.
   - b. In a fork:
     - send damage-cropped pixels, with compression optional
     - make the send queue bounded and coalescing per surface
     - put a protocol version in the hello
     - send popup positions already solved in the guest

     Measure again.
   - c. Write the Android renderer, driving PoC 1's Activities. Either do it in
     Rust through the NDK on wprs's protocol, or replace that protocol with
     simple frames that Kotlin reads, with JNI only for the pixel copy. Decide
     after 2b.

Not pursued: waypipe into an in-app compositor, RDP RAIL, X11.
