//! The GUI protocol's framing, commit encoding and content hash, as
//! docs/gui-protocol.md specifies them.

use anyhow::{Result, bail};

pub const VERSION: u32 = 2;
pub const MIN_VERSION: u32 = 2;
pub const MAX_FRAME_PAYLOAD: usize = 1 << 20;

pub const JSON: u8 = 0x00;
pub const COMMIT: u8 = 0x01;
pub const COMMIT_DATA: u8 = 0x02;
pub const ICON: u8 = 0x03;
pub const PRESENTED: u8 = 0x10;
pub const POINTER_MOTION: u8 = 0x20;
pub const POINTER_BUTTON: u8 = 0x21;
pub const POINTER_AXIS: u8 = 0x22;
pub const POINTER_LEAVE: u8 = 0x23;
pub const KEY: u8 = 0x30;
pub const MODIFIERS: u8 = 0x31;
pub const TOUCH_DOWN: u8 = 0x40;
pub const TOUCH_UP: u8 = 0x41;
pub const TOUCH_MOTION: u8 = 0x42;
pub const TOUCH_CANCEL: u8 = 0x43;
pub const TOUCH_FRAME: u8 = 0x44;

pub const FLAG_BUFFER: u16 = 1 << 0;
pub const FLAG_FULL: u16 = 1 << 1;
pub const FLAG_SCROLL: u16 = 1 << 2;
pub const FLAG_HASH: u16 = 1 << 3;
pub const FLAG_GROUPED: u16 = 1 << 4;
pub const FLAG_HIDDEN: u16 = 1 << 5;

pub const FORMAT_RGBA8888: u8 = 1;
pub const FORMAT_RGBX8888: u8 = 2;

pub const ENCODING_RAW: u8 = 0;
pub const ENCODING_ZSTD: u8 = 1;

pub const COMMIT_HEADER_SIZE: usize = 88;
pub const MAX_RECTS: usize = 4096;
/// The largest buffer the app takes, per side and in bytes at 4 bytes a pixel.
pub const MAX_DIMENSION: u32 = 16384;
pub const MAX_BUFFER_BYTES: u64 = 64 << 20;

/// Negotiates a version with a peer speaking `min..=max`, by the agent's
/// `Negotiate` rule: a `min` of 0 means `max`.
pub fn negotiate(min: u32, max: u32) -> Option<u32> {
    let min = if min == 0 { max } else { min };
    let v = VERSION.min(max);
    (v >= min && v >= MIN_VERSION).then_some(v)
}

pub fn frame(ty: u8, payload: &[u8]) -> Vec<u8> {
    debug_assert!(payload.len() <= MAX_FRAME_PAYLOAD);
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(ty);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

pub fn json_frame(value: &serde_json::Value) -> Vec<u8> {
    frame(JSON, &serde_json::to_vec(value).expect("a JSON value always serializes"))
}

/// Splits a byte stream into frames.
#[derive(Default)]
pub struct FrameReader {
    buf: Vec<u8>,
    pos: usize,
}

impl FrameReader {
    pub fn push(&mut self, data: &[u8]) {
        if self.pos > 0 && self.pos == self.buf.len() {
            self.buf.clear();
            self.pos = 0;
        }
        self.buf.extend_from_slice(data);
    }

    /// The next whole frame, or None until more bytes arrive.
    pub fn next_frame(&mut self) -> Result<Option<(u8, Vec<u8>)>> {
        let rest = &self.buf[self.pos..];
        if rest.len() < 5 {
            self.compact();
            return Ok(None);
        }
        let len = u32::from_be_bytes(rest[1..5].try_into().unwrap()) as usize;
        if len > MAX_FRAME_PAYLOAD {
            bail!("frame of {len} bytes exceeds {MAX_FRAME_PAYLOAD}");
        }
        if rest.len() < 5 + len {
            self.compact();
            return Ok(None);
        }
        let ty = rest[0];
        let payload = rest[5..5 + len].to_vec();
        self.pos += 5 + len;
        Ok(Some((ty, payload)))
    }

    fn compact(&mut self) {
        if self.pos > 0 {
            self.buf.drain(..self.pos);
            self.pos = 0;
        }
    }
}

/// Reads big-endian fields from a binary payload; trailing bytes are ignored.
pub struct Fields<'a> {
    data: &'a [u8],
}

impl<'a> Fields<'a> {
    pub fn new(data: &'a [u8]) -> Self {
        Self { data }
    }

    fn take<const N: usize>(&mut self) -> Result<[u8; N]> {
        if self.data.len() < N {
            bail!("binary frame too short");
        }
        let (head, rest) = self.data.split_at(N);
        self.data = rest;
        Ok(head.try_into().unwrap())
    }

    pub fn u8(&mut self) -> Result<u8> {
        Ok(self.take::<1>()?[0])
    }

    pub fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_be_bytes(self.take()?))
    }

    pub fn i32(&mut self) -> Result<i32> {
        Ok(i32::from_be_bytes(self.take()?))
    }

    pub fn f32(&mut self) -> Result<f32> {
        Ok(f32::from_be_bytes(self.take()?))
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Rect {
    pub x: u32,
    pub y: u32,
    pub w: u32,
    pub h: u32,
}

impl Rect {
    pub fn area(&self) -> u64 {
        self.w as u64 * self.h as u64
    }
}

/// A COMMIT's fixed fields; see "COMMIT (0x01)".
#[derive(Clone, Debug, Default)]
pub struct Commit {
    pub flags: u16,
    pub surface: u32,
    pub window: u32,
    pub seq: u32,
    pub z: u16,
    pub format: u8,
    pub encoding: u8,
    pub transform: u8,
    pub x: i32,
    pub y: i32,
    pub dst_width: u32,
    pub dst_height: u32,
    pub width: u32,
    pub height: u32,
    pub src_x: u32,
    pub src_y: u32,
    pub src_width: u32,
    pub src_height: u32,
    pub scroll: i32,
    pub rects: Vec<Rect>,
    pub hash: u64,
}

impl Commit {
    /// The COMMIT frame and the COMMIT_DATA frames that carry `data` (the
    /// already encoded pixel data), each within MAX_FRAME_PAYLOAD.
    pub fn encode(&self, data: &[u8]) -> Vec<Vec<u8>> {
        let mut head = Vec::with_capacity(COMMIT_HEADER_SIZE + 16 * self.rects.len());
        head.extend_from_slice(&(COMMIT_HEADER_SIZE as u16).to_be_bytes());
        head.extend_from_slice(&self.flags.to_be_bytes());
        head.extend_from_slice(&self.surface.to_be_bytes());
        head.extend_from_slice(&self.window.to_be_bytes());
        head.extend_from_slice(&self.seq.to_be_bytes());
        head.extend_from_slice(&self.z.to_be_bytes());
        head.push(self.format);
        head.push(self.encoding);
        head.push(self.transform);
        head.push(0);
        head.extend_from_slice(&0u16.to_be_bytes());
        head.extend_from_slice(&self.x.to_be_bytes());
        head.extend_from_slice(&self.y.to_be_bytes());
        for v in [
            self.dst_width,
            self.dst_height,
            self.width,
            self.height,
            self.src_x,
            self.src_y,
            self.src_width,
            self.src_height,
        ] {
            head.extend_from_slice(&v.to_be_bytes());
        }
        head.extend_from_slice(&self.scroll.to_be_bytes());
        head.extend_from_slice(&(self.rects.len() as u32).to_be_bytes());
        head.extend_from_slice(&(data.len() as u32).to_be_bytes());
        head.extend_from_slice(&0u32.to_be_bytes());
        head.extend_from_slice(&self.hash.to_be_bytes());
        debug_assert_eq!(head.len(), COMMIT_HEADER_SIZE);
        for r in &self.rects {
            for v in [r.x, r.y, r.w, r.h] {
                head.extend_from_slice(&v.to_be_bytes());
            }
        }

        let first = data.len().min(MAX_FRAME_PAYLOAD - head.len());
        head.extend_from_slice(&data[..first]);
        let mut frames = vec![frame(COMMIT, &head)];
        for chunk in data[first..].chunks(MAX_FRAME_PAYLOAD - 4) {
            let mut payload = Vec::with_capacity(4 + chunk.len());
            payload.extend_from_slice(&self.seq.to_be_bytes());
            payload.extend_from_slice(chunk);
            frames.push(frame(COMMIT_DATA, &payload));
        }
        frames
    }
}

/// The spec's Content hash, fed in any slicing of the same byte stream.
pub struct ContentHash {
    h: u64,
    tail: [u8; 8],
    n: usize,
}

const FNV_PRIME: u64 = 0x0100_0000_01b3;

impl Default for ContentHash {
    fn default() -> Self {
        Self {
            h: 0xcbf2_9ce4_8422_2325,
            tail: [0; 8],
            n: 0,
        }
    }
}

impl ContentHash {
    pub fn update(&mut self, mut bytes: &[u8]) {
        if self.n > 0 {
            let k = (8 - self.n).min(bytes.len());
            self.tail[self.n..self.n + k].copy_from_slice(&bytes[..k]);
            self.n += k;
            bytes = &bytes[k..];
            if self.n < 8 {
                return;
            }
            self.mix(u64::from_le_bytes(self.tail));
            self.n = 0;
        }
        let (chunks, rest) = bytes.as_chunks::<8>();
        for c in chunks {
            self.mix(u64::from_le_bytes(*c));
        }
        self.tail[..rest.len()].copy_from_slice(rest);
        self.n = rest.len();
    }

    fn mix(&mut self, c: u64) {
        self.h = (self.h ^ c).wrapping_mul(FNV_PRIME).rotate_left(29);
    }

    pub fn finish(mut self) -> u64 {
        for &b in &self.tail[..self.n] {
            self.h = (self.h ^ b as u64).wrapping_mul(FNV_PRIME);
        }
        self.h
    }
}

/// Data larger than this is sampled before it's compressed: SAMPLES spots of
/// SAMPLE bytes each, spread across it.
const SAMPLE_ABOVE: usize = 256 << 10;
const SAMPLES: usize = 4;
const SAMPLE: usize = 16 << 10;

/// Samples that compress to more than this fraction of their size send the
/// data raw: compressing it would cost both ends more time than the few
/// bytes it saves on the wire.
const INCOMPRESSIBLE: f64 = 0.9;

/// zstd-compresses `data` as one or more concatenated frames, in parallel
/// when it is large; None when compressing it isn't worth it.
pub fn compress(data: &[u8]) -> Option<Vec<u8>> {
    if data.len() > SAMPLE_ABOVE {
        let step = data.len() / SAMPLES;
        let packed: usize = (0..SAMPLES)
            .map(|i| zstd::bulk::compress(&data[i * step..i * step + SAMPLE], 1).expect("zstd").len())
            .sum();
        if packed as f64 > (SAMPLES * SAMPLE) as f64 * INCOMPRESSIBLE {
            return None;
        }
    }
    let packed = shards(data);
    (packed.len() < data.len()).then_some(packed)
}

fn shards(data: &[u8]) -> Vec<u8> {
    const SHARD: usize = 1 << 20;
    if data.len() <= 2 * SHARD {
        return zstd::bulk::compress(data, 1).expect("zstd compression of a slice can't fail");
    }
    let threads = std::thread::available_parallelism().map_or(1, |n| n.get()).min(4);
    let shard = data.len().div_ceil(threads).max(SHARD);
    std::thread::scope(|scope| {
        let handles: Vec<_> = data
            .chunks(shard)
            .map(|chunk| scope.spawn(move || zstd::bulk::compress(chunk, 1).expect("zstd")))
            .collect();
        handles.into_iter().flat_map(|h| h.join().expect("zstd thread")).collect()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn reference_hash(bytes: &[u8]) -> u64 {
        let mut h: u64 = 0xcbf2_9ce4_8422_2325;
        let (chunks, rest) = bytes.as_chunks::<8>();
        for c in chunks {
            h = (h ^ u64::from_le_bytes(*c)).wrapping_mul(FNV_PRIME).rotate_left(29);
        }
        for b in rest {
            h = (h ^ *b as u64).wrapping_mul(FNV_PRIME);
        }
        h
    }

    #[test]
    fn hash_is_independent_of_slicing() {
        let data: Vec<u8> = (0..1001u32).map(|i| (i * 7 + 3) as u8).collect();
        for step in [1, 3, 4, 8, 12, 1001] {
            let mut h = ContentHash::default();
            for chunk in data.chunks(step) {
                h.update(chunk);
            }
            assert_eq!(h.finish(), reference_hash(&data), "step {step}");
        }
    }

    #[test]
    fn large_commits_split_under_the_frame_limit() {
        let data = vec![7u8; 3 * MAX_FRAME_PAYLOAD];
        let commit = Commit {
            seq: 9,
            rects: vec![Rect { x: 0, y: 0, w: 1, h: 1 }],
            ..Default::default()
        };
        let frames = commit.encode(&data);
        let mut carried = 0;
        for (i, f) in frames.iter().enumerate() {
            let len = u32::from_be_bytes(f[1..5].try_into().unwrap()) as usize;
            assert!(len <= MAX_FRAME_PAYLOAD);
            assert_eq!(f.len(), 5 + len);
            if i == 0 {
                assert_eq!(f[0], COMMIT);
                carried += len - COMMIT_HEADER_SIZE - 16;
            } else {
                assert_eq!(f[0], COMMIT_DATA);
                assert_eq!(u32::from_be_bytes(f[5..9].try_into().unwrap()), 9);
                carried += len - 4;
            }
        }
        assert_eq!(carried, data.len());
    }

    #[test]
    fn sharded_compression_decodes_to_the_input() {
        let data: Vec<u8> = (0..5 * 1024 * 1024u32).map(|i| (i % 251) as u8).collect();
        let packed = compress(&data).unwrap();
        let mut out = Vec::new();
        zstd::stream::copy_decode(&packed[..], &mut out).unwrap();
        assert_eq!(out, data);
    }

    #[test]
    fn noise_is_not_compressed() {
        let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
        let noise: Vec<u8> = (0..1024 * 1024)
            .map(|_| {
                x ^= x << 13;
                x ^= x >> 7;
                x ^= x << 17;
                x as u8
            })
            .collect();
        assert_eq!(compress(&noise), None);
        assert_eq!(compress(&noise[..4096]), None);
    }

    #[test]
    fn negotiates_like_the_agent() {
        assert_eq!(negotiate(1, 1), None);
        assert_eq!(negotiate(2, 2), Some(2));
        assert_eq!(negotiate(0, 3), None);
        assert_eq!(negotiate(1, 3), Some(2));
        assert_eq!(negotiate(3, 3), None);
    }
}
