# Handoff: a Wayland compositor on the Android side

Status: exploratory. Nothing here is settled, and none of it is needed for the
current display stream (`go/internal/agent/display.go`, `ui/DisplayPane.kt`),
which moves pixels. This document is for the alternative that does not.

## The problem this solves

The shipped display stream is pixel-based: the guest compositor renders in
software, we read the whole frame back, and ship it to the app, which blits it.
Measured on a desktop agent, 1280x720: 4.6 fps, ~1-3 MB/frame, with grim's own
32 ms (79 ms in the guest) a fifth of the budget. Optimising that path
(in-process screencopy, wlr-screencopy `damage`, rect transport) is worth doing
and is being done separately. It will not remove the fundamental cost: every
frame crosses the VM boundary as pixels and is re-read by the guest CPU.

A compositor on the Android side inverts it: guest applications send *buffers
and protocol* when their surfaces change, and the phone composites on its own
GPU. No full-frame readback, damage at surface granularity, and the phone's
compositor runs at display refresh.

## What the current platform gives us (verified, do not re-derive)

- AVF third-party VM: sepolicy grants only `vmlauncher_app` the crosvm display
  service, so there is no zero-copy scanout for us (`AGENTS.md`, "Traps").
- The guest *can* reach the GPU for Vulkan through crosvm's gfxstream backend
  (`--es gpu gfxstream`), but no GL stack can use it: zink needs robustness2
  `nullDescriptor` and Mesa's gfxstream ICD does not expose it at all;
  ANGLE has no `EGL_EXT_platform_device`, `EGL_KHR_platform_gbm` or Wayland
  platform; venus panics this crosvm and virgl has no host contexts. So guest
  applications render in software (llvmpipe/pixman) regardless of this work.
  GPU compositing on the phone side is still a win: compositing is cheap there,
  and the wire carries only damaged surface content.
- A Wayland compositor needs, from the client side: `wl_shm` (or dmabuf),
  `xdg-shell`, `wl_seat`, `wl_output`, `wp_viewporter`, `xdg-decoration`,
  fractional-scale, `wl_data_device` for clipboard. Guest apps under sway today
  use exactly these.

## Shape of the work

Three layers, each independently useful:

1. **Transport.** A proxy over the agent's existing stream machinery (the app
   always dials; vsock on the phone, TCP for a desktop agent). Wayland messages
   are small; app buffers are the payload.
2. **Buffers.** Guest `wl_shm` buffers are guest memory: the guest side must
   copy out the damaged region and send it (a per-surface, per-damage transfer),
   or hand over dmabufs — which needs a guest GPU path we do not have, so plan
   for shm. This is where the bandwidth goes: a typing app damages a few
   thousand pixels, a video player the whole surface.
3. **Compositor.** A Wayland server on Android rendering with EGL into a
   `SurfaceView`, mapping each guest surface to a texture updated only where it
   changed. This is the large piece: `libwayland-server` is not in the Android
   platform, so it means building it with the NDK and bridging through JNI, or
   writing a server (protocol dispatch, object table, fd passing) from scratch.

Read `waypipe` first: it already solves "carry a Wayland client's protocol and
buffers over a byte stream", including damage-ish optimisations, dmabuf/shm
handling and `wl_shm` pool management. The cheapest viable design is very
likely waypipe's guest side plus a compositor on the Android side, rather than
a bespoke proxy.

### Variant B: an X11 server on Android instead

`Termux:X11` is an X server on Android that renders with GLES into a
SurfaceView, and X11 carries damage as regions natively. Guest apps become X11
clients over the agent's stream. This avoids writing a Wayland server at all
and is the pragmatic version of the same idea; costs are Wayland-native apps,
portals and HiDPI/scaling polish.

### Variant C: compositor on Android as the *only* compositor

Guest runs a headless compositor for policy (or none) and apps connect straight
to the Android-side compositor. Simplest semantics, worst compatibility: apps
expect a full session (portals, dbus, seat) around them.

## Non-goals

- Not a way to run GPU-heavy guests: the guest's GL is software either way.
- Not the existing pixel stream: do not bolt rect transport onto this document's
  work and call it protocol compositing.
- Not a replacement for `--es gpu gfxstream`; that is orthogonal and stays.

## Acceptance criteria for a first slice

- One guest application (a terminal) shown in the app, typed into through the
  phone keyboard, at ≥30 fps while scrolling text at 720p-equivalent size.
- Clipboard both ways (`wl_data_device`), and the app's back gesture ending a
  session cleanly.
- Measured `KiB/s` while typing under 200, and idle traffic at zero.
- Works with a desktop agent over TCP first, then in the guest over vsock; no
  guest image rebuild needed for the desktop path.

## Where to start

1. Read `docs/design.md` (settled decisions: terminal first, no graphical guest)
   and the display-stream code, then waypipe's transport and its damage model.
2. Prototype the compositor against a *local* waypipe client before touching
   the agent: proof that a guest app renders in an Android SurfaceView at all.
3. Only then wire it to a stream kind (`protocol.StreamWayland`) and the app's
   pane. Keep the pixel stream as a fallback for the transition.

## Risks

- Guest applications draw with software rendering; a busy app still costs guest
  CPU, and the phone's GPU only buys compositing.
- Wayland protocol coverage is a treadmill; every missing global is a bug visible
  to users. X11 (variant B) has a smaller surface but the same class of problem.
- fd passing over vsock is supported, but every buffer transfer is a copy; a
  full-screen video at 60 fps is out of reach either way.
- `libwayland-server` under the NDK is a build-system project of its own; budget
  for it explicitly, and consider a Rust or Kotlin Wayland server if the C
  route fights the NDK.
