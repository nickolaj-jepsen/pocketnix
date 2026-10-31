//! A stand-in for the app on a GUI socket, for benchmarks without a phone:
//! it decodes commits into reference copies and window buffers about as the
//! app does, and acknowledges each group at the next tick of a display
//! clock. It is no decoder to test against (the compositor's tests have
//! one): it trusts what it reads.

use std::collections::HashMap;
use std::collections::HashSet;
use std::io::Read;
use std::io::Write;
use std::os::unix::net::UnixListener;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;
use serde_json::Value;
use serde_json::json;

use crate::guest_ram::GuestRam;
use crate::parsed;
use crate::protocol;
use crate::stats::Stats;
use crate::value;

/// The app shows a surface from this many buffers in rotation.
const SLOTS: usize = 3;

struct Opts {
    socket: PathBuf,
    width: u32,
    height: u32,
    scale: f64,
    refresh: u32,
    stats_socket: Option<PathBuf>,
    /// Stands in for guest RAM, serving frame buffers on this socket.
    guest_ram: Option<PathBuf>,
    decode: bool,
}

struct Surface {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
    slots: Vec<Vec<u8>>,
    next: usize,
}

type Shared = Arc<Mutex<Stats>>;

pub fn run(args: &[String]) -> Result<()> {
    let mut o = Opts {
        socket: PathBuf::new(),
        width: 1008,
        height: 2244,
        scale: 2.625,
        refresh: 120_000,
        stats_socket: None,
        guest_ram: None,
        decode: true,
    };
    let mut it = args.iter();
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--socket" => o.socket = value(&mut it, arg)?.into(),
            "--width" => o.width = parsed(&mut it, arg)?,
            "--height" => o.height = parsed(&mut it, arg)?,
            "--scale" => o.scale = parsed(&mut it, arg)?,
            "--refresh" => o.refresh = parsed(&mut it, arg)?,
            "--stats-socket" => o.stats_socket = Some(value(&mut it, arg)?.into()),
            "--guest-ram" => o.guest_ram = Some(value(&mut it, arg)?.into()),
            "--no-decode" => o.decode = false,
            _ => bail!("unknown argument {arg:?}"),
        }
    }
    if o.socket.as_os_str().is_empty() {
        bail!("sink needs --socket");
    }
    let stats: Shared = Arc::new(Mutex::new(Stats::default()));
    if let Some(path) = &o.stats_socket {
        serve_stats(path, stats.clone())?;
    }
    let ram = o.guest_ram.as_deref().map(GuestRam::serve).transpose()?;
    let stream = UnixStream::connect(&o.socket).with_context(|| format!("connecting to {}", o.socket.display()))?;
    let out = Arc::new(Mutex::new(stream.try_clone()?));
    send_json(
        &out,
        &json!({"type": "hello", "version": protocol::VERSION, "minVersion": protocol::MIN_VERSION, "app": "pocketnix-gui-bench sink",
            "hashes": false, "guestRam": ram.is_some()}),
    )?;
    let pending: Arc<Mutex<Vec<(u32, u32, Instant)>>> = Arc::default();
    ticker(o.refresh, out.clone(), pending.clone(), stats.clone());
    read_loop(&o, ram.as_ref(), stream, &out, &pending, &stats)
}

fn serve_stats(path: &std::path::Path, stats: Shared) -> Result<()> {
    let _ = std::fs::remove_file(path);
    let listener = UnixListener::bind(path).with_context(|| format!("binding {}", path.display()))?;
    std::thread::spawn(move || {
        for mut s in listener.incoming().flatten() {
            let line = stats.lock().unwrap().to_json("sink", json!([])).to_string();
            let _ = s.write_all(format!("{line}\n").as_bytes());
        }
    });
    Ok(())
}

/// Acknowledges every finished group at each tick, as the app does once
/// SurfaceFlinger has committed the transaction carrying it.
fn ticker(refresh_mhz: u32, out: Arc<Mutex<UnixStream>>, pending: Arc<Mutex<Vec<(u32, u32, Instant)>>>, stats: Shared) {
    let period = Duration::from_secs_f64(1000.0 / refresh_mhz.max(1000) as f64);
    std::thread::spawn(move || {
        let mut next = Instant::now() + period;
        loop {
            std::thread::sleep(next.saturating_duration_since(Instant::now()));
            next += period;
            let due = std::mem::take(&mut *pending.lock().unwrap());
            // The newest seq per window covers the older ones.
            let mut last: HashMap<u32, u32> = HashMap::new();
            let now = Instant::now();
            {
                let mut s = stats.lock().unwrap();
                for (window, seq, at) in &due {
                    last.insert(*window, *seq);
                    s.record("receivedToPresented", now - *at);
                }
            }
            for (window, seq) in last {
                let mut p = window.to_be_bytes().to_vec();
                p.extend_from_slice(&seq.to_be_bytes());
                if send(&out, protocol::PRESENTED, &p).is_err() {
                    return;
                }
                stats.lock().unwrap().add("presentedSent", 1);
            }
        }
    });
}

fn send(out: &Mutex<UnixStream>, ty: u8, payload: &[u8]) -> std::io::Result<()> {
    let mut f = vec![ty];
    f.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    f.extend_from_slice(payload);
    out.lock().unwrap().write_all(&f)
}

fn send_json(out: &Mutex<UnixStream>, v: &Value) -> Result<()> {
    Ok(send(out, protocol::JSON, &serde_json::to_vec(v)?)?)
}

fn read_frame(stream: &mut UnixStream) -> Result<Option<(u8, Vec<u8>)>> {
    let mut head = [0u8; 5];
    match stream.read_exact(&mut head) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(e) => return Err(e.into()),
    }
    let len = u32::from_be_bytes(head[1..5].try_into().unwrap()) as usize;
    let mut payload = vec![0; len];
    stream.read_exact(&mut payload)?;
    Ok(Some((head[0], payload)))
}

fn be32(b: &[u8], at: usize) -> u32 {
    u32::from_be_bytes(b[at..at + 4].try_into().unwrap())
}

fn read_loop(
    o: &Opts,
    ram: Option<&GuestRam>,
    mut stream: UnixStream,
    out: &Mutex<UnixStream>,
    pending: &Mutex<Vec<(u32, u32, Instant)>>,
    stats: &Shared,
) -> Result<()> {
    let mut surfaces: HashMap<u32, Surface> = HashMap::new();
    let mut windows: HashSet<u32> = HashSet::new();
    let mut decoder = zstd::bulk::Decompressor::new()?;
    let mut data = Vec::new();
    let mut group_start: Option<Instant> = None;
    while let Some((ty, p)) = read_frame(&mut stream)? {
        match ty {
            protocol::JSON => {
                let msg: Value = serde_json::from_slice(&p)?;
                match msg["type"].as_str() {
                    Some("hello") => send_json(
                        out,
                        &json!({"type": "output", "width": o.width, "height": o.height, "scale": o.scale, "refresh": o.refresh}),
                    )?,
                    Some("toplevel") if msg["parent"] == 0 && msg["id"] == msg["window"] => {
                        let id = msg["id"].as_u64().unwrap_or(0) as u32;
                        if windows.insert(id) {
                            send_json(out, &json!({"type": "window", "id": id, "width": o.width, "height": o.height,
                                "scale": o.scale, "focused": true, "visible": true, "maximized": false, "fullscreen": false}))?;
                        }
                    }
                    Some("toplevel-removed") => {
                        windows.remove(&(msg["id"].as_u64().unwrap_or(0) as u32));
                    }
                    Some("error") => bail!("compositor error: {}", msg["message"]),
                    _ => {}
                }
            }
            protocol::COMMIT => {
                let received = Instant::now();
                let start = *group_start.get_or_insert(received);
                let header = u16::from_be_bytes([p[0], p[1]]) as usize;
                let flags = u16::from_be_bytes([p[2], p[3]]);
                let (surface, window, seq) = (be32(&p, 4), be32(&p, 8), be32(&p, 12));
                let (width, height) = (be32(&p, 40), be32(&p, 44));
                let scroll = be32(&p, 64) as i32;
                let rect_count = be32(&p, 68) as usize;
                let data_len = be32(&p, 72) as usize;
                let encoding = p[19];
                let rects: Vec<[u32; 4]> = (0..rect_count)
                    .map(|i| {
                        let at = header + 16 * i;
                        [be32(&p, at), be32(&p, at + 4), be32(&p, at + 8), be32(&p, at + 12)]
                    })
                    .collect();
                let mut raw = p[header + 16 * rect_count..].to_vec();
                while raw.len() < data_len {
                    let Some((_, more)) = read_frame(&mut stream)? else { bail!("EOF inside a commit") };
                    raw.extend_from_slice(&more[4..]);
                }
                let mut s = stats.lock().unwrap();
                s.add("commits", 1);
                s.add("wireBytes", raw.len() as u64);
                drop(s);
                if flags & protocol::FLAG_HIDDEN != 0 {
                    surfaces.remove(&surface);
                }
                if flags & protocol::FLAG_BUFFER != 0 && encoding == protocol::ENCODING_PAGES {
                    stats.lock().unwrap().add("commitsPages", 1);
                }
                if flags & protocol::FLAG_BUFFER != 0 && o.decode {
                    let pixel_bytes: usize = rects.iter().map(|r| (r[2] * r[3] * 4) as usize).sum();
                    let t = Instant::now();
                    let src: &[u8] = match encoding {
                        protocol::ENCODING_ZSTD => {
                            data.clear();
                            data.reserve(pixel_bytes);
                            decoder.decompress_to_buffer(&raw[..], &mut data)?;
                            &data
                        }
                        protocol::ENCODING_PAGES => {
                            ram.context("a PAGES commit without guest RAM")?.gather(&raw, pixel_bytes, &mut data)?;
                            &data
                        }
                        _ => &raw,
                    };
                    let decoded = t.elapsed();
                    let t = Instant::now();
                    apply(&mut surfaces, surface, width, height, flags, scroll, &rects, src);
                    let mut s = stats.lock().unwrap();
                    s.add("pixelBytes", pixel_bytes as u64);
                    s.time("decodeNs", decoded);
                    s.time("applyNs", t.elapsed());
                }
                // Copied out, or never to be: the compositor may write those pages again.
                if flags & protocol::FLAG_BUFFER != 0 && encoding == protocol::ENCODING_PAGES {
                    send(out, protocol::RELEASED, &seq.to_be_bytes())?;
                }
                if flags & protocol::FLAG_GROUPED == 0 {
                    group_start = None;
                    let mut s = stats.lock().unwrap();
                    s.add("groups", 1);
                    s.record("groupWork", start.elapsed());
                    drop(s);
                    pending.lock().unwrap().push((window, seq, start));
                }
            }
            _ => {}
        }
    }
    Ok(())
}

/// Applies a BUFFER commit to the surface's reference copy, then copies
/// what changed into its next buffer.
#[allow(clippy::too_many_arguments)]
fn apply(surfaces: &mut HashMap<u32, Surface>, id: u32, width: u32, height: u32, flags: u16, scroll: i32, rects: &[[u32; 4]], src: &[u8]) {
    let row = width as usize * 4;
    let size = row * height as usize;
    let s = surfaces.entry(id).or_insert_with(|| Surface { width, height, pixels: Vec::new(), slots: Vec::new(), next: 0 });
    if flags & protocol::FLAG_FULL != 0 || s.width != width || s.height != height {
        s.width = width;
        s.height = height;
        s.pixels.resize(size, 0);
        s.slots = vec![vec![0; size]; SLOTS];
    }
    if flags & protocol::FLAG_SCROLL != 0 {
        let by = scroll.unsigned_abs() as usize * row;
        if by < size {
            if scroll > 0 {
                s.pixels.copy_within(by.., 0);
            } else {
                s.pixels.copy_within(..size - by, by);
            }
        }
    }
    let mut at = 0;
    for [x, y, w, h] in rects {
        let n = *w as usize * 4;
        for yy in *y..y + h {
            let start = yy as usize * row + *x as usize * 4;
            s.pixels[start..start + n].copy_from_slice(&src[at..at + n]);
            at += n;
        }
    }
    let slot = s.next;
    s.next = (s.next + 1) % SLOTS;
    if flags & (protocol::FLAG_FULL | protocol::FLAG_SCROLL) != 0 {
        s.slots[slot].copy_from_slice(&s.pixels);
    } else {
        for [x, y, w, h] in rects {
            let n = *w as usize * 4;
            for yy in *y..y + h {
                let start = yy as usize * row + *x as usize * 4;
                s.slots[slot][start..start + n].copy_from_slice(&s.pixels[start..start + n]);
            }
        }
    }
}
