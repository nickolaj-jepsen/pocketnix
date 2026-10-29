//! The synthetic client: one window, content that costs it next to
//! nothing to draw, and timing of every frame from commit to callback.

use std::collections::VecDeque;
use std::os::fd::AsFd;
use std::os::fd::AsRawFd;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::path::PathBuf;
use std::time::Duration;
use std::time::Instant;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;
use serde_json::Map;
use serde_json::Value;
use serde_json::json;
use wayland_client::Connection;
use wayland_client::Dispatch;
use wayland_client::EventQueue;
use wayland_client::QueueHandle;
use wayland_client::WEnum;
use wayland_client::delegate_noop;
use wayland_client::globals::GlobalListContents;
use wayland_client::globals::registry_queue_init;
use wayland_client::protocol::wl_buffer;
use wayland_client::protocol::wl_buffer::WlBuffer;
use wayland_client::protocol::wl_callback;
use wayland_client::protocol::wl_callback::WlCallback;
use wayland_client::protocol::wl_compositor::WlCompositor;
use wayland_client::protocol::wl_pointer;
use wayland_client::protocol::wl_pointer::WlPointer;
use wayland_client::protocol::wl_registry::WlRegistry;
use wayland_client::protocol::wl_seat;
use wayland_client::protocol::wl_seat::WlSeat;
use wayland_client::protocol::wl_shm;
use wayland_client::protocol::wl_shm::WlShm;
use wayland_client::protocol::wl_shm_pool::WlShmPool;
use wayland_client::protocol::wl_surface;
use wayland_client::protocol::wl_surface::WlSurface;
use wayland_client::protocol::wl_touch;
use wayland_client::protocol::wl_touch::WlTouch;
use wayland_protocols::wp::fractional_scale::v1::client::wp_fractional_scale_manager_v1::WpFractionalScaleManagerV1;
use wayland_protocols::wp::fractional_scale::v1::client::wp_fractional_scale_v1;
use wayland_protocols::wp::fractional_scale::v1::client::wp_fractional_scale_v1::WpFractionalScaleV1;
use wayland_protocols::wp::viewporter::client::wp_viewport::WpViewport;
use wayland_protocols::wp::viewporter::client::wp_viewporter::WpViewporter;
use wayland_protocols::xdg::shell::client::xdg_surface;
use wayland_protocols::xdg::shell::client::xdg_surface::XdgSurface;
use wayland_protocols::xdg::shell::client::xdg_toplevel;
use wayland_protocols::xdg::shell::client::xdg_toplevel::XdgToplevel;
use wayland_protocols::xdg::shell::client::xdg_wm_base;
use wayland_protocols::xdg::shell::client::xdg_wm_base::XdgWmBase;

use crate::parsed;
use crate::snapshot;
use crate::stats::Histogram;
use crate::value;

const BUFFERS: usize = 3;
/// A window the compositor configures with no size gets this logical size.
const DEFAULT_SIZE: (i32, i32) = (400, 300);
/// Commits whose callback hasn't come yet; older ones stop being timed.
const MAX_TIMED: usize = 64;

#[derive(Clone, Copy, PartialEq, Debug)]
enum Mode {
    Noise,
    Gradient,
    Scroll,
    Rect,
    Idle,
    Touch,
}

struct Opts {
    mode: Mode,
    duration: f64,
    warmup: f64,
    rate: f64,
    res: f64,
    scroll: u32,
    rect: u32,
    fullscreen: bool,
    timeout: f64,
    stats: Vec<(String, PathBuf)>,
}

fn parse(args: &[String]) -> Result<Opts> {
    let mut o = Opts {
        mode: Mode::Noise,
        duration: 10.0,
        warmup: 2.0,
        rate: 0.0,
        res: 1.0,
        scroll: 8,
        rect: 64,
        fullscreen: false,
        timeout: 60.0,
        stats: Vec::new(),
    };
    let mut default_stats = true;
    let mut it = args.iter();
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--mode" => {
                o.mode = match value(&mut it, arg)? {
                    "noise" => Mode::Noise,
                    "gradient" => Mode::Gradient,
                    "scroll" => Mode::Scroll,
                    "rect" => Mode::Rect,
                    "idle" => Mode::Idle,
                    "touch" => Mode::Touch,
                    m => bail!("unknown mode {m:?}"),
                }
            }
            "--duration" => o.duration = parsed(&mut it, arg)?,
            "--warmup" => o.warmup = parsed(&mut it, arg)?,
            "--rate" => o.rate = parsed(&mut it, arg)?,
            "--res" => o.res = parsed(&mut it, arg)?,
            "--scroll" => o.scroll = parsed(&mut it, arg)?,
            "--rect" => o.rect = parsed(&mut it, arg)?,
            "--fullscreen" => o.fullscreen = true,
            "--timeout" => o.timeout = parsed(&mut it, arg)?,
            "--no-default-stats" => default_stats = false,
            "--stats" => {
                let v = value(&mut it, arg)?;
                let (name, path) = v.split_once('=').with_context(|| format!("--stats {v:?}: want NAME=SOCKET"))?;
                o.stats.push((name.into(), path.into()));
            }
            _ => bail!("unknown argument {arg:?}"),
        }
    }
    if !(o.res > 0.0 && o.res <= 4.0) {
        bail!("--res {} is outside (0, 4]", o.res);
    }
    if default_stats && !o.stats.iter().any(|(n, _)| n == "compositor") {
        o.stats.push(("compositor".into(), snapshot::compositor_socket()?));
    }
    Ok(o)
}

/// One generation of shm buffers, all of the same size.
struct Buffers {
    generation: u32,
    width: i32,
    height: i32,
    stride: i32,
    map: *mut u8,
    len: usize,
    _fd: OwnedFd,
    pool: WlShmPool,
    bufs: Vec<(WlBuffer, bool)>,
}

impl Buffers {
    fn pixels(&mut self, i: usize) -> &mut [u8] {
        let size = (self.stride * self.height) as usize;
        // SAFETY: the map is `len` bytes, `BUFFERS` buffers of `size` each, and lives as long as self.
        unsafe { std::slice::from_raw_parts_mut(self.map.add(i * size), size) }
    }
}

impl Drop for Buffers {
    fn drop(&mut self) {
        for (b, _) in &self.bufs {
            b.destroy();
        }
        self.pool.destroy();
        // SAFETY: mapped in `alloc` with this length, and nothing borrows it past self.
        unsafe { libc::munmap(self.map.cast(), self.len) };
    }
}

struct Metrics {
    since: Instant,
    frames: u64,
    presented: u64,
    missed: u64,
    starved: u64,
    render: Duration,
    latency: Histogram,
    interval: Histogram,
    input_latency: Histogram,
    last_done: Option<Instant>,
}

impl Metrics {
    fn new() -> Self {
        Self {
            since: Instant::now(),
            frames: 0,
            presented: 0,
            missed: 0,
            starved: 0,
            render: Duration::ZERO,
            latency: Histogram::default(),
            interval: Histogram::default(),
            input_latency: Histogram::default(),
            last_done: None,
        }
    }
}

struct Bench {
    opts: Opts,
    qh: QueueHandle<Bench>,
    shm: WlShm,
    surface: WlSurface,
    _toplevel: XdgToplevel,
    viewport: Option<WpViewport>,
    has_fractional: bool,
    configured: bool,
    closed: bool,
    /// The configured logical size, and the size the buffers were made for.
    size: (i32, i32),
    scale: f64,
    shown: Option<((i32, i32), f64)>,
    buffers: Option<Buffers>,
    next_generation: u32,
    next_buffer: usize,
    page: Vec<u8>,
    frame_no: u64,
    /// Frames committed and not yet called back: number, commit time, the input it answers.
    timed: VecDeque<(u64, Instant, Option<Instant>)>,
    frame_pending: bool,
    pending_input: Option<Instant>,
    first_presented: bool,
    m: Metrics,
    _seat: Option<WlSeat>,
    touch: Option<WlTouch>,
    pointer: Option<WlPointer>,
}

pub fn run(args: &[String]) -> Result<()> {
    let opts = parse(args)?;
    let conn = Connection::connect_to_env().context("connecting to the Wayland display")?;
    let (globals, mut queue) = registry_queue_init::<Bench>(&conn).context("listing the globals")?;
    let qh = queue.handle();
    let compositor: WlCompositor = globals.bind(&qh, 4..=6, ()).context("wl_compositor")?;
    let shm: WlShm = globals.bind(&qh, 1..=1, ()).context("wl_shm")?;
    let wm: XdgWmBase = globals.bind(&qh, 1..=5, ()).context("xdg_wm_base")?;
    let viewporter: Option<WpViewporter> = globals.bind(&qh, 1..=1, ()).ok();
    let fractional: Option<WpFractionalScaleManagerV1> = globals.bind(&qh, 1..=1, ()).ok();
    let seat: Option<WlSeat> = if opts.mode == Mode::Touch { globals.bind(&qh, 1..=7, ()).ok() } else { None };

    let surface = compositor.create_surface(&qh, ());
    let viewport = viewporter.as_ref().map(|v| v.get_viewport(&surface, &qh, ()));
    if let Some(f) = &fractional {
        f.get_fractional_scale(&surface, &qh, ());
    }
    let xdg = wm.get_xdg_surface(&surface, &qh, ());
    let toplevel = xdg.get_toplevel(&qh, ());
    toplevel.set_title("pocketnix-gui-bench".into());
    toplevel.set_app_id("pocketnix-gui-bench".into());
    if opts.fullscreen {
        toplevel.set_fullscreen(None);
    }
    surface.commit();

    let mut b = Bench {
        opts,
        qh: qh.clone(),
        shm,
        surface,
        _toplevel: toplevel,
        viewport,
        has_fractional: fractional.is_some(),
        configured: false,
        closed: false,
        size: (0, 0),
        scale: 1.0,
        shown: None,
        buffers: None,
        next_generation: 1,
        next_buffer: 0,
        page: Vec::new(),
        frame_no: 0,
        timed: VecDeque::new(),
        frame_pending: false,
        pending_input: None,
        first_presented: false,
        m: Metrics::new(),
        _seat: seat,
        touch: None,
        pointer: None,
    };

    enum Phase {
        Waiting(Instant),
        Warmup(Instant),
        Measuring(Instant),
    }
    let mut phase = Phase::Waiting(Instant::now() + Duration::from_secs_f64(b.opts.timeout));
    let period = (b.opts.rate > 0.0).then(|| Duration::from_secs_f64(1.0 / b.opts.rate));
    let mut next_tick = Instant::now();
    let mut before: Vec<Option<Value>> = Vec::new();
    let mut proc_before = None;
    eprintln!("pocketnix-gui-bench: {:?}, waiting for the window to show", b.opts.mode);
    loop {
        if b.closed {
            bail!("the window was closed");
        }
        let now = Instant::now();
        let deadline = match phase {
            Phase::Waiting(until) => {
                if b.first_presented {
                    let until = now + Duration::from_secs_f64(b.opts.warmup);
                    phase = Phase::Warmup(until);
                    until
                } else if now >= until {
                    bail!("no frame was presented within {}s: is the app showing the window?", b.opts.timeout);
                } else {
                    until
                }
            }
            Phase::Warmup(until) if now >= until => {
                before = b.opts.stats.iter().map(|(name, path)| read_or_warn(name, path)).collect();
                proc_before = Some(snapshot::proc_sample());
                b.m = Metrics::new();
                eprintln!("MEASURE-START");
                let until = Instant::now() + Duration::from_secs_f64(b.opts.duration);
                phase = Phase::Measuring(until);
                until
            }
            Phase::Warmup(until) => until,
            Phase::Measuring(until) if now >= until => break,
            Phase::Measuring(until) => until,
        };

        if b.configured {
            match b.opts.mode {
                Mode::Idle => {
                    if b.frame_no == 0 {
                        b.draw(None);
                    }
                }
                Mode::Touch => {
                    if b.frame_no == 0 || (b.pending_input.is_some() && !b.frame_pending) {
                        let input = b.pending_input.take();
                        b.draw(input);
                    }
                }
                _ => match period {
                    None => {
                        if !b.frame_pending {
                            b.draw(None);
                        }
                    }
                    Some(p) if now >= next_tick => {
                        if b.frame_pending {
                            b.m.missed += 1;
                        } else {
                            b.draw(None);
                        }
                        next_tick += p;
                        if next_tick < now {
                            next_tick = now + p;
                        }
                    }
                    Some(_) => {}
                },
            }
        }
        let mut wake = deadline;
        if period.is_some() && b.configured {
            wake = wake.min(next_tick);
        }
        dispatch_until(&mut queue, &mut b, wake)?;
    }

    let secs = b.m.since.elapsed().as_secs_f64();
    let proc_after = snapshot::proc_sample();
    let mut sources = Map::new();
    for ((name, path), before) in b.opts.stats.iter().zip(&before) {
        if let (Some(before), Some(after)) = (before, read_or_warn(name, path)) {
            sources.insert(name.clone(), snapshot::diff(before, &after));
        }
    }
    eprintln!("MEASURE-END");
    let result = b.result(secs, &proc_before.expect("measured"), &proc_after, Value::Object(sources));
    eprintln!("{}", summary(&result));
    println!("{result}");
    Ok(())
}

fn read_or_warn(name: &str, path: &std::path::Path) -> Option<Value> {
    snapshot::read_socket(path).map_err(|e| eprintln!("pocketnix-gui-bench: stats {name}: {e:#}")).ok()
}

/// Reads and dispatches events until `deadline` or the first event.
fn dispatch_until(queue: &mut EventQueue<Bench>, b: &mut Bench, deadline: Instant) -> Result<()> {
    queue.dispatch_pending(b)?;
    queue.flush()?;
    if let Some(guard) = queue.prepare_read() {
        let timeout = deadline.saturating_duration_since(Instant::now());
        let mut pfd = libc::pollfd { fd: guard.connection_fd().as_raw_fd(), events: libc::POLLIN, revents: 0 };
        // Rounded up, so a deadline less than 1 ms away doesn't spin.
        let ms = (timeout.as_micros().div_ceil(1000)).min(1000) as i32;
        // SAFETY: one valid pollfd.
        if unsafe { libc::poll(&mut pfd, 1, ms) } > 0 {
            guard.read()?;
        }
    }
    queue.dispatch_pending(b)?;
    Ok(())
}

impl Bench {
    fn buffer_size(&self) -> (i32, i32) {
        let (w, h) = self.size;
        let s = self.scale * self.opts.res;
        (((w as f64 * s).round() as i32).max(1), ((h as f64 * s).round() as i32).max(1))
    }

    fn alloc(&mut self) -> Result<()> {
        let (width, height) = self.buffer_size();
        let stride = width * 4;
        let size = (stride * height) as usize;
        let len = size * BUFFERS;
        // SAFETY: a fresh memfd, sized and mapped once here.
        let fd = unsafe {
            let fd = libc::memfd_create(c"pocketnix-gui-bench".as_ptr(), libc::MFD_CLOEXEC);
            if fd < 0 {
                bail!("memfd_create: {}", std::io::Error::last_os_error());
            }
            OwnedFd::from_raw_fd(fd)
        };
        if unsafe { libc::ftruncate(fd.as_raw_fd(), len as libc::off_t) } != 0 {
            bail!("ftruncate: {}", std::io::Error::last_os_error());
        }
        // SAFETY: maps the memfd just sized to `len`.
        let map = unsafe {
            libc::mmap(std::ptr::null_mut(), len, libc::PROT_READ | libc::PROT_WRITE, libc::MAP_SHARED, fd.as_raw_fd(), 0)
        };
        if map == libc::MAP_FAILED {
            bail!("mmap: {}", std::io::Error::last_os_error());
        }
        let generation = self.next_generation;
        self.next_generation += 1;
        let pool = self.shm.create_pool(fd.as_fd(), len as i32, &self.qh, ());
        let bufs = (0..BUFFERS)
            .map(|i| {
                let b = pool.create_buffer(
                    (i * size) as i32,
                    width,
                    height,
                    stride,
                    wl_shm::Format::Xrgb8888,
                    &self.qh,
                    (generation, i),
                );
                (b, false)
            })
            .collect();
        let mut buffers = Buffers {
            generation,
            width,
            height,
            stride,
            map: map.cast(),
            len,
            _fd: fd,
            pool,
            bufs,
        };
        let (w, h) = (width as usize, height as usize);
        for i in 0..BUFFERS {
            let px = buffers.pixels(i);
            match self.opts.mode {
                Mode::Noise => noise(px, 0x9e37_79b9_7f4a_7c15 ^ (i as u64 + 1) * 0x1234_5678_9abc_def1),
                Mode::Gradient | Mode::Rect | Mode::Idle => gradient(px, w, h, i * 37),
                Mode::Touch => fill(px, COLORS[0]),
                Mode::Scroll => {}
            }
        }
        if self.opts.mode == Mode::Scroll {
            let line = ((16.0 * self.scale * self.opts.res).round() as usize).max(4);
            self.page = text_page(w, (h * 2).max(line * 4), line);
        }
        self.buffers = Some(buffers);
        if let Some(v) = &self.viewport {
            v.set_destination(self.size.0, self.size.1);
        } else {
            self.surface.set_buffer_scale(self.scale.ceil() as i32);
        }
        self.shown = Some((self.size, self.scale));
        eprintln!(
            "pocketnix-gui-bench: window {}x{} at scale {}, buffer {width}x{height}",
            self.size.0, self.size.1, self.scale
        );
        Ok(())
    }

    /// Commits the next frame, if a buffer is free.
    fn draw(&mut self, input: Option<Instant>) {
        if self.shown != Some((self.size, self.scale)) {
            if let Err(e) = self.alloc() {
                eprintln!("pocketnix-gui-bench: {e:#}");
                self.closed = true;
                return;
            }
        }
        let mode = self.opts.mode;
        let frame = self.frame_no;
        let (scroll, rect, scale_res) = (self.opts.scroll as usize, self.opts.rect as f64, self.scale * self.opts.res);
        let bufs = self.buffers.as_mut().expect("allocated");
        let Some(i) = (0..BUFFERS).map(|k| (self.next_buffer + k) % BUFFERS).find(|i| !bufs.bufs[*i].1) else {
            self.m.starved += 1;
            return;
        };
        self.next_buffer = (i + 1) % BUFFERS;
        let (w, h) = (bufs.width, bufs.height);
        let start = Instant::now();
        let damage = match mode {
            Mode::Noise | Mode::Gradient | Mode::Idle => (0, 0, w, h),
            Mode::Touch => {
                let c = COLORS[(frame as usize) % COLORS.len()];
                fill(bufs.pixels(i), c);
                (0, 0, w, h)
            }
            Mode::Rect => {
                let side = ((rect * scale_res).round() as i32).clamp(1, w.min(h));
                let (x0, y0) = ((w - side) / 2, (h - side) / 2);
                let c = COLORS[(frame as usize) % COLORS.len()];
                let stride = bufs.stride as usize;
                let px = bufs.pixels(i);
                for y in y0..y0 + side {
                    let row = &mut px[y as usize * stride + x0 as usize * 4..][..side as usize * 4];
                    for p in row.chunks_exact_mut(4) {
                        p.copy_from_slice(&c);
                    }
                }
                (x0, y0, side, side)
            }
            Mode::Scroll => {
                let row = w as usize * 4;
                let rows = self.page.len() / row;
                let offset = (frame as usize * scroll) % rows;
                let stride = bufs.stride as usize;
                let px = bufs.pixels(i);
                for y in 0..h as usize {
                    let src = (offset + y) % rows;
                    px[y * stride..y * stride + row].copy_from_slice(&self.page[src * row..(src + 1) * row]);
                }
                (0, 0, w, h)
            }
        };
        self.m.render += start.elapsed();
        let buffer = &mut bufs.bufs[i];
        buffer.1 = true;
        self.surface.attach(Some(&buffer.0), 0, 0);
        self.surface.damage_buffer(damage.0, damage.1, damage.2, damage.3);
        self.surface.frame(&self.qh, frame);
        self.surface.commit();
        if self.timed.len() == MAX_TIMED {
            self.timed.pop_front();
        }
        self.timed.push_back((frame, Instant::now(), input));
        self.frame_no += 1;
        self.frame_pending = true;
        self.m.frames += 1;
    }

    fn frame_done(&mut self, frame: u64) {
        let now = Instant::now();
        self.first_presented = true;
        if frame + 1 == self.frame_no {
            self.frame_pending = false;
        }
        while let Some(&(n, at, input)) = self.timed.front() {
            if n > frame {
                break;
            }
            self.timed.pop_front();
            if n == frame {
                self.m.latency.record(now - at);
                if let Some(t) = input {
                    self.m.input_latency.record(now - t);
                }
            }
        }
        self.m.presented += 1;
        if let Some(last) = self.m.last_done.replace(now) {
            self.m.interval.record(now - last);
        }
    }

    fn input(&mut self) {
        if self.opts.mode == Mode::Touch && self.pending_input.is_none() {
            self.pending_input = Some(Instant::now());
        }
    }

    fn result(&self, secs: f64, before: &snapshot::Proc, after: &snapshot::Proc, sources: Value) -> Value {
        let (bw, bh) = self.buffers.as_ref().map_or((0, 0), |b| (b.width, b.height));
        let (user, system) = rusage();
        json!({
            "mode": format!("{:?}", self.opts.mode).to_lowercase(),
            "rate": self.opts.rate,
            "res": self.opts.res,
            "fullscreen": self.opts.fullscreen,
            "seconds": snapshot::round1(secs * 10.0) / 10.0,
            "window": {
                "logicalWidth": self.size.0,
                "logicalHeight": self.size.1,
                "scale": self.scale,
                "bufferWidth": bw,
                "bufferHeight": bh,
            },
            "frames": self.m.frames,
            "presented": self.m.presented,
            "fps": snapshot::round1(self.m.presented as f64 / secs),
            "missedTicks": self.m.missed,
            "starved": self.m.starved,
            "renderNs": self.m.render.as_nanos() as u64,
            "histograms": {
                "commitToCallback": self.m.latency.to_json(),
                "callbackInterval": self.m.interval.to_json(),
                "inputToCallback": self.m.input_latency.to_json(),
            },
            "cpu": {"userNs": user, "systemNs": system},
            "guest": snapshot::proc_diff(before, after, secs),
            "sources": sources,
        })
    }
}

/// One line for a human: frame rate, latency and the compositor's share.
fn summary(r: &Value) -> String {
    let q = |h: &Value, p: f64| snapshot::quantile_us(h, p).map_or("-".into(), |us| format!("{:.1}", us as f64 / 1000.0));
    let lat = &r["histograms"]["commitToCallback"];
    let mut s = format!(
        "{} {}x{}: {} fps, commit→callback p50 {} p95 {} ms",
        r["mode"].as_str().unwrap_or(""),
        r["window"]["bufferWidth"],
        r["window"]["bufferHeight"],
        r["fps"],
        q(lat, 0.5),
        q(lat, 0.95)
    );
    if let Some(c) = r["sources"].get("compositor") {
        let groups = c["counters"]["groupsSent"].as_u64().unwrap_or(0).max(1) as f64;
        let cpu = c["counters"]["cpuUserNs"].as_u64().unwrap_or(0) + c["counters"]["cpuSystemNs"].as_u64().unwrap_or(0);
        s += &format!(", compositor {:.1} ms CPU a frame", cpu as f64 / groups / 1e6);
    }
    s
}

fn rusage() -> (u64, u64) {
    // SAFETY: getrusage fills the struct it's given.
    let mut ru: libc::rusage = unsafe { std::mem::zeroed() };
    unsafe { libc::getrusage(libc::RUSAGE_SELF, &mut ru) };
    let ns = |t: libc::timeval| t.tv_sec as u64 * 1_000_000_000 + t.tv_usec as u64 * 1000;
    (ns(ru.ru_utime), ns(ru.ru_stime))
}

/// XRGB8888 in memory order: B, G, R, X.
const COLORS: [[u8; 4]; 4] = [[0x30, 0x30, 0xd0, 0xff], [0x30, 0xd0, 0x30, 0xff], [0xd0, 0x30, 0x30, 0xff], [0xd0, 0xd0, 0x30, 0xff]];

fn fill(px: &mut [u8], c: [u8; 4]) {
    for p in px.chunks_exact_mut(4) {
        p.copy_from_slice(&c);
    }
}

fn noise(px: &mut [u8], mut x: u64) {
    for chunk in px.chunks_exact_mut(8) {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        chunk.copy_from_slice(&x.to_le_bytes());
    }
}

fn gradient(px: &mut [u8], w: usize, h: usize, phase: usize) {
    for y in 0..h {
        for x in 0..w {
            let p = &mut px[(y * w + x) * 4..][..4];
            p.copy_from_slice(&[((x + phase) * 255 / w.max(1)) as u8, ((y + phase) * 255 / h.max(1)) as u8, 0x80, 0xff]);
        }
    }
}

/// Rows like a terminal's: lines of `line` rows, each with glyph-like runs
/// that make every glyph row unique, between blank rows.
fn text_page(w: usize, rows: usize, line: usize) -> Vec<u8> {
    let mut px = vec![0xf0u8; w * rows * 4];
    let glyph = (line / 2).max(2);
    let mut seed = 0x2545_f491_4f6c_dd1du64;
    for l in 0..rows / line {
        seed ^= seed << 13;
        seed ^= seed >> 7;
        seed ^= seed << 17;
        let width = (seed as usize % (w / glyph).max(1)).max(1);
        for gy in line / 4..line - line / 8 {
            let y = l * line + gy;
            for g in 0..width {
                let bits = (seed.rotate_left((g * 7 + gy) as u32 % 64) as usize) | 1;
                for gx in 0..glyph - 1 {
                    if (bits >> (gx % 16)) & 1 == 1 {
                        let x = g * glyph + gx;
                        px[(y * w + x) * 4..][..4].copy_from_slice(&[0x20, 0x20, 0x20, 0xff]);
                    }
                }
            }
        }
    }
    px
}

impl Dispatch<WlRegistry, GlobalListContents> for Bench {
    fn event(_: &mut Self, _: &WlRegistry, _: wayland_client::protocol::wl_registry::Event, _: &GlobalListContents, _: &Connection, _: &QueueHandle<Self>) {}
}

impl Dispatch<XdgWmBase, ()> for Bench {
    fn event(_: &mut Self, wm: &XdgWmBase, event: xdg_wm_base::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_wm_base::Event::Ping { serial } = event {
            wm.pong(serial);
        }
    }
}

impl Dispatch<XdgSurface, ()> for Bench {
    fn event(b: &mut Self, xdg: &XdgSurface, event: xdg_surface::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            xdg.ack_configure(serial);
            b.configured = true;
        }
    }
}

impl Dispatch<XdgToplevel, ()> for Bench {
    fn event(b: &mut Self, _: &XdgToplevel, event: xdg_toplevel::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            xdg_toplevel::Event::Configure { width, height, .. } => {
                b.size = if width > 0 && height > 0 { (width, height) } else if b.size.0 > 0 { b.size } else { DEFAULT_SIZE };
            }
            xdg_toplevel::Event::Close => b.closed = true,
            _ => {}
        }
    }
}

impl Dispatch<WlSurface, ()> for Bench {
    fn event(b: &mut Self, _: &WlSurface, event: wl_surface::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_surface::Event::PreferredBufferScale { factor } = event
            && !b.has_fractional
        {
            b.scale = factor.max(1) as f64;
        }
    }
}

impl Dispatch<WpFractionalScaleV1, ()> for Bench {
    fn event(b: &mut Self, _: &WpFractionalScaleV1, event: wp_fractional_scale_v1::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wp_fractional_scale_v1::Event::PreferredScale { scale } = event {
            b.scale = scale as f64 / 120.0;
        }
    }
}

impl Dispatch<WlCallback, u64> for Bench {
    fn event(b: &mut Self, _: &WlCallback, event: wl_callback::Event, frame: &u64, _: &Connection, _: &QueueHandle<Self>) {
        if let wl_callback::Event::Done { .. } = event {
            b.frame_done(*frame);
        }
    }
}

impl Dispatch<WlBuffer, (u32, usize)> for Bench {
    fn event(b: &mut Self, _: &WlBuffer, event: wl_buffer::Event, id: &(u32, usize), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_buffer::Event::Release = event
            && let Some(bufs) = b.buffers.as_mut().filter(|bufs| bufs.generation == id.0)
        {
            bufs.bufs[id.1].1 = false;
        }
    }
}

impl Dispatch<WlSeat, ()> for Bench {
    fn event(b: &mut Self, seat: &WlSeat, event: wl_seat::Event, _: &(), _: &Connection, qh: &QueueHandle<Self>) {
        if let wl_seat::Event::Capabilities { capabilities: WEnum::Value(caps) } = event {
            if caps.contains(wl_seat::Capability::Touch) && b.touch.is_none() {
                b.touch = Some(seat.get_touch(qh, ()));
            }
            if caps.contains(wl_seat::Capability::Pointer) && b.pointer.is_none() {
                b.pointer = Some(seat.get_pointer(qh, ()));
            }
        }
    }
}

impl Dispatch<WlTouch, ()> for Bench {
    fn event(b: &mut Self, _: &WlTouch, event: wl_touch::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_touch::Event::Down { .. } = event {
            b.input();
        }
    }
}

impl Dispatch<WlPointer, ()> for Bench {
    fn event(b: &mut Self, _: &WlPointer, event: wl_pointer::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_pointer::Event::Button { state: WEnum::Value(wl_pointer::ButtonState::Pressed), .. } = event {
            b.input();
        }
    }
}

delegate_noop!(Bench: ignore WlCompositor);
delegate_noop!(Bench: ignore WlShm);
delegate_noop!(Bench: ignore WlShmPool);
delegate_noop!(Bench: ignore WpViewporter);
delegate_noop!(Bench: ignore WpViewport);
delegate_noop!(Bench: ignore WpFractionalScaleManagerV1);
