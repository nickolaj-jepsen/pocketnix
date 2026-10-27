// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// The damage-only commits and scroll detection come from the wprs fork's
// damage_patch.rs; the mirror now holds wire-ready rows and the packing
// produces the GUI protocol's commits.

//! Per-surface state: the mirror of the surface's content (the reference
//! copy the app holds once it has applied every commit) and the damage the
//! app hasn't been sent yet.

use std::collections::HashMap;
use std::sync::Mutex;

use smithay::reexports::wayland_server::protocol::wl_callback::WlCallback;
use smithay::reexports::wayland_server::protocol::wl_shm;
use smithay::utils::Logical;
use smithay::utils::Size;
use smithay::utils::Transform;
use smithay::wayland::compositor::SurfaceData;

use crate::damage::Viewport;
use crate::protocol::{self, ContentHash, Rect};

/// Above this fraction of the buffer's area, damage is checked for a scroll
/// and otherwise sent whole.
const MAX_PATCH_FRACTION: f64 = 0.5;

pub struct Mirror {
    pub width: u32,
    pub height: u32,
    pub format: u8,
    /// Tightly packed rows, `width * 4` bytes each, in the client's wl_shm
    /// byte order; packing converts them to the wire's.
    pub pixels: Vec<u8>,
    /// Row hashes of `pixels`, while known (scroll detection's last result).
    row_hashes: Option<Vec<u64>>,
}

/// What the app hasn't been sent yet, relative to the content it holds.
#[derive(Default)]
pub struct Pending {
    pub full: bool,
    pub scroll: Option<i32>,
    pub rects: Vec<Rect>,
}

/// What the active connection's app was last sent for a surface.
#[derive(Clone, PartialEq, Debug)]
pub struct Sent {
    /// Buffer size and format, None when hidden.
    pub content: Option<(u32, u32, u8)>,
    pub placement: Placement,
}

/// A COMMIT's placement fields: everything but content.
#[derive(Clone, Copy, PartialEq, Debug, Default)]
pub struct Placement {
    pub z: u16,
    pub x: i32,
    pub y: i32,
    pub dst_width: u32,
    pub dst_height: u32,
    pub src: (u32, u32, u32, u32),
    pub transform: u8,
}

pub struct Surf {
    pub id: u32,
    pub mirror: Option<Mirror>,
    pub pending: Pending,
    pub callbacks: Vec<WlCallback>,
    pub buffer_scale: i32,
    pub transform: Transform,
    pub viewport: Viewport,
    /// Valid only while `sent_gen` is the active connection's generation.
    pub sent: Option<Sent>,
    pub sent_gen: u64,
    pub entered_output: bool,
    /// The scale this surface was last told to prefer.
    pub applied_scale: Option<f64>,
}

pub struct SurfCell(pub Mutex<Surf>);

pub fn with_surf<T>(data: &SurfaceData, f: impl FnOnce(&mut Surf) -> T) -> Option<T> {
    data.data_map.get::<SurfCell>().map(|cell| f(&mut cell.0.lock().unwrap()))
}

impl Surf {
    pub fn new(id: u32) -> Self {
        Self {
            id,
            mirror: None,
            pending: Pending::default(),
            callbacks: Vec::new(),
            buffer_scale: 1,
            transform: Transform::Normal,
            viewport: Viewport::default(),
            sent: None,
            sent_gen: 0,
            entered_output: false,
            applied_scale: None,
        }
    }

    /// The surface's size in its client's logical coordinates.
    pub fn logical_size(&self) -> Size<i32, Logical> {
        if let Some(dst) = self.viewport.dst {
            return dst;
        }
        if let Some(src) = self.viewport.src {
            return Size::from((src.size.w.round() as i32, src.size.h.round() as i32));
        }
        match &self.mirror {
            Some(m) => Size::<i32, smithay::utils::Buffer>::from((m.width as i32, m.height as i32))
                .to_logical(self.buffer_scale.max(1), self.transform),
            None => Size::from((0, 0)),
        }
    }

    /// The shown part of the transformed buffer, in buffer pixels.
    pub fn src_rect(&self) -> (u32, u32, u32, u32) {
        let Some(m) = &self.mirror else { return (0, 0, 0, 0) };
        let rotated = matches!(
            self.transform,
            Transform::_90 | Transform::_270 | Transform::Flipped90 | Transform::Flipped270
        );
        let (w, h) = if rotated {
            (m.height, m.width)
        } else {
            (m.width, m.height)
        };
        match self.viewport.src {
            Some(src) => {
                let s = self.buffer_scale.max(1) as f64;
                let x = (src.loc.x * s).round().clamp(0.0, w as f64) as u32;
                let y = (src.loc.y * s).round().clamp(0.0, h as f64) as u32;
                let sw = (src.size.w * s).round().max(0.0) as u32;
                let sh = (src.size.h * s).round().max(0.0) as u32;
                (x, y, sw.min(w - x), sh.min(h - y))
            }
            None => (0, 0, w, h),
        }
    }

    /// Whether the active connection's app holds this surface's current
    /// content size and format.
    pub fn app_has_content(&self, generation: u64) -> bool {
        let Some(m) = &self.mirror else { return false };
        generation != 0
            && self.sent_gen == generation
            && self
                .sent
                .as_ref()
                .is_some_and(|s| s.content == Some((m.width, m.height, m.format)))
    }

    /// Takes a new committed buffer into the mirror and records what the app
    /// will need: `damage` in buffer pixels, `src` the client's buffer.
    pub fn apply_buffer(
        &mut self,
        src: &[u8],
        width: u32,
        height: u32,
        stride: usize,
        format: u8,
        damage: Vec<Rect>,
        app_has: bool,
    ) {
        let same = self
            .mirror
            .as_ref()
            .is_some_and(|m| m.width == width && m.height == height && m.format == format);
        if !same || !app_has {
            self.mirror = Some(Mirror {
                width,
                height,
                format,
                pixels: copy_whole(src, width, height, stride),
                row_hashes: None,
            });
            self.pending = Pending { full: true, ..Default::default() };
            return;
        }
        let m = self.mirror.as_mut().unwrap();
        let rects: Vec<Rect> = damage.into_iter().filter_map(|r| clip(r, width, height)).collect();
        if rects.is_empty() {
            return;
        }
        if self.pending.full {
            m.copy_rects(src, stride, &rects);
            return;
        }
        let area: u64 = rects.iter().map(Rect::area).sum();
        let whole = width as u64 * height as u64;
        if area as f64 >= whole as f64 * MAX_PATCH_FRACTION {
            if self.pending.scroll.is_none() && self.pending.rects.is_empty() {
                let new_hashes = row_hashes(src, stride, width as usize * 4, height as usize);
                let old_hashes = m
                    .row_hashes
                    .take()
                    .unwrap_or_else(|| row_hashes(&m.pixels, width as usize * 4, width as usize * 4, height as usize));
                let (dy, matched) = detect_scroll(&old_hashes, &new_hashes, &m.pixels, src, width as usize * 4, stride);
                let n = matched.iter().filter(|m| **m).count();
                m.pixels = copy_whole(src, width, height, stride);
                m.row_hashes = Some(new_hashes);
                if dy != 0 && n * 2 > height as usize {
                    self.pending.scroll = Some(dy as i32);
                    self.pending.rects = unmatched_rows(&matched, width);
                    return;
                }
                self.pending.full = true;
                return;
            }
            m.pixels = copy_whole(src, width, height, stride);
            m.row_hashes = None;
            self.pending.full = true;
            return;
        }
        m.copy_rects(src, stride, &rects);
        self.pending.rects.extend(rects);
        if self.pending.rects.len() > protocol::MAX_RECTS {
            self.pending.rects = vec![bounding(&self.pending.rects)];
        }
        // Rects pile up over commits until a flush; past the buffer's area the
        // whole buffer is cheaper, and a commit never carries more than it.
        if self.pending.rects.iter().map(Rect::area).sum::<u64>() > whole {
            self.pending = Pending { full: true, ..Default::default() };
        }
    }

    pub fn remove_buffer(&mut self) {
        self.mirror = None;
        self.pending = Pending { full: true, ..Default::default() };
    }
}

impl Mirror {
    fn copy_rects(&mut self, src: &[u8], stride: usize, rects: &[Rect]) {
        let row = self.width as usize * 4;
        for r in rects {
            let (x, w) = (r.x as usize * 4, r.w as usize * 4);
            for y in r.y as usize..(r.y + r.h) as usize {
                self.pixels[y * row + x..y * row + x + w].copy_from_slice(&src[y * stride + x..y * stride + x + w]);
            }
        }
        self.row_hashes = None;
    }

    /// The wire bytes of `rects`, rows in rect order.
    pub fn pack(&self, rects: &[Rect]) -> Vec<u8> {
        let row = self.width as usize * 4;
        let len: u64 = rects.iter().map(|r| r.area() * 4).sum();
        let mut out = Vec::with_capacity(len as usize);
        for r in rects {
            let (x, w) = (r.x as usize * 4, r.w as usize * 4);
            for y in r.y as usize..(r.y + r.h) as usize {
                to_wire(&self.pixels[y * row + x..y * row + x + w], &mut out);
            }
        }
        out
    }

    pub fn hash(&self) -> u64 {
        let row = self.width as usize * 4;
        let mut h = ContentHash::default();
        let mut buf = Vec::with_capacity(row);
        for line in self.pixels.chunks(row.max(1)) {
            buf.clear();
            to_wire(line, &mut buf);
            h.update(&buf);
        }
        h.finish()
    }
}

/// The wire format for a wl_shm format, if it's one we advertise.
pub fn wire_format(format: wl_shm::Format) -> Option<u8> {
    match format {
        wl_shm::Format::Argb8888 => Some(protocol::FORMAT_RGBA8888),
        wl_shm::Format::Xrgb8888 => Some(protocol::FORMAT_RGBX8888),
        _ => None,
    }
}

/// wl_shm's ARGB8888 and XRGB8888 are little-endian words, bytes B, G, R, A
/// in memory; the wire wants R, G, B, A.
fn to_wire(src: &[u8], out: &mut Vec<u8>) {
    let (px, _) = src.as_chunks::<4>();
    out.reserve(src.len());
    for p in px {
        out.extend_from_slice(&[p[2], p[1], p[0], p[3]]);
    }
}

fn copy_whole(src: &[u8], width: u32, height: u32, stride: usize) -> Vec<u8> {
    let row = width as usize * 4;
    if stride == row {
        return src[..row * height as usize].to_vec();
    }
    let mut out = Vec::with_capacity(row * height as usize);
    for y in 0..height as usize {
        out.extend_from_slice(&src[y * stride..y * stride + row]);
    }
    out
}

pub fn clip(r: Rect, width: u32, height: u32) -> Option<Rect> {
    let x1 = (r.x.saturating_add(r.w)).min(width);
    let y1 = (r.y.saturating_add(r.h)).min(height);
    (x1 > r.x && y1 > r.y).then(|| Rect { x: r.x, y: r.y, w: x1 - r.x, h: y1 - r.y })
}

fn bounding(rects: &[Rect]) -> Rect {
    let x0 = rects.iter().map(|r| r.x).min().unwrap_or(0);
    let y0 = rects.iter().map(|r| r.y).min().unwrap_or(0);
    let x1 = rects.iter().map(|r| r.x + r.w).max().unwrap_or(0);
    let y1 = rects.iter().map(|r| r.y + r.h).max().unwrap_or(0);
    Rect { x: x0, y: y0, w: x1 - x0, h: y1 - y0 }
}

fn row_hash(row: &[u8]) -> u64 {
    let mut h = ContentHash::default();
    h.update(row);
    h.finish()
}

fn row_hashes(pixels: &[u8], stride: usize, row: usize, height: usize) -> Vec<u64> {
    (0..height).map(|y| row_hash(&pixels[y * stride..y * stride + row])).collect()
}

/// The vertical shift `dy` (new row y == old row y + dy) that matches the
/// most rows, and which rows it matches.
fn detect_scroll(
    old_h: &[u64],
    new_h: &[u64],
    old: &[u8],
    new: &[u8],
    old_stride: usize,
    new_stride: usize,
) -> (i64, Vec<bool>) {
    let height = old_h.len();
    let mut by_hash: HashMap<u64, Vec<usize>> = HashMap::new();
    for (y, h) in old_h.iter().enumerate() {
        by_hash.entry(*h).or_default().push(y);
    }
    let mut votes: HashMap<i64, usize> = HashMap::new();
    for (y, h) in new_h.iter().enumerate() {
        if let Some(ys) = by_hash.get(h) {
            // rows repeated many times (blank lines) say nothing about the shift
            if ys.len() <= 4 {
                for oy in ys {
                    let dy = *oy as i64 - y as i64;
                    if dy != 0 {
                        *votes.entry(dy).or_default() += 1;
                    }
                }
            }
        }
    }
    let dy = votes.into_iter().max_by_key(|(d, n)| (*n, -d.abs())).map_or(0, |(d, _)| d);
    let row = old_stride;
    let matched = (0..height)
        .map(|y| {
            let oy = y as i64 + dy;
            dy != 0
                && (0..height as i64).contains(&oy)
                && new_h[y] == old_h[oy as usize]
                && new[y * new_stride..y * new_stride + row] == old[oy as usize * old_stride..(oy as usize + 1) * old_stride]
        })
        .collect();
    (dy, matched)
}

fn unmatched_rows(matched: &[bool], width: u32) -> Vec<Rect> {
    let mut rects = Vec::new();
    let mut y = 0;
    while y < matched.len() {
        if matched[y] {
            y += 1;
            continue;
        }
        let y0 = y;
        while y < matched.len() && !matched[y] {
            y += 1;
        }
        rects.push(Rect { x: 0, y: y0 as u32, w: width, h: (y - y0) as u32 });
    }
    rects
}

#[cfg(test)]
mod tests {
    use super::*;

    fn image(width: u32, height: u32, seed: u32) -> Vec<u8> {
        (0..width * height * 4).map(|i| ((i / 4 / width + seed) * 31 + i % (width * 4)) as u8).collect()
    }

    /// Applies what the app would be sent to its copy, the way the spec's
    /// "Applying a commit" does, and returns the result.
    fn apply_as_app(app: &[u8], s: &Surf) -> Vec<u8> {
        let m = s.mirror.as_ref().unwrap();
        let row = m.width as usize * 4;
        let mut out = app.to_vec();
        if s.pending.full {
            return m.pixels.clone();
        }
        if let Some(dy) = s.pending.scroll {
            let prev = out.clone();
            for y in 0..m.height as i64 {
                let sy = y + dy as i64;
                if (0..m.height as i64).contains(&sy) {
                    out[y as usize * row..(y as usize + 1) * row]
                        .copy_from_slice(&prev[sy as usize * row..(sy as usize + 1) * row]);
                }
            }
        }
        for r in &s.pending.rects {
            for y in r.y..r.y + r.h {
                let a = y as usize * row + r.x as usize * 4;
                let b = a + r.w as usize * 4;
                out[a..b].copy_from_slice(&m.pixels[a..b]);
            }
        }
        out
    }

    #[test]
    fn small_damage_sends_only_its_rects() {
        let (w, h) = (64, 48);
        let mut s = Surf::new(1);
        let first = image(w, h, 0);
        s.apply_buffer(&first, w, h, w as usize * 4, 1, vec![], false);
        assert!(s.pending.full);
        let app = s.mirror.as_ref().unwrap().pixels.clone();
        s.pending = Pending::default();

        let mut next = first.clone();
        for b in &mut next[(10 * w as usize + 5) * 4..(10 * w as usize + 9) * 4] {
            *b ^= 0xff;
        }
        s.apply_buffer(&next, w, h, w as usize * 4, 1, vec![Rect { x: 5, y: 10, w: 4, h: 1 }], true);
        assert!(!s.pending.full);
        assert_eq!(s.pending.rects, vec![Rect { x: 5, y: 10, w: 4, h: 1 }]);
        assert_eq!(apply_as_app(&app, &s), next);
    }

    #[test]
    fn a_scroll_sends_the_new_rows_and_an_offset() {
        let (w, h) = (32, 100);
        let stride = w as usize * 4 + 16;
        let base = image(w, h + 7, 3);
        let padded = |rows: &[u8]| {
            let mut out = vec![0u8; stride * h as usize];
            for y in 0..h as usize {
                out[y * stride..y * stride + w as usize * 4].copy_from_slice(&rows[y * w as usize * 4..(y + 1) * w as usize * 4]);
            }
            out
        };
        let mut s = Surf::new(1);
        s.apply_buffer(&padded(&base[..(w * h * 4) as usize]), w, h, stride, 1, vec![], false);
        let app = s.mirror.as_ref().unwrap().pixels.clone();
        s.pending = Pending::default();

        let scrolled = &base[(7 * w * 4) as usize..((h + 7) * w * 4) as usize];
        s.apply_buffer(&padded(scrolled), w, h, stride, 1, vec![Rect { x: 0, y: 0, w, h }], true);
        assert_eq!(s.pending.scroll, Some(7));
        assert_eq!(s.pending.rects, vec![Rect { x: 0, y: h - 7, w, h: 7 }]);
        assert_eq!(apply_as_app(&app, &s), scrolled);
    }

    #[test]
    fn damage_piling_up_past_the_buffer_is_full() {
        let (w, h) = (16, 16);
        let mut s = Surf::new(1);
        let first = image(w, h, 0);
        s.apply_buffer(&first, w, h, w as usize * 4, 1, vec![], false);
        let app = s.mirror.as_ref().unwrap().pixels.clone();
        s.pending = Pending::default();

        // A third of the buffer each time: none alone is big enough to send whole.
        let mut last = first;
        for seed in 1..=4 {
            last = image(w, h, seed);
            s.apply_buffer(&last, w, h, w as usize * 4, 1, vec![Rect { x: 0, y: 0, w, h: h / 3 }], true);
        }
        let area: u64 = s.pending.rects.iter().map(Rect::area).sum();
        assert!(s.pending.full || area <= (w * h) as u64, "pending rects cover {area} pixels");
        let mut want = app.clone();
        let row = w as usize * 4;
        want[..row * (h / 3) as usize].copy_from_slice(&last[..row * (h / 3) as usize]);
        assert_eq!(apply_as_app(&app, &s), want);
    }

    #[test]
    fn a_size_change_is_full() {
        let mut s = Surf::new(1);
        s.apply_buffer(&image(8, 8, 0), 8, 8, 32, 1, vec![], false);
        s.pending = Pending::default();
        s.apply_buffer(&image(9, 8, 0), 9, 8, 36, 1, vec![Rect { x: 0, y: 0, w: 1, h: 1 }], true);
        assert!(s.pending.full);
    }

    #[test]
    fn packing_converts_to_rgba() {
        let s = {
            let mut s = Surf::new(1);
            s.apply_buffer(&[1, 2, 3, 4, 5, 6, 7, 8], 2, 1, 8, 1, vec![], false);
            s
        };
        let m = s.mirror.as_ref().unwrap();
        assert_eq!(m.pack(&[Rect { x: 1, y: 0, w: 1, h: 1 }]), vec![7, 6, 5, 8]);
    }
}
