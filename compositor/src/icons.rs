//! Icon lookup and rendering for the app list (docs/gui-protocol.md, Icons):
//! which files an `Icon=` value can render from, the key derived from their
//! content, and a PNG of one at a given size.

use std::collections::HashMap;
use std::os::unix::fs::MetadataExt;
use std::path::Path;
use std::path::PathBuf;
use std::sync::Arc;

use resvg::tiny_skia;
use resvg::usvg;

use crate::protocol::ContentHash;

/// Bumped whenever rendering changes, so the app drops icons it cached.
const RENDER_VERSION: u64 = 1;
const THEMES: [&str; 2] = ["hicolor", "Adwaita"];

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Kind {
    Png,
    Svg,
}

/// One file an icon can render from.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct IconFile {
    pub kind: Kind,
    /// Nominal size times scale, in pixels; 0 when unknown (scalable, a
    /// pixmap or an absolute path).
    pub pixels: u32,
    pub path: PathBuf,
}

pub type IconSet = Arc<Vec<IconFile>>;

/// The icon directories of one scan: every theme file and pixmap by icon
/// name, and the content hashes of files seen before.
#[derive(Default)]
pub struct IconIndex {
    themes: Vec<HashMap<String, Vec<IconFile>>>,
    pixmaps: HashMap<String, Vec<IconFile>>,
}

/// Content hashes by file identity, kept across scans so an unchanged file
/// is read once.
#[derive(Default)]
pub struct HashCache {
    known: HashMap<(u64, u64, u64, i64, i64), u64>,
}

impl HashCache {
    fn hash(&mut self, path: &Path) -> Option<u64> {
        let meta = std::fs::metadata(path).ok()?;
        let id = (meta.dev(), meta.ino(), meta.len(), meta.mtime(), meta.mtime_nsec());
        if let Some(h) = self.known.get(&id) {
            return Some(*h);
        }
        let mut h = ContentHash::default();
        h.update(&std::fs::read(path).ok()?);
        let h = h.finish();
        self.known.insert(id, h);
        Some(h)
    }
}

fn kind_of(path: &Path) -> Option<Kind> {
    match path.extension()?.to_str()? {
        "png" => Some(Kind::Png),
        "svg" => Some(Kind::Svg),
        _ => None,
    }
}

/// A theme directory's nominal pixel size from its index.theme entry or its
/// name (`48x48`, `48x48@2`, `scalable`); None for directories that hold
/// no app icons we render (`symbolic`).
fn dir_pixels(name: &str, index: Option<&HashMap<String, (u32, u32, bool)>>) -> Option<u32> {
    if let Some(index) = index {
        let (size, scale, scalable) = index.get(name)?;
        return Some(if *scalable { 0 } else { size * scale.max(&1) });
    }
    let first = name.split('/').next()?;
    if first == "scalable" {
        return Some(0);
    }
    let (dims, scale) = match first.split_once('@') {
        Some((d, s)) => (d, s.parse::<u32>().ok()?),
        None => (first, 1),
    };
    let (w, _) = dims.split_once('x')?;
    Some(w.parse::<u32>().ok()? * scale.max(1))
}

/// index.theme's directories: name → (Size, Scale, scalable).
fn read_index(path: &Path) -> Option<HashMap<String, (u32, u32, bool)>> {
    let text = std::fs::read_to_string(path).ok()?;
    let mut out = HashMap::new();
    let mut group = String::new();
    let mut cur: (u32, u32, bool) = (0, 1, false);
    let flush = |group: &str, cur: (u32, u32, bool), out: &mut HashMap<String, (u32, u32, bool)>| {
        if !group.is_empty() && group != "Icon Theme" && cur.0 > 0 {
            out.insert(group.to_owned(), cur);
        }
    };
    for line in text.lines() {
        let line = line.trim();
        if let Some(g) = line.strip_prefix('[').and_then(|l| l.strip_suffix(']')) {
            flush(&group, cur, &mut out);
            group = g.to_owned();
            cur = (0, 1, false);
        } else if let Some((k, v)) = line.split_once('=') {
            let v = v.trim();
            match k.trim() {
                "Size" => cur.0 = v.parse().unwrap_or(0),
                "Scale" => cur.1 = v.parse().unwrap_or(1),
                "Type" => cur.2 = v == "Scalable",
                _ => {}
            }
        }
    }
    flush(&group, cur, &mut out);
    Some(out)
}

/// Files directly in `dir` by icon name (extension dropped).
fn add_files(dir: &Path, pixels: u32, out: &mut HashMap<String, Vec<IconFile>>) {
    let Ok(rd) = std::fs::read_dir(dir) else { return };
    for e in rd.flatten() {
        let path = e.path();
        let Some(kind) = kind_of(&path) else { continue };
        let Some(stem) = path.file_stem().and_then(|s| s.to_str()) else { continue };
        out.entry(stem.to_owned()).or_default().push(IconFile { kind, pixels, path });
    }
}

fn subdirs(dir: &Path) -> Vec<String> {
    let Ok(rd) = std::fs::read_dir(dir) else { return Vec::new() };
    let mut out: Vec<String> = rd
        .flatten()
        .filter(|e| e.path().is_dir())
        .filter_map(|e| e.file_name().into_string().ok())
        .collect();
    out.sort();
    out
}

impl IconIndex {
    /// Indexes `hicolor` and `Adwaita` under each of `icon_bases`, and the
    /// `pixmaps` directories.
    pub fn build(icon_bases: &[PathBuf], pixmap_dirs: &[PathBuf]) -> Self {
        let mut themes = Vec::new();
        for theme in THEMES {
            let mut files: HashMap<String, Vec<IconFile>> = HashMap::new();
            for base in icon_bases {
                let root = base.join(theme);
                if !root.is_dir() {
                    continue;
                }
                let index = read_index(&root.join("index.theme")).filter(|i| !i.is_empty());
                match &index {
                    Some(index) => {
                        let mut dirs: Vec<&String> = index.keys().collect();
                        dirs.sort();
                        for d in dirs {
                            if let Some(px) = dir_pixels(d, Some(index)) {
                                add_files(&root.join(d), px, &mut files);
                            }
                        }
                    }
                    None => {
                        for size in subdirs(&root) {
                            for context in subdirs(&root.join(&size)) {
                                let name = format!("{size}/{context}");
                                if let Some(px) = dir_pixels(&name, None) {
                                    add_files(&root.join(&name), px, &mut files);
                                }
                            }
                        }
                    }
                }
            }
            themes.push(files);
        }
        let mut pixmaps = HashMap::new();
        for dir in pixmap_dirs {
            add_files(dir, 0, &mut pixmaps);
        }
        Self { themes, pixmaps }
    }

    /// The files an `Icon=` value can render from: an absolute path as is,
    /// else the first theme that has the name, else the pixmaps.
    pub fn lookup(&self, icon: &str) -> Vec<IconFile> {
        if icon.is_empty() {
            return Vec::new();
        }
        if icon.starts_with('/') {
            let path = PathBuf::from(icon);
            return match kind_of(&path) {
                Some(kind) if path.is_file() => vec![IconFile { kind, pixels: 0, path }],
                _ => Vec::new(),
            };
        }
        let name = icon
            .strip_suffix(".png")
            .or_else(|| icon.strip_suffix(".svg"))
            .unwrap_or(icon);
        for theme in &self.themes {
            if let Some(files) = theme.get(name).filter(|f| !f.is_empty()) {
                return files.clone();
            }
        }
        self.pixmaps.get(name).cloned().unwrap_or_default()
    }
}

/// The key for a set of files: their content and the rendering version.
/// Empty when nothing in the set can be read.
pub fn key(files: &[IconFile], hashes: &mut HashCache) -> String {
    let mut parts: Vec<(Kind, u32, u64)> = files
        .iter()
        .filter_map(|f| Some((f.kind, f.pixels, hashes.hash(&f.path)?)))
        .collect();
    if parts.is_empty() {
        return String::new();
    }
    parts.sort();
    parts.dedup();
    let mut h = ContentHash::default();
    h.update(&RENDER_VERSION.to_le_bytes());
    for (kind, pixels, content) in parts {
        h.update(&[kind as u8]);
        h.update(&pixels.to_le_bytes());
        h.update(&content.to_le_bytes());
    }
    format!("{:016x}", h.finish())
}

/// Renders `files` at `size` × `size` as a PNG, the icon scaled to fit and
/// centred on transparency; None when no file renders.
pub fn render(files: &[IconFile], size: u32) -> Option<Vec<u8>> {
    if let Some(svg) = files.iter().find(|f| f.kind == Kind::Svg)
        && let Some(png) = render_svg(&svg.path, size)
    {
        return Some(png);
    }
    let mut pngs: Vec<&IconFile> = files.iter().filter(|f| f.kind == Kind::Png).collect();
    // The smallest at least `size`, else the largest.
    pngs.sort_by_key(|f| (f.pixels < size, if f.pixels >= size { f.pixels } else { u32::MAX - f.pixels }));
    pngs.into_iter().find_map(|f| render_png(&f.path, size))
}

fn render_svg(path: &Path, size: u32) -> Option<Vec<u8>> {
    let data = std::fs::read(path).ok()?;
    let opt = usvg::Options {
        resources_dir: path.parent().map(Path::to_path_buf),
        ..Default::default()
    };
    let tree = usvg::Tree::from_data(&data, &opt).ok()?;
    let (w, h) = (tree.size().width(), tree.size().height());
    if !(w > 0.0 && h > 0.0) {
        return None;
    }
    let s = size as f32 / w.max(h);
    let (dx, dy) = ((size as f32 - w * s) / 2.0, (size as f32 - h * s) / 2.0);
    let mut out = tiny_skia::Pixmap::new(size, size)?;
    resvg::render(&tree, tiny_skia::Transform::from_row(s, 0.0, 0.0, s, dx, dy), &mut out.as_mut());
    out.encode_png().ok()
}

fn render_png(path: &Path, size: u32) -> Option<Vec<u8>> {
    let mut src = tiny_skia::Pixmap::decode_png(&std::fs::read(path).ok()?).ok()?;
    // Bicubic alone aliases when shrinking a lot: halve first.
    while src.width().max(src.height()) >= 2 * size && src.width() >= 2 && src.height() >= 2 {
        src = scaled(&src, src.width() / 2, src.height() / 2, 0.0, 0.0, src.width() / 2, src.height() / 2)?;
    }
    let (w, h) = (src.width() as f32, src.height() as f32);
    let s = size as f32 / w.max(h);
    let (dx, dy) = ((size as f32 - w * s) / 2.0, (size as f32 - h * s) / 2.0);
    scaled(&src, size, size, dx, dy, (w * s).round() as u32, (h * s).round() as u32)?
        .encode_png()
        .ok()
}

/// `src` drawn into a new `width` × `height` pixmap at (dx, dy), scaled to
/// `dw` × `dh`.
fn scaled(src: &tiny_skia::Pixmap, width: u32, height: u32, dx: f32, dy: f32, dw: u32, dh: u32) -> Option<tiny_skia::Pixmap> {
    let mut out = tiny_skia::Pixmap::new(width, height)?;
    let sx = dw.max(1) as f32 / src.width() as f32;
    let sy = dh.max(1) as f32 / src.height() as f32;
    let paint = tiny_skia::PixmapPaint {
        quality: tiny_skia::FilterQuality::Bicubic,
        ..Default::default()
    };
    out.draw_pixmap(0, 0, src.as_ref(), &paint, tiny_skia::Transform::from_row(sx, 0.0, 0.0, sy, dx, dy), None);
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn directory_names_give_sizes() {
        assert_eq!(dir_pixels("48x48/apps", None), Some(48));
        assert_eq!(dir_pixels("32x32@2/apps", None), Some(64));
        assert_eq!(dir_pixels("scalable/apps", None), Some(0));
        assert_eq!(dir_pixels("symbolic/apps", None), None);
    }
}
