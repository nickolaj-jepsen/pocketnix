# Spikes: guest Wayland windows on Android

Follows `wayland-prior-art.md`, which picked the candidates. These spikes ran
one piece of each design and measured it, on 2026-10-26 and 27. They weren't
judged against `wayland-poc` or an fps target: the question is which design
is right. Harness, raw numbers, logs and patches are in
`~/.local/share/agents/pocketnix/spikes/` (`README.md` and the `notes-*.md`
files).

## Setup

- **Display side:** headless sway at 1280x720, standing in for the phone.
- **Guest side:** the server half of each design, with foot or
  gtk3-widget-factory as the app.
- **Link:** a relay that counts bytes each way. For wprs it also ran through
  a desktop pocketnix-agent's forward stream, with the same numbers, so the
  PoC transport needs no agent changes.
- **Workloads:**
  - idle
  - typing into foot (about 19 keys/s)
  - foot scrolling 60 lines/s
  - gtk3-widget-factory's spinners and pulse bar animating
  - a combo popup and a tooltip
- **CPU:** measured with each side pinned to 2 cores and AVX2 off, the
  closest desktop stand-in for phone-vm. Bytes carry over to the phone; CPU
  percentages only roughly.

| system | what it is |
|---|---|
| wprs | upstream HEAD: a guest compositor that sends each surface's whole buffer per commit |
| wprs-patched | wprs plus three changes, below |
| waypipe | 0.11, proxying the Wayland protocol, lz4 or zstd |
| RAIL | Weston's RDP RAIL backend (microsoft/weston-mirror) to xfreerdp `/app` |

## Bandwidth and CPU

Guest→display KiB/s, and CPU as % of one core (guest / display side):

| | typing | scroll | gtk animation |
|---|---|---|---|
| wprs | 66 · 24 / 21 | 1386 · 50 / 42 | 4996 · 107 / 61 |
| wprs-patched | 32 · 3 / 4 | 261 · 20 / 11 | 125 · 8 / 9 |
| waypipe lz4 | 14 · 0.5 / 0.4 | 2288 · 23 / 3 | ~8000 · 62 / 39 |
| waypipe zstd | 9.5 · 0.5 / 0.5 | 1487 · 27 / 4 | ~4000 · 67 / 36 |
| RAIL | 28 | ~60000 | ~60000 |

- All four send nothing at idle.
- The app's own CPU is the same under every system.
- RAIL's CPU wasn't measured pinned: weston used about 50% of a core and
  xfreerdp about 150% while scrolling.

The three changes in wprs-patched (about 370 lines; `src/wprs` branch
`damage-patch`):
- **Damage only.** When the client's damage covers under half the surface,
  only those rects are sent. Wayland damage is relative to the previous
  content whichever buffer held it, so the receiver's copy plus the rects is
  exact. A per-surface mirror in the guest keeps reconnect replay whole.
- **Scroll detection.** foot damages the whole window on every scrolled line.
  A row-hash match against the mirror finds the shift (710 of 720 rows at one
  text line) in about 1.3 ms per 720p frame. Only the new rows are sent, with
  a scroll offset.
- **Plain zstd for patches.** wprs's per-channel filter and 128 KiB sharding
  left small patches uncompressed: 1296 bytes per keystroke against 165 with
  zstd-1. On the GTK run, the same patches came to 18.8 MB with wprs's
  encoding and 1.2 MB with zstd.

Content hashes on the client matched the guest's mirror for every commit
(over 760 in the bandwidth runs, 621 across reattaches).

What the numbers say:
- **Typing** is cheap everywhere. waypipe's diff within the damage beats
  wprs-patched's bytes. Most of wprs-patched's remainder is a 456-byte rkyv
  commit object per key; a compact commit message would bring it to about
  5 KiB/s.
- **Scrolling** costs the same guest CPU under patched wprs and waypipe,
  with 6–9x fewer bytes for wprs.
  - waypipe's display side is cheaper, because wprsc copies the whole
    surface into a new shm buffer each commit. An Android renderer copying
    damage into AHardwareBuffers wouldn't.
  - Scroll detection needs the guest to see whole surfaces. A compositor in
    the guest does; waypipe could add it, but it isn't ours to change.
- **Animation** is where the designs separate.
  - Two spinners and a pulse bar cost waypipe 4–8 MB/s and 60% of a guest
    core. GTK3 hands it whole new buffers to diff.
  - wprs-patched sends the client's 8k-pixel damage per frame: 125 KiB/s
    for 8% + 9%.
  - RAIL sends the damage bounding box uncompressed, with no codec off the
    Hyper-V path.
- **AVX2** made no measurable difference to waypipe or patched wprs, nor did
  thread pool sizes. Only stock wprs, which filters whole buffers, slows down
  without AVX2 (1.5–1.8x).

## Behaviour

| | wprs / wprs-patched | waypipe |
|---|---|---|
| Display proxy restarted | apps live; windows replayed exactly | foot dies (SIGHUP); stale socket blocks a new client |
| Transport or agent restarted | apps live; reconnect restores all | apps die |
| Popups, tooltips | yes | yes |
| Clipboard both ways from apps | yes | yes |
| Clipboard from CLI `wl-copy` in the guest | no: no data-control; wprsc sets the selection only on a click serial | yes |
| Ctrl/shift sent only as a modifiers event | dropped (wprsd syncs only caps and num lock) | yes |
| Fractional scale (2.625) | no: renders at integer 3, 31% more pixels | exact, via fractional-scale-v1 |
| Several windows, focus, close from the display side | yes | yes |

- Under waypipe, an agent update (which restarts the agent and drops its
  streams) would kill every guest GUI app.
- Stock wprs hit upstream race #148 once ("Received buffer commit with
  empty data"): buffer and commit travel as two messages, and a reattach
  between them loses the buffer. The patch keeps that design, so the race
  is still possible there.

## RAIL (the WSLg shape)

It runs, but it needed patching on both sides and doesn't fit:
- **Build:** weston-mirror (Weston 9 base) builds only against Microsoft's
  FreeRDP 2.4 fork.
- **Client:** a stock FreeRDP 3 client needed two server-side fixes, a
  client flag and a client patch before windows appeared at all.
- **Window model:** every surface is its own RAIL window, subsurfaces
  included. One foot window arrives as 12 windows, since Weston 9 has no
  xdg-decoration and foot draws its own title bar. Popups come as separate
  windows in absolute desktop coordinates.
- **Pixels:** uncompressed, and adding a codec means changing a pinned C
  fork.
- **Android client:** aFreeRDP's RAIL code would be a rewrite.

## The in-app compositor (waypipe into Wawona)

The handoff's original plan: desktop `waypipe server -- foot` → `adb
forward` → a static waypipe client on the emulator → Wawona's Wayland
server.
- **Local Desktop** wasn't tried: it starts its compositor only after its
  proot/Arch bootstrap.
- **Wawona** (arm64, under the emulator's ARM translation) rendered foot and
  gtk3-widget-factory, but:
  - it configured the window at 0x0, so foot's content was cropped
  - any key, IME commit or tap panics its smithay seat ("wrong client"), so
    input and popups were untestable
- **Both concerns from `wayland-prior-art.md` showed up live:**
  - **Version coupling:** waypipe passes the APK's whole registry through,
    64 globals including `wp_color_manager_v1` v2. nixpkgs' `wayland-info`
    binds v2 and aborts on a v2-only event its listener lacks.
  - **Double copy:** the phone-side waypipe client peaked at 517 MB RSS
    (258 MB shm) for one foot window, mirroring foot's shm pools, before the
    compositor copies into textures. wprsc stays near 20 MB.
- **Bytes** were in line with the desktop runs: 1.3 MB/s scrolling with lz4.
  CPU wasn't comparable under translation.

## The Android side (emulator, API 36)

A standalone test app tried the recipe from `wayland-prior-art.md`:
document tasks, and a SurfaceView with child ASurfaceControls fed
AHardwareBuffers.
- **Works:**
  - one Recents card per toplevel, with live content
  - dedupe by data URI
  - back as `moveTaskToBack`
  - `finishAndRemoveTask` on unmap
  - freeform resize arrives as `surfaceChanged`, without a recreate
  - several freeform windows resumed at once
- **Stale cards:** they survive a process kill. Sweep them from
  `Application.onCreate`, not from the launcher activity.
- **Popups are clipped to the task bounds.** The guest must solve
  xdg_positioner placement inside the window, which a guest compositor can
  do and an Android-side one would have to reimplement.
- **Close and a Recents swipe can't be intercepted:** the app sees only
  `onDestroy(isFinishing)`. Send `xdg_toplevel.close` from there, and
  relaunch a toplevel that stays mapped (a save dialog).
- **Background launches:** `startActivity` from the background returns
  normally and does nothing (BAL_BLOCK). Launch while a pocketnix window is
  visible; otherwise post a notification, whose PendingIntent works.
- **Window placement:** launch bounds and windowing mode can't be chosen
  from the application context.
- **Copy cost:** the emulator's gralloc costs about 5 ms per AHB lock,
  whatever the rect, so the damage-copy saving has to be measured on the
  phone.

## Conclusions

- **The server belongs in the guest.** Four spikes now argue for it:
  - reattach (waypipe loses every app on an agent restart)
  - scroll and damage handling that needs the whole surface
  - popup placement that Android clips to the window
  - the in-app compositor's protocol skew and memory, both seen live
- **wprs's model is right; its wire format isn't.** The per-surface state,
  roles, replay on connect and the listen-in-the-guest direction all held
  up. The encoding (rkyv, whole buffers, filter and shards) and the
  buffer/commit split are what the patch had to change or still has to
  replace. A pocketnix-owned, versioned message should carry:
  - the surface tree
  - damage rects
  - an optional scroll offset
  - zstd-packed pixels, all in one frame

  Kotlin can read that, so the renderer needs JNI only for the pixel copy
  into AHardwareBuffers. That settles PoC 2b's open question in favour of
  replacing the protocol.
- **The fork's gaps are cheap.** All five behaviour gaps closed in about
  210 lines on branch `gaps` (`notes-wprs-gaps.md`):
  - **modifier-only events:** applied to the seat. The Android side can send
    `metaState` as is.
  - **#148:** queued messages are dropped at accept, since replay resends
    everything. 60 random reattaches mid-scroll went from 20 "empty data"
    errors and a corrupt frame to none in 3497 hashed frames. Separate
    per-connection queues would close the last in-flight window.
  - **fractional-scale-v1:** wprsc sends its output's scale. At 2.625 foot
    renders 1079x2399, like waypipe, and patches still hash-match.
  - **ext/wlr data-control:** guest `wl-copy` works without a surface.
  - **popups constrained to the toplevel in the guest:** an edge menu slides
    inside its window.

  The first four look upstreamable. A pocketnix protocol replaces the wire
  half anyway, so a fork can stay close to upstream's compositor half.
- **RAIL:** dropped.

## Still open

- **phone-vm run:** a split harness and aarch64 builds of wprs-patched and
  waypipe are ready (`spikes/phone/`, steps in `notes-cpu.md`). The guest
  half runs in phone-vm over the app's ssh forward. It has run on the
  desktop under binfmt against an x86 display half, but not on the phone.
  Run it once the phone is free, to replace the desktop CPU estimates and
  measure AHB copy cost.
- **Unchecked on Android:** IME, an external display with real desktop
  windowing, task icons, overlay planes, background launches under a
  foreground service.
