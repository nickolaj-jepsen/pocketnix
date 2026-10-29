//! The app list (docs/gui-protocol.md, The app list): desktop entries from
//! the XDG data directories, named in the compositor's locale, with their
//! icons' keys and how to start them.

use std::collections::HashMap;
use std::collections::HashSet;
use std::ffi::OsStr;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::path::PathBuf;

use serde_json::Value;
use serde_json::json;
use tracing::warn;

use crate::icons::HashCache;
use crate::icons::IconIndex;
use crate::icons::IconSet;
use crate::watch::Watch;
use crate::protocol::MAX_FRAME_PAYLOAD;

/// Where the list and icons come from: the compositor's environment.
#[derive(Clone, Debug, Default)]
pub struct Env {
    pub data_home: PathBuf,
    pub data_dirs: Vec<PathBuf>,
    pub home: Option<PathBuf>,
    pub path: Vec<PathBuf>,
    /// Locale names to try, most specific first (`da_DK@x`, `da_DK`, `da@x`, `da`).
    pub locales: Vec<String>,
}

impl Env {
    pub fn from_process() -> Self {
        let var = |k: &str| std::env::var_os(k).filter(|v| !v.is_empty());
        let home = var("HOME").map(PathBuf::from);
        let data_home = var("XDG_DATA_HOME")
            .map(PathBuf::from)
            .or_else(|| home.as_ref().map(|h| h.join(".local/share")))
            .unwrap_or_default();
        let data_dirs = var("XDG_DATA_DIRS")
            .map(|v| std::env::split_paths(&v).filter(|p| p.is_absolute()).collect())
            .unwrap_or_else(|| vec!["/usr/local/share".into(), "/usr/share".into()]);
        let path = var("PATH").map(|v| std::env::split_paths(&v).collect()).unwrap_or_default();
        let locale = ["LC_ALL", "LC_MESSAGES", "LANG"]
            .iter()
            .find_map(|k| std::env::var(k).ok().filter(|v| !v.is_empty()))
            .unwrap_or_default();
        Self {
            data_home,
            data_dirs,
            home,
            path,
            locales: locale_names(&locale),
        }
    }

    fn data_bases(&self) -> Vec<PathBuf> {
        let mut out = Vec::new();
        if self.data_home.is_absolute() {
            out.push(self.data_home.clone());
        }
        out.extend(self.data_dirs.iter().cloned());
        out
    }

    /// Whether `program` names an executable file, directly or on PATH.
    pub fn find_program(&self, program: &str) -> bool {
        let executable = |p: &Path| p.metadata().is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0);
        if program.contains('/') {
            return executable(Path::new(program));
        }
        self.path.iter().any(|dir| executable(&dir.join(program)))
    }
}

/// The Desktop Entry spec's locale matching order for a POSIX locale name.
fn locale_names(locale: &str) -> Vec<String> {
    if locale.is_empty() || locale == "C" || locale == "POSIX" || locale.starts_with("C.") {
        return Vec::new();
    }
    let (rest, modifier) = match locale.split_once('@') {
        Some((r, m)) => (r, Some(m)),
        None => (locale, None),
    };
    let rest = rest.split('.').next().unwrap_or(rest);
    let (lang, country) = match rest.split_once('_') {
        Some((l, c)) => (l, Some(c)),
        None => (rest, None),
    };
    let mut out = Vec::new();
    if let (Some(c), Some(m)) = (country, modifier) {
        out.push(format!("{lang}_{c}@{m}"));
    }
    if let Some(c) = country {
        out.push(format!("{lang}_{c}"));
    }
    if let Some(m) = modifier {
        out.push(format!("{lang}@{m}"));
    }
    out.push(lang.to_owned());
    out
}

/// One parsed `.desktop` file: its groups' keys, values still escaped.
struct DesktopFile {
    groups: HashMap<String, HashMap<String, String>>,
}

impl DesktopFile {
    fn parse(text: &str) -> Result<Self, String> {
        let mut groups: HashMap<String, HashMap<String, String>> = HashMap::new();
        let mut current: Option<String> = None;
        for (n, line) in text.lines().enumerate() {
            let line = line.trim_start();
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            if let Some(name) = line.trim_end().strip_prefix('[').and_then(|l| l.strip_suffix(']')) {
                groups.entry(name.to_owned()).or_default();
                current = Some(name.to_owned());
                continue;
            }
            let Some((key, value)) = line.split_once('=') else {
                return Err(format!("line {}: not a key or group", n + 1));
            };
            let Some(group) = &current else {
                return Err(format!("line {}: a key before any group", n + 1));
            };
            let entries = groups.get_mut(group).expect("the group was added");
            entries.entry(key.trim().to_owned()).or_insert_with(|| value.trim().to_owned());
        }
        if !groups.contains_key("Desktop Entry") {
            return Err("no [Desktop Entry] group".into());
        }
        Ok(Self { groups })
    }

    fn raw(&self, group: &str, key: &str) -> Option<&str> {
        self.groups.get(group)?.get(key).map(String::as_str)
    }

    fn string(&self, group: &str, key: &str) -> Option<String> {
        self.raw(group, key).map(unescape)
    }

    fn localized(&self, group: &str, key: &str, locales: &[String]) -> Option<String> {
        locales
            .iter()
            .find_map(|l| self.string(group, &format!("{key}[{l}]")))
            .or_else(|| self.string(group, key))
    }

    fn localized_list(&self, group: &str, key: &str, locales: &[String]) -> Vec<String> {
        locales
            .iter()
            .find_map(|l| self.raw(group, &format!("{key}[{l}]")))
            .or_else(|| self.raw(group, key))
            .map(split_list)
            .unwrap_or_default()
    }

    fn bool(&self, group: &str, key: &str) -> bool {
        self.raw(group, key) == Some("true")
    }
}

/// A string value's escapes: `\s`, `\n`, `\t`, `\r`, `\\`.
fn unescape(v: &str) -> String {
    let mut out = String::with_capacity(v.len());
    let mut chars = v.chars();
    while let Some(c) = chars.next() {
        if c != '\\' {
            out.push(c);
            continue;
        }
        match chars.next() {
            Some('s') => out.push(' '),
            Some('n') => out.push('\n'),
            Some('t') => out.push('\t'),
            Some('r') => out.push('\r'),
            Some('\\') => out.push('\\'),
            Some(other) => {
                out.push('\\');
                out.push(other);
            }
            None => out.push('\\'),
        }
    }
    out
}

/// A list value: `;`-separated, `\;` a literal semicolon.
fn split_list(v: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut cur = String::new();
    let mut chars = v.chars();
    while let Some(c) = chars.next() {
        match c {
            '\\' if chars.as_str().starts_with(';') => {
                chars.next();
                cur.push(';');
            }
            ';' => out.push(unescape(&std::mem::take(&mut cur))),
            c => cur.push(c),
        }
    }
    if !cur.is_empty() {
        out.push(unescape(&cur));
    }
    out.into_iter().filter(|s| !s.is_empty()).collect()
}

/// An `Exec` value split into arguments by the Desktop Entry spec's quoting
/// rules, field codes still in place. Err for an unterminated quote.
fn split_exec(exec: &str) -> Result<Vec<String>, String> {
    let mut args = Vec::new();
    let mut cur = String::new();
    let mut in_arg = false;
    let mut chars = exec.chars();
    while let Some(c) = chars.next() {
        match c {
            '"' => {
                in_arg = true;
                loop {
                    match chars.next() {
                        None => return Err("unterminated quote in Exec".into()),
                        Some('"') => break,
                        Some('\\') => match chars.next() {
                            Some(e @ ('"' | '`' | '$' | '\\')) => cur.push(e),
                            Some(e) => {
                                cur.push('\\');
                                cur.push(e);
                            }
                            None => return Err("unterminated quote in Exec".into()),
                        },
                        Some(c) => cur.push(c),
                    }
                }
            }
            ' ' | '\t' | '\n' => {
                if in_arg {
                    args.push(std::mem::take(&mut cur));
                    in_arg = false;
                }
            }
            c => {
                in_arg = true;
                cur.push(c);
            }
        }
    }
    if in_arg {
        args.push(cur);
    }
    Ok(args)
}

/// The argv an `Exec` value runs, with field codes expanded: files and URLs
/// dropped, `%i`, `%c`, `%k` and `%%` expanded, others dropped.
pub fn exec_argv(exec: &str, icon: &str, name: &str, path: &Path) -> Result<Vec<String>, String> {
    let mut argv = Vec::new();
    for arg in split_exec(exec)? {
        if arg == "%i" {
            if !icon.is_empty() {
                argv.extend(["--icon".to_owned(), icon.to_owned()]);
            }
            continue;
        }
        let mut out = String::new();
        let mut had_code = false;
        let mut chars = arg.chars();
        while let Some(c) = chars.next() {
            if c != '%' {
                out.push(c);
                continue;
            }
            match chars.next() {
                Some('%') => out.push('%'),
                Some('c') => out.push_str(name),
                Some('k') => out.push_str(&path.to_string_lossy()),
                Some(_) | None => had_code = true,
            }
        }
        if !(had_code && out.is_empty()) {
            argv.push(out);
        }
    }
    if argv.is_empty() {
        return Err("Exec is empty".into());
    }
    Ok(argv)
}

#[derive(Clone, Debug)]
pub struct Action {
    pub id: String,
    pub name: String,
    pub icon_key: String,
    pub exec: Option<String>,
}

#[derive(Clone, Debug)]
pub struct Entry {
    pub id: String,
    pub path: PathBuf,
    pub name: String,
    pub generic_name: String,
    pub comment: String,
    pub keywords: Vec<String>,
    pub categories: Vec<String>,
    /// The `Icon=` value, for `%i`.
    pub icon: String,
    pub icon_key: String,
    pub no_display: bool,
    pub exec: Option<String>,
    pub dbus: bool,
    pub working_dir: Option<String>,
    pub wm_class: Option<String>,
    pub actions: Vec<Action>,
}

impl Entry {
    pub fn action(&self, id: &str) -> Option<&Action> {
        self.actions.iter().find(|a| a.id == id)
    }
}

/// One scan's result.
#[derive(Default)]
pub struct AppList {
    pub entries: Vec<Entry>,
    by_id: HashMap<String, usize>,
    by_wm_class: HashMap<String, usize>,
    /// Icon files by key, for rendering.
    pub icons: HashMap<String, IconSet>,
    /// The `apps` message.
    pub message: Value,
    /// Directories whose changes can change the list.
    pub watch: Vec<Watch>,
}

impl AppList {
    pub fn get(&self, id: &str) -> Option<&Entry> {
        self.by_id.get(id).map(|i| &self.entries[*i])
    }

    /// The entry a toplevel with this app id is: by id, else by `StartupWMClass`.
    pub fn for_app_id(&self, app_id: &str) -> Option<&Entry> {
        self.by_id
            .get(app_id)
            .or_else(|| self.by_wm_class.get(app_id))
            .map(|i| &self.entries[*i])
    }
}

/// Desktop files below `dir` by desktop file id, recursively.
fn collect(dir: &Path, prefix: &str, depth: usize, out: &mut Vec<(String, PathBuf)>, dirs: &mut Vec<Watch>) {
    let Ok(rd) = std::fs::read_dir(dir) else { return };
    dirs.push((dir.to_path_buf(), None));
    let mut items: Vec<_> = rd.flatten().collect();
    items.sort_by_key(|e| e.file_name());
    for e in items {
        let Ok(name) = e.file_name().into_string() else { continue };
        let path = e.path();
        // Profiles link their files and directories into the store.
        let Ok(meta) = std::fs::metadata(&path) else { continue };
        if meta.is_dir() {
            if depth < 8 {
                collect(&path, &format!("{prefix}{name}-"), depth + 1, out, dirs);
            }
        } else if let Some(stem) = name.strip_suffix(".desktop") {
            out.push((format!("{prefix}{stem}"), path));
        }
    }
}

/// The entries whose change moves `path` elsewhere: each symlink on the way
/// to it (a profile switch replaces one), and the first missing directory.
fn path_watches(path: &Path) -> Vec<Watch> {
    let mut out = Vec::new();
    let named = |dir: &Path, name: &OsStr| (dir.to_path_buf(), Some(HashSet::from([name.to_owned()])));
    let mut resolved = PathBuf::from("/");
    let mut parts: Vec<std::ffi::OsString> = path.components().skip(1).map(|c| c.as_os_str().to_owned()).collect();
    parts.reverse();
    let mut hops = 0;
    while let Some(part) = parts.pop() {
        let next = resolved.join(&part);
        match std::fs::read_link(&next) {
            Ok(target) if hops < 40 => {
                hops += 1;
                out.push(named(&resolved, &part));
                let target = if target.is_absolute() { target } else { resolved.join(target) };
                let mut more: Vec<_> = target.components().map(|c| c.as_os_str().to_owned()).collect();
                if target.is_absolute() {
                    resolved = PathBuf::from("/");
                    more.remove(0);
                }
                for p in more.into_iter().rev() {
                    parts.push(p);
                }
            }
            _ if part == ".." => {
                resolved.pop();
            }
            _ if part == "." => {}
            _ if !next.exists() => {
                out.push(named(&resolved, &part));
                break;
            }
            _ => resolved = next,
        }
    }
    out
}

pub fn scan(env: &Env, hashes: &mut HashCache) -> AppList {
    let bases = env.data_bases();
    let mut watch = Vec::new();
    let mut seen: HashSet<String> = HashSet::new();
    let mut files = Vec::new();
    for base in &bases {
        let mut found = Vec::new();
        collect(&base.join("applications"), "", 0, &mut found, &mut watch);
        for (id, path) in found {
            if seen.insert(id.clone()) {
                files.push((id, path));
            }
        }
        watch.extend(path_watches(&base.join("applications")));
    }

    let mut icon_bases: Vec<PathBuf> = bases.iter().map(|b| b.join("icons")).collect();
    if let Some(home) = &env.home {
        icon_bases.push(home.join(".icons"));
    }
    let pixmaps: Vec<PathBuf> = env.data_dirs.iter().map(|d| d.join("pixmaps")).collect();
    let index = IconIndex::build(&icon_bases, &pixmaps);
    let mut icons: HashMap<String, IconSet> = HashMap::new();
    let mut icon_key = |icon: &str, icons: &mut HashMap<String, IconSet>| {
        let set = index.lookup(icon);
        let key = crate::icons::key(&set, hashes);
        if !key.is_empty() {
            icons.entry(key.clone()).or_insert_with(|| std::sync::Arc::new(set));
        }
        key
    };

    let mut entries = Vec::new();
    for (id, path) in files {
        let text = match std::fs::read(&path).map(String::from_utf8) {
            Ok(Ok(text)) => text,
            Ok(Err(_)) => {
                warn!("{}: not UTF-8", path.display());
                continue;
            }
            Err(e) => {
                warn!("{}: {e}", path.display());
                continue;
            }
        };
        let file = match DesktopFile::parse(&text) {
            Ok(f) => f,
            Err(e) => {
                warn!("{}: {e}", path.display());
                continue;
            }
        };
        let g = "Desktop Entry";
        if file.raw(g, "Type") != Some("Application") || file.bool(g, "Hidden") || file.bool(g, "Terminal") {
            continue;
        }
        let Some(name) = file.localized(g, "Name", &env.locales) else { continue };
        let exec = file.string(g, "Exec").filter(|e| !e.is_empty());
        let dbus = file.bool(g, "DBusActivatable");
        if exec.is_none() && !dbus {
            continue;
        }
        if let Some(try_exec) = file.string(g, "TryExec").filter(|t| !t.is_empty())
            && !env.find_program(&try_exec)
        {
            continue;
        }
        let icon = file.localized(g, "Icon", &env.locales).unwrap_or_default();
        let actions = split_list(file.raw(g, "Actions").unwrap_or_default())
            .into_iter()
            .filter_map(|aid| {
                let group = format!("Desktop Action {aid}");
                let name = file.localized(&group, "Name", &env.locales)?;
                let icon = file.localized(&group, "Icon", &env.locales).unwrap_or_default();
                Some(Action {
                    icon_key: icon_key(&icon, &mut icons),
                    id: aid,
                    name,
                    exec: file.string(&group, "Exec").filter(|e| !e.is_empty()),
                })
            })
            .collect();
        entries.push(Entry {
            icon_key: icon_key(&icon, &mut icons),
            id,
            path,
            name,
            generic_name: file.localized(g, "GenericName", &env.locales).unwrap_or_default(),
            comment: file.localized(g, "Comment", &env.locales).unwrap_or_default(),
            keywords: file.localized_list(g, "Keywords", &env.locales),
            categories: file.localized_list(g, "Categories", &env.locales),
            icon,
            no_display: file.bool(g, "NoDisplay") || file.raw(g, "OnlyShowIn").is_some(),
            exec,
            dbus,
            working_dir: file.string(g, "Path").filter(|p| !p.is_empty()),
            wm_class: file.string(g, "StartupWMClass").filter(|c| !c.is_empty()),
            actions,
        });
    }
    entries.sort_by(|a, b| a.id.cmp(&b.id));
    let by_id = entries.iter().enumerate().map(|(i, e)| (e.id.clone(), i)).collect();
    let mut by_wm_class = HashMap::new();
    for (i, e) in entries.iter().enumerate() {
        if let Some(class) = &e.wm_class {
            by_wm_class.entry(class.clone()).or_insert(i);
        }
    }
    let message = message(&entries);
    AppList {
        entries,
        by_id,
        by_wm_class,
        icons,
        message,
        watch,
    }
}

/// The `apps` message, shrunk to fit a frame: keywords go first, then
/// comments and generic names.
fn message(entries: &[Entry]) -> Value {
    for level in 0..3 {
        let apps: Vec<Value> = entries
            .iter()
            .map(|e| {
                json!({
                    "id": e.id,
                    "name": e.name,
                    "genericName": if level < 2 { e.generic_name.as_str() } else { "" },
                    "comment": if level < 2 { e.comment.as_str() } else { "" },
                    "keywords": if level < 1 { e.keywords.clone() } else { Vec::new() },
                    "categories": e.categories,
                    "icon": e.icon_key,
                    "noDisplay": e.no_display,
                    "actions": e.actions.iter().map(|a| json!({"id": a.id, "name": a.name, "icon": a.icon_key})).collect::<Vec<_>>(),
                })
            })
            .collect();
        let msg = json!({"type": "apps", "apps": apps});
        let len = serde_json::to_vec(&msg).map_or(usize::MAX, |v| v.len());
        if len <= MAX_FRAME_PAYLOAD {
            return msg;
        }
        warn!("the app list is {len} bytes; dropping {}", ["keywords", "comments and generic names", "nothing more"][level]);
    }
    json!({"type": "apps", "apps": []})
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exec_quoting_and_field_codes() {
        let path = Path::new("/a/b.desktop");
        assert_eq!(
            exec_argv(r#"foo "a b" "c\"d" --x=%f %U %i %c %k 100%%"#, "ic", "Name", path).unwrap(),
            vec!["foo", "a b", "c\"d", "--x=", "--icon", "ic", "Name", "/a/b.desktop", "100%"]
        );
        assert_eq!(exec_argv("foo %i", "", "N", path).unwrap(), vec!["foo"]);
        assert!(exec_argv("foo \"bar", "", "N", path).is_err());
        assert!(exec_argv("%F", "", "N", path).is_err());
    }

    #[test]
    fn locales_match_most_specific_first() {
        assert_eq!(locale_names("da_DK.UTF-8@euro"), vec!["da_DK@euro", "da_DK", "da@euro", "da"]);
        assert_eq!(locale_names("de"), vec!["de"]);
        assert!(locale_names("C.UTF-8").is_empty());
    }

    #[test]
    fn lists_and_escapes() {
        assert_eq!(split_list(r"a;b\;c;;d"), vec!["a", "b;c", "d"]);
        assert_eq!(unescape(r"a\sb\\n"), "a b\\n");
    }
}
