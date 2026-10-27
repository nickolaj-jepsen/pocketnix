//! Drives the compositor binary through its sockets: Wayland clients draw,
//! and a GUI client decodes what the app would get. The decoder here follows
//! docs/gui-protocol.md on its own, so it checks the compositor against the
//! spec rather than against itself.

use std::collections::HashMap;
use std::io::BufRead;
use std::io::BufReader;
use std::io::Read;
use std::io::Write;
use std::os::fd::AsFd;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::fs::FileExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::process::Child;
use std::process::Command;
use std::process::Stdio;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;

use serde_json::Value;
use serde_json::json;
use wayland_client::Connection;
use wayland_client::Dispatch;
use wayland_client::EventQueue;
use wayland_client::QueueHandle;
use wayland_client::WEnum;
use wayland_client::globals::GlobalListContents;
use wayland_client::globals::registry_queue_init;
use wayland_client::protocol::wl_buffer::WlBuffer;
use wayland_client::protocol::wl_callback;
use wayland_client::protocol::wl_callback::WlCallback;
use wayland_client::protocol::wl_compositor::WlCompositor;
use wayland_client::protocol::wl_keyboard;
use wayland_client::protocol::wl_keyboard::WlKeyboard;
use wayland_client::protocol::wl_pointer;
use wayland_client::protocol::wl_pointer::WlPointer;
use wayland_client::protocol::wl_registry::WlRegistry;
use wayland_client::protocol::wl_seat;
use wayland_client::protocol::wl_seat::WlSeat;
use wayland_client::protocol::wl_shm;
use wayland_client::protocol::wl_shm::WlShm;
use wayland_client::protocol::wl_shm_pool::WlShmPool;
use wayland_client::protocol::wl_surface::WlSurface;
use wayland_protocols::xdg::shell::client::xdg_popup;
use wayland_protocols::xdg::shell::client::xdg_popup::XdgPopup;
use wayland_protocols::xdg::shell::client::xdg_positioner;
use wayland_protocols::xdg::shell::client::xdg_positioner::XdgPositioner;
use wayland_protocols::xdg::shell::client::xdg_surface;
use wayland_protocols::xdg::shell::client::xdg_surface::XdgSurface;
use wayland_protocols::xdg::shell::client::xdg_toplevel;
use wayland_protocols::xdg::shell::client::xdg_toplevel::XdgToplevel;
use wayland_protocols::xdg::shell::client::xdg_wm_base;
use wayland_protocols::xdg::shell::client::xdg_wm_base::XdgWmBase;

const TIMEOUT: Duration = Duration::from_secs(30);

const FLAG_BUFFER: u16 = 1;
const FLAG_FULL: u16 = 2;
const FLAG_SCROLL: u16 = 4;
const FLAG_HASH: u16 = 8;
const FLAG_GROUPED: u16 = 16;
const FLAG_HIDDEN: u16 = 32;

const BTN_LEFT: u32 = 0x110;
const KEY_A: u32 = 30;

fn wait_for(what: &str, mut f: impl FnMut() -> bool) {
    let deadline = Instant::now() + TIMEOUT;
    while !f() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(10));
    }
}

struct Compositor {
    child: Child,
    dir: PathBuf,
    log: Arc<Mutex<Vec<String>>>,
}

impl Compositor {
    fn start(name: &str) -> Self {
        let dir = std::env::temp_dir().join(format!("pnc-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        let mut child = Command::new(env!("CARGO_BIN_EXE_pocketnix-compositor"))
            .arg("--gui-socket")
            .arg(dir.join("gui.sock"))
            .args(["--wayland-display", "wl-test", "--log-hashes"])
            .env("XDG_RUNTIME_DIR", &dir)
            .env("RUST_LOG", "info")
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let log = Arc::new(Mutex::new(Vec::new()));
        let stderr = child.stderr.take().unwrap();
        let lines = log.clone();
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                eprintln!("compositor: {line}");
                lines.lock().unwrap().push(line);
            }
        });
        let c = Compositor { child, dir, log };
        wait_for("the compositor's sockets", || {
            c.dir.join("gui.sock").exists() && c.dir.join("wl-test").exists()
        });
        c
    }

    fn gui_socket(&self) -> PathBuf {
        self.dir.join("gui.sock")
    }

    fn wayland_socket(&self) -> PathBuf {
        self.dir.join("wl-test")
    }

    /// The `--log-hashes` line for a commit, waiting for it to arrive.
    fn logged_hash(&self, surface: u32, seq: u32) -> String {
        let prefix = format!("hash surface={surface} seq={seq} ");
        let mut found = None;
        wait_for("the compositor's hash line", || {
            found = self.log.lock().unwrap().iter().find_map(|l| l.strip_prefix(&prefix).map(str::to_owned));
            found.is_some()
        });
        found.unwrap()
    }
}

impl Drop for Compositor {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

/// The spec's content hash.
fn content_hash(bytes: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    let mut chunks = bytes.chunks_exact(8);
    for c in &mut chunks {
        h = (h ^ u64::from_le_bytes(c.try_into().unwrap()))
            .wrapping_mul(0x0100_0000_01b3)
            .rotate_left(29);
    }
    for b in chunks.remainder() {
        h = (h ^ *b as u64).wrapping_mul(0x0100_0000_01b3);
    }
    h
}

#[derive(Clone, Debug)]
struct Commit {
    flags: u16,
    surface: u32,
    window: u32,
    seq: u32,
    x: i32,
    y: i32,
    dst_width: u32,
    dst_height: u32,
    width: u32,
    height: u32,
    scroll: i32,
    rects: Vec<[u32; 4]>,
    hash: u64,
}

#[derive(Debug)]
enum Msg {
    Json(Value),
    Commit(Commit),
}

/// A surface's reference copy, as the app keeps it.
struct Layer {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
}

/// The app's side of a GUI connection.
struct App {
    stream: UnixStream,
    layers: HashMap<u32, Layer>,
    auto_ack: bool,
    hashed: usize,
}

fn be32(b: &[u8], at: usize) -> u32 {
    u32::from_be_bytes(b[at..at + 4].try_into().unwrap())
}

impl App {
    fn connect(compositor: &Compositor) -> Self {
        let stream = UnixStream::connect(compositor.gui_socket()).unwrap();
        stream.set_read_timeout(Some(TIMEOUT)).unwrap();
        let mut app = App {
            stream,
            layers: HashMap::new(),
            auto_ack: false,
            hashed: 0,
        };
        app.send_json(json!({"type": "hello", "version": 1, "minVersion": 1, "app": "test", "hashes": true}));
        app
    }

    fn send_frame(&mut self, ty: u8, payload: &[u8]) {
        let mut f = vec![ty];
        f.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        f.extend_from_slice(payload);
        self.stream.write_all(&f).unwrap();
    }

    fn send_json(&mut self, v: Value) {
        self.send_frame(0, &serde_json::to_vec(&v).unwrap());
    }

    fn send_binary(&mut self, ty: u8, fields: &[&[u8]]) {
        self.send_frame(ty, &fields.concat());
    }

    fn presented(&mut self, window: u32, seq: u32) {
        self.send_binary(0x10, &[&window.to_be_bytes(), &seq.to_be_bytes()]);
    }

    fn read_frame(&mut self) -> Option<(u8, Vec<u8>)> {
        let mut head = [0u8; 5];
        match self.stream.read_exact(&mut head) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return None,
            Err(e) => panic!("reading the GUI socket: {e}"),
        }
        let len = be32(&head, 1) as usize;
        assert!(len <= 1 << 20, "frame of {len} bytes");
        let mut payload = vec![0; len];
        self.stream.read_exact(&mut payload).unwrap();
        Some((head[0], payload))
    }

    fn next(&mut self) -> Msg {
        loop {
            let (ty, p) = self.read_frame().expect("the compositor closed the connection");
            match ty {
                0x00 => return Msg::Json(serde_json::from_slice(&p).unwrap()),
                0x01 => return Msg::Commit(self.commit(p)),
                0x02 => panic!("COMMIT_DATA without a COMMIT"),
                _ => {}
            }
        }
    }

    fn commit(&mut self, p: Vec<u8>) -> Commit {
        let header = u16::from_be_bytes([p[0], p[1]]) as usize;
        let rect_count = be32(&p, 68) as usize;
        let data_len = be32(&p, 72) as usize;
        let c = Commit {
            flags: u16::from_be_bytes([p[2], p[3]]),
            surface: be32(&p, 4),
            window: be32(&p, 8),
            seq: be32(&p, 12),
            x: be32(&p, 24) as i32,
            y: be32(&p, 28) as i32,
            dst_width: be32(&p, 32),
            dst_height: be32(&p, 36),
            width: be32(&p, 40),
            height: be32(&p, 44),
            scroll: be32(&p, 64) as i32,
            rects: (0..rect_count)
                .map(|i| {
                    let at = header + 16 * i;
                    [be32(&p, at), be32(&p, at + 4), be32(&p, at + 8), be32(&p, at + 12)]
                })
                .collect(),
            hash: u64::from_be_bytes(p[80..88].try_into().unwrap()),
        };
        let encoding = p[19];
        let mut raw = p[header + 16 * rect_count..].to_vec();
        while raw.len() < data_len {
            let (ty, more) = self.read_frame().expect("EOF inside a commit");
            assert_eq!(ty, 0x02, "a commit's frames must be consecutive");
            assert_eq!(be32(&more, 0), c.seq);
            raw.extend_from_slice(&more[4..]);
        }
        assert_eq!(raw.len(), data_len);

        if c.flags & FLAG_HIDDEN != 0 {
            self.layers.remove(&c.surface);
        }
        if c.flags & FLAG_BUFFER != 0 {
            let data = if encoding == 1 { zstd::stream::decode_all(&raw[..]).unwrap() } else { raw };
            let expected: u32 = c.rects.iter().map(|r| r[2] * r[3] * 4).sum();
            assert_eq!(data.len(), expected as usize, "decoded size of seq {}", c.seq);
            if c.flags & FLAG_FULL != 0 {
                assert_eq!(c.rects, vec![[0, 0, c.width, c.height]]);
                self.layers.insert(
                    c.surface,
                    Layer {
                        width: c.width,
                        height: c.height,
                        pixels: data,
                    },
                );
            } else {
                let layer = self.layers.get_mut(&c.surface).expect("a patch for a surface without content");
                assert_eq!((layer.width, layer.height), (c.width, c.height));
                let row = layer.width as usize * 4;
                if c.flags & FLAG_SCROLL != 0 {
                    let old = layer.pixels.clone();
                    for y in 0..layer.height as i64 {
                        let sy = y + c.scroll as i64;
                        if (0..layer.height as i64).contains(&sy) {
                            layer.pixels[y as usize * row..(y as usize + 1) * row]
                                .copy_from_slice(&old[sy as usize * row..(sy as usize + 1) * row]);
                        }
                    }
                }
                let mut at = 0;
                for [x, y, w, h] in &c.rects {
                    for yy in *y..y + h {
                        let start = yy as usize * row + *x as usize * 4;
                        let n = *w as usize * 4;
                        layer.pixels[start..start + n].copy_from_slice(&data[at..at + n]);
                        at += n;
                    }
                }
            }
            if c.flags & FLAG_HASH != 0 {
                let layer = &self.layers[&c.surface];
                assert_eq!(content_hash(&layer.pixels), c.hash, "content hash of seq {}", c.seq);
                self.hashed += 1;
            }
        }
        if self.auto_ack && c.flags & FLAG_GROUPED == 0 {
            self.presented(c.window, c.seq);
        }
        c
    }

    fn wait<T>(&mut self, what: &str, mut f: impl FnMut(&Msg) -> Option<T>) -> T {
        let deadline = Instant::now() + TIMEOUT;
        loop {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            let msg = self.next();
            if let Some(t) = f(&msg) {
                return t;
            }
        }
    }

    fn wait_json(&mut self, ty: &str) -> Value {
        self.wait(ty, |m| match m {
            Msg::Json(v) if v["type"] == ty => Some(v.clone()),
            _ => None,
        })
    }

    fn wait_commit(&mut self, what: &str, f: impl Fn(&Commit) -> bool) -> Commit {
        self.wait(what, |m| match m {
            Msg::Commit(c) if f(c) => Some(c.clone()),
            _ => None,
        })
    }
}

#[derive(Default)]
struct Wl {
    configure: Option<u32>,
    popup_configure: Option<u32>,
    /// The popup's last configure: x, y, width, height.
    popup_geometry: Option<(i32, i32, i32, i32)>,
    size: (i32, i32),
    states: Vec<u32>,
    frames: u32,
    pointer: Vec<String>,
    keys: Vec<(u32, u32)>,
    mods_depressed: u32,
    keyboard_focus: bool,
}

/// A Wayland client drawing known pixels with known damage.
struct Pattern {
    conn: Connection,
    queue: EventQueue<Wl>,
    state: Wl,
    qh: QueueHandle<Wl>,
    globals: Vec<String>,
    compositor: WlCompositor,
    shm: WlShm,
    wm_base: XdgWmBase,
    surface: WlSurface,
    xdg_surface: XdgSurface,
    file: std::fs::File,
    buffers: Vec<WlBuffer>,
    next: usize,
    width: i32,
    height: i32,
}

impl Pattern {
    fn connect(compositor: &Compositor, width: i32, height: i32) -> Self {
        let conn = Connection::from_socket(UnixStream::connect(compositor.wayland_socket()).unwrap()).unwrap();
        let (globals, queue) = registry_queue_init::<Wl>(&conn).unwrap();
        let qh = queue.handle();
        let compositor: WlCompositor = globals.bind(&qh, 4..=6, ()).unwrap();
        let shm: WlShm = globals.bind(&qh, 1..=1, ()).unwrap();
        let wm_base: XdgWmBase = globals.bind(&qh, 1..=6, ()).unwrap();
        let seat: WlSeat = globals.bind(&qh, 1..=7, ()).unwrap();
        let _ = seat;
        let names = globals.contents().with_list(|list| list.iter().map(|g| g.interface.clone()).collect());

        // SAFETY: memfd_create with a valid C string.
        let fd = unsafe { libc::memfd_create(c"pattern".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: a fresh descriptor we own.
        let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
        let size = (width * height * 4) as u64;
        file.set_len(2 * size).unwrap();
        let pool: WlShmPool = shm.create_pool(file.as_fd(), 2 * size as i32, &qh, ());
        let buffers = (0..2)
            .map(|i| pool.create_buffer(i * size as i32, width, height, width * 4, wl_shm::Format::Argb8888, &qh, ()))
            .collect();

        let surface = compositor.create_surface(&qh, ());
        let xdg_surface = wm_base.get_xdg_surface(&surface, &qh, ());
        let toplevel = xdg_surface.get_toplevel(&qh, ());
        toplevel.set_title("pattern".into());
        toplevel.set_app_id("test.pattern".into());
        surface.commit();
        let mut p = Pattern {
            conn,
            queue,
            state: Wl::default(),
            qh,
            globals: names,
            compositor,
            shm,
            wm_base,
            surface,
            xdg_surface,
            file,
            buffers,
            next: 0,
            width,
            height,
        };
        p.dispatch_until("the initial configure", |s| s.configure.is_some());
        p
    }

    fn dispatch_until(&mut self, what: &str, f: impl Fn(&Wl) -> bool) {
        let deadline = Instant::now() + TIMEOUT;
        while !f(&self.state) {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            self.queue.roundtrip(&mut self.state).unwrap();
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    fn ack(&mut self) {
        if let Some(serial) = self.state.configure.take() {
            self.xdg_surface.ack_configure(serial);
        }
    }

    /// Attaches `pixels` (ARGB8888 rows) with buffer damage `damage` and
    /// commits, optionally asking for a frame callback.
    fn draw(&mut self, pixels: &[u8], damage: [i32; 4], frame: bool) {
        self.ack();
        let size = (self.width * self.height * 4) as u64;
        self.file.write_all_at(pixels, self.next as u64 * size).unwrap();
        self.surface.attach(Some(&self.buffers[self.next]), 0, 0);
        self.next = 1 - self.next;
        self.surface.damage_buffer(damage[0], damage[1], damage[2], damage[3]);
        if frame {
            self.surface.frame(&self.qh, ());
        }
        self.surface.commit();
        self.conn.flush().unwrap();
    }

    /// Opens a popup on the toplevel that wants `size` below and right of
    /// `anchor`, allowing no constraint adjustment, and draws it at the size
    /// it's configured with. Returns that configure's x, y, width, height.
    fn popup(&mut self, anchor: [i32; 4], size: (i32, i32)) -> (i32, i32, i32, i32) {
        let surface = self.compositor.create_surface(&self.qh, ());
        let xdg_surface = self.wm_base.get_xdg_surface(&surface, &self.qh, 1u32);
        let positioner = self.wm_base.create_positioner(&self.qh, ());
        positioner.set_size(size.0, size.1);
        positioner.set_anchor_rect(anchor[0], anchor[1], anchor[2], anchor[3]);
        positioner.set_anchor(xdg_positioner::Anchor::BottomRight);
        positioner.set_gravity(xdg_positioner::Gravity::BottomRight);
        xdg_surface.get_popup(Some(&self.xdg_surface), &positioner, &self.qh, ());
        surface.commit();
        self.dispatch_until("the popup's configure", |s| s.popup_configure.is_some());
        xdg_surface.ack_configure(self.state.popup_configure.take().unwrap());
        let geometry = self.state.popup_geometry.unwrap();
        let (w, h) = (geometry.2, geometry.3);

        // SAFETY: memfd_create with a valid C string.
        let fd = unsafe { libc::memfd_create(c"popup".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: a fresh descriptor we own.
        let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
        file.write_all_at(&pattern(w, h, 7), 0).unwrap();
        let pool = self.shm.create_pool(file.as_fd(), w * h * 4, &self.qh, ());
        let buffer = pool.create_buffer(0, w, h, w * 4, wl_shm::Format::Argb8888, &self.qh, ());
        surface.attach(Some(&buffer), 0, 0);
        surface.damage_buffer(0, 0, w, h);
        surface.commit();
        self.conn.flush().unwrap();
        geometry
    }

    fn roundtrip(&mut self) {
        self.queue.roundtrip(&mut self.state).unwrap();
    }
}

/// The wire's RGBA for ARGB8888 rows.
fn to_rgba(argb: &[u8]) -> Vec<u8> {
    argb.chunks(4).flat_map(|p| [p[2], p[1], p[0], p[3]]).collect()
}

fn pattern(width: i32, height: i32, seed: u32) -> Vec<u8> {
    let mut out = Vec::with_capacity((width * height * 4) as usize);
    for y in 0..height as u32 {
        for x in 0..width as u32 {
            let v = (y + seed).wrapping_mul(2654435761) ^ x.wrapping_mul(40503);
            out.extend_from_slice(&[v as u8, (v >> 8) as u8, (v >> 16) as u8, 0xff]);
        }
    }
    out
}

#[test]
fn a_client_window_reaches_the_app_with_matching_pixels() {
    let compositor = Compositor::start("pattern");
    let mut app = App::connect(&compositor);
    let hello = app.wait_json("hello");
    assert_eq!(hello["version"], 1);
    let instance = hello["instance"].as_str().unwrap().to_owned();
    assert!(!instance.is_empty());
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 2.0, "refresh": 60000}));

    // The output message must land before the toplevel's first configure.
    std::thread::sleep(Duration::from_millis(100));
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    assert_eq!(client.state.size, (400, 300), "the first configure fills the output");

    let mut content = pattern(w, h, 0);
    client.draw(&content, [0, 0, w, h], false);
    let toplevel = app.wait_json("toplevel");
    assert_eq!(toplevel["appId"], "test.pattern");
    assert_eq!(toplevel["title"], "pattern");
    assert_eq!((toplevel["width"].as_i64(), toplevel["height"].as_i64()), (Some(400), Some(200)));
    let window = toplevel["id"].as_u64().unwrap() as u32;
    let surface = app.wait_json("surface");
    assert_eq!(surface["role"], "toplevel");
    assert_eq!(surface["window"], window);
    let full = app.wait_commit("the first commit", |c| c.surface == window);
    assert_eq!(full.flags & (FLAG_BUFFER | FLAG_FULL | FLAG_HASH), FLAG_BUFFER | FLAG_FULL | FLAG_HASH);
    assert_eq!((full.x, full.y, full.dst_width, full.dst_height), (0, 0, 400, 200));
    assert_eq!(app.layers[&window].pixels, to_rgba(&content));
    assert_eq!(compositor.logged_hash(window, full.seq), format!("{w}x{h} {:016x}", full.hash));

    // A small change sends only its rect.
    for y in 20..25 {
        for x in 30..40 {
            content[((y * w + x) * 4) as usize] ^= 0x5a;
        }
    }
    client.draw(&content, [30, 20, 10, 5], false);
    let patch = app.wait_commit("a patch", |c| c.surface == window);
    assert_eq!(patch.flags & (FLAG_BUFFER | FLAG_FULL), FLAG_BUFFER);
    assert_eq!(patch.rects, vec![[30, 20, 10, 5]]);
    assert_eq!(app.layers[&window].pixels, to_rgba(&content));

    // Content moving up by 7 rows comes as a scroll and the new rows.
    let row = (w * 4) as usize;
    let fresh = pattern(w, 7, 1000);
    let mut scrolled = content[7 * row..].to_vec();
    scrolled.extend_from_slice(&fresh);
    client.draw(&scrolled, [0, 0, w, h], false);
    let scroll = app.wait_commit("a scroll", |c| c.surface == window);
    assert_eq!(scroll.flags & (FLAG_BUFFER | FLAG_FULL | FLAG_SCROLL), FLAG_BUFFER | FLAG_SCROLL);
    assert_eq!(scroll.scroll, 7);
    assert_eq!(scroll.rects, vec![[0, (h - 7) as u32, w as u32, 7]]);
    assert_eq!(app.layers[&window].pixels, to_rgba(&scrolled));

    // Frame callbacks wait for the app to present.
    client.draw(&scrolled, [0, 0, 0, 0], true);
    let paced = app.wait_commit("a callback's commit", |c| c.surface == window);
    client.roundtrip();
    std::thread::sleep(Duration::from_millis(100));
    client.roundtrip();
    assert_eq!(client.state.frames, 0, "a frame callback fired before PRESENTED");
    app.presented(window, paced.seq);
    client.dispatch_until("the frame callback", |s| s.frames == 1);

    // Window state: size and focus, then input in window pixels.
    app.send_json(json!({"type": "window", "id": window, "width": 600, "height": 400, "scale": 2.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("the window's configure", |s| s.size == (300, 200) && s.keyboard_focus);
    assert!(client.state.states.contains(&(xdg_toplevel::State::Activated as u32)));
    app.send_binary(0x20, &[&window.to_be_bytes(), &100f32.to_be_bytes(), &50f32.to_be_bytes()]);
    app.send_binary(
        0x21,
        &[&window.to_be_bytes(), &100f32.to_be_bytes(), &50f32.to_be_bytes(), &BTN_LEFT.to_be_bytes(), &[1]],
    );
    client.dispatch_until("pointer events", |s| s.pointer.iter().any(|e| e.starts_with("button")));
    assert_eq!(client.state.pointer[0], "enter 50 25");
    assert!(client.state.pointer.contains(&format!("button {BTN_LEFT} 1")));
    app.send_binary(0x31, &[&window.to_be_bytes(), &[0b10]]);
    client.dispatch_until("ctrl", |s| s.mods_depressed != 0);
    app.send_binary(0x30, &[&window.to_be_bytes(), &KEY_A.to_be_bytes(), &[1]]);
    app.send_binary(0x30, &[&window.to_be_bytes(), &KEY_A.to_be_bytes(), &[0]]);
    client.dispatch_until("a key", |s| s.keys == vec![(KEY_A, 1), (KEY_A, 0)]);

    // A new connection replaces this one and replays the window whole.
    let last_hash = content_hash(&app.layers[&window].pixels);
    let mut second = App::connect(&compositor);
    assert_eq!(second.wait_json("hello")["instance"], instance.as_str());
    assert_eq!(second.wait_json("toplevel")["id"], window);
    let replay = second.wait_commit("the replay", |c| c.surface == window);
    assert_eq!(replay.flags & (FLAG_BUFFER | FLAG_FULL), FLAG_BUFFER | FLAG_FULL);
    assert_eq!(replay.hash, last_hash);
    second.wait_json("ready");
    let deadline = Instant::now() + TIMEOUT;
    while app.read_frame().is_some() {
        assert!(Instant::now() < deadline, "the old connection stayed open");
    }
}

#[test]
fn popups_stay_inside_their_window_and_data_control_is_offered() {
    let compositor = Compositor::start("popup");
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 400, "scale": 2.0, "refresh": 60000}));
    std::thread::sleep(Duration::from_millis(100));

    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    for global in ["zwlr_data_control_manager_v1", "ext_data_control_manager_v1"] {
        assert!(client.globals.iter().any(|g| g == global), "{global} missing from {:?}", client.globals);
    }
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let window = app.wait_json("toplevel")["id"].as_u64().unwrap() as u32;
    app.wait_commit("the toplevel's first commit", |c| c.surface == window);

    // Unconstrained it would hang 80 px past the right edge and 50 past the bottom.
    let geometry = client.popup([150, 60, 10, 10], (120, 80));
    assert_eq!(geometry, (80, 20, 120, 80));
    let surface = app.wait("the popup's surface", |m| match m {
        Msg::Json(v) if v["type"] == "surface" && v["role"] == "popup" => Some(v.clone()),
        _ => None,
    });
    assert_eq!((surface["window"].as_u64(), surface["parent"].as_u64()), (Some(window as u64), Some(window as u64)));
    let id = surface["id"].as_u64().unwrap() as u32;
    let commit = app.wait_commit("the popup's commit", |c| c.surface == id && c.flags & FLAG_BUFFER != 0);
    assert_eq!(commit.window, window);
    // Window pixels at scale 2, inside the 400x200 window.
    assert_eq!((commit.x, commit.y, commit.dst_width, commit.dst_height), (160, 40, 240, 160));
    assert_eq!(app.layers[&id].pixels, to_rgba(&pattern(120, 80, 7)));

    // A window drawn larger than it was configured shows only the configured part.
    app.send_json(json!({"type": "window", "id": window, "width": 240, "height": 120, "scale": 2.0,
        "focused": true, "visible": true, "maximized": true, "fullscreen": false}));
    client.dispatch_until("the smaller configure", |s| s.size == (120, 60));
    client.draw(&pattern(w, h, 1), [0, 0, w, h], false);
    client.roundtrip();
    assert_eq!(client.popup([150, 60, 10, 10], (120, 80)), (0, 0, 120, 60));
}

/// foot, a real client: set FOOT to its binary (with fonts reachable) to run.
#[test]
fn foot_renders_and_its_hashes_match() {
    let Ok(foot) = std::env::var("FOOT") else {
        eprintln!("FOOT is not set; skipping");
        return;
    };
    let compositor = Compositor::start("foot");
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 1080, "height": 2400, "scale": 2.625, "refresh": 60000}));
    std::thread::sleep(Duration::from_millis(100));

    let mut child = Command::new(foot)
        .args(["--override", "pad=0x0", "sh", "-c", "echo pocketnix; exec cat"])
        .env("XDG_RUNTIME_DIR", &compositor.dir)
        .env("WAYLAND_DISPLAY", "wl-test")
        .env_remove("DISPLAY")
        .stdout(Stdio::null())
        .spawn()
        .unwrap();
    let toplevel = app.wait_json("toplevel");
    assert_eq!(toplevel["appId"], "foot");
    let window = toplevel["id"].as_u64().unwrap() as u32;
    let first = app.wait_commit("foot's first frame", |c| c.window == window && c.flags & FLAG_BUFFER != 0);
    assert_eq!(compositor.logged_hash(first.surface, first.seq), format!("{}x{} {:016x}", first.width, first.height, first.hash));
    // Fractional scale: foot renders at 2.625 itself, so its buffer matches the window's pixels.
    assert_eq!((first.width, first.height), (first.dst_width, first.dst_height));

    app.send_json(json!({"type": "window", "id": window, "width": 1080, "height": 2400, "scale": 2.625,
        "focused": true, "visible": true, "maximized": true, "fullscreen": false}));
    let before = app.hashed;
    for key in [35u32, 18, 38, 38, 24] {
        app.send_binary(0x30, &[&window.to_be_bytes(), &key.to_be_bytes(), &[1]]);
        app.send_binary(0x30, &[&window.to_be_bytes(), &key.to_be_bytes(), &[0]]);
    }
    let patch = app.wait_commit("a patch from typing", |c| {
        c.window == window && c.flags & FLAG_BUFFER != 0 && c.flags & FLAG_FULL == 0
    });
    assert!(app.hashed > before);
    assert_eq!(compositor.logged_hash(patch.surface, patch.seq), format!("{}x{} {:016x}", patch.width, patch.height, patch.hash));
    let _ = child.kill();
    let _ = child.wait();
}

impl Dispatch<WlRegistry, GlobalListContents> for Wl {
    fn event(_: &mut Self, _: &WlRegistry, _: <WlRegistry as wayland_client::Proxy>::Event, _: &GlobalListContents, _: &Connection, _: &QueueHandle<Self>) {}
}

wayland_client::delegate_noop!(Wl: ignore WlCompositor);
wayland_client::delegate_noop!(Wl: ignore WlShm);
wayland_client::delegate_noop!(Wl: ignore WlShmPool);
wayland_client::delegate_noop!(Wl: ignore WlBuffer);
wayland_client::delegate_noop!(Wl: ignore WlSurface);

impl Dispatch<XdgWmBase, ()> for Wl {
    fn event(_: &mut Self, base: &XdgWmBase, event: xdg_wm_base::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_wm_base::Event::Ping { serial } = event {
            base.pong(serial);
        }
    }
}

impl Dispatch<XdgSurface, ()> for Wl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.configure = Some(serial);
        }
    }
}

impl Dispatch<XdgSurface, u32> for Wl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &u32, _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.popup_configure = Some(serial);
        }
    }
}

impl Dispatch<XdgPopup, ()> for Wl {
    fn event(state: &mut Self, _: &XdgPopup, event: xdg_popup::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_popup::Event::Configure { x, y, width, height } = event {
            state.popup_geometry = Some((x, y, width, height));
        }
    }
}

wayland_client::delegate_noop!(Wl: ignore XdgPositioner);

impl Dispatch<XdgToplevel, ()> for Wl {
    fn event(state: &mut Self, _: &XdgToplevel, event: xdg_toplevel::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_toplevel::Event::Configure { width, height, states } = event {
            state.size = (width, height);
            state.states = states.chunks(4).map(|c| u32::from_ne_bytes(c.try_into().unwrap())).collect();
        }
    }
}

impl Dispatch<WlCallback, ()> for Wl {
    fn event(state: &mut Self, _: &WlCallback, event: wl_callback::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_callback::Event::Done { .. } = event {
            state.frames += 1;
        }
    }
}

impl Dispatch<WlSeat, ()> for Wl {
    fn event(_: &mut Self, seat: &WlSeat, event: wl_seat::Event, _: &(), _: &Connection, qh: &QueueHandle<Self>) {
        if let wl_seat::Event::Capabilities { capabilities: WEnum::Value(caps) } = event {
            if caps.contains(wl_seat::Capability::Pointer) {
                seat.get_pointer(qh, ());
            }
            if caps.contains(wl_seat::Capability::Keyboard) {
                seat.get_keyboard(qh, ());
            }
        }
    }
}

impl Dispatch<WlPointer, ()> for Wl {
    fn event(state: &mut Self, _: &WlPointer, event: wl_pointer::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            wl_pointer::Event::Enter { surface_x, surface_y, .. } => {
                state.pointer.push(format!("enter {surface_x} {surface_y}"))
            }
            wl_pointer::Event::Button { button, state: WEnum::Value(s), .. } => {
                state.pointer.push(format!("button {button} {}", s as u32))
            }
            _ => {}
        }
    }
}

impl Dispatch<WlKeyboard, ()> for Wl {
    fn event(state: &mut Self, _: &WlKeyboard, event: wl_keyboard::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            wl_keyboard::Event::Enter { .. } => state.keyboard_focus = true,
            wl_keyboard::Event::Leave { .. } => state.keyboard_focus = false,
            wl_keyboard::Event::Key { key, state: WEnum::Value(s), .. } => state.keys.push((key, s as u32)),
            wl_keyboard::Event::Modifiers { mods_depressed, .. } => state.mods_depressed = mods_depressed,
            _ => {}
        }
    }
}
