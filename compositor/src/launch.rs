//! Starting apps for the app's `launch` messages (docs/gui-protocol.md,
//! Launching), and what the compositor reads about a client's process.

use std::path::Path;
use std::process::Command;
use std::process::Stdio;
use std::time::Instant;

use crate::apps::Entry;
use crate::apps::Env;
use crate::apps::exec_argv;

/// No window within this long fails a launch.
pub const TIMEOUT: std::time::Duration = std::time::Duration::from_secs(90);

/// A launch or tab id: 1–64 of `[A-Za-z0-9_-]`.
pub fn valid_id(id: &str) -> bool {
    (1..=64).contains(&id.len()) && id.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

#[derive(Clone, Debug, PartialEq)]
pub enum Outcome {
    Pending,
    /// Ended with this toplevel, by binding or activation.
    Window(u32),
    Failed { reason: &'static str, message: String },
    Cancelled,
}

pub struct Launch {
    pub id: String,
    pub app: String,
    pub tab: String,
    pub token: String,
    /// The scope unit, without `.scope`.
    pub unit: String,
    pub pid: Option<u32>,
    pub started: Instant,
    pub outcome: Outcome,
}

/// systemd-escape's rules for a unit name part.
pub fn systemd_escape(s: &str) -> String {
    let mut out = String::new();
    for (i, b) in s.bytes().enumerate() {
        match b {
            b'/' => out.push('-'),
            b'.' if i == 0 => out.push_str("\\x2e"),
            b if b.is_ascii_alphanumeric() || b == b':' || b == b'_' || b == b'.' => out.push(b as char),
            b => out.push_str(&format!("\\x{b:02x}")),
        }
    }
    out
}

fn random_hex() -> String {
    let mut bytes = [0u8; 4];
    if let Ok(mut f) = std::fs::File::open("/dev/urandom") {
        let _ = std::io::Read::read_exact(&mut f, &mut bytes);
    }
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// What a launch runs: the entry's or action's argv, or a D-Bus activation.
pub fn command(entry: &Entry, action: &str, token: &str) -> Result<Vec<String>, String> {
    let exec = if action.is_empty() {
        entry.exec.as_deref()
    } else {
        entry.action(action).and_then(|a| a.exec.as_deref())
    };
    if let Some(exec) = exec {
        return exec_argv(exec, &entry.icon, &entry.name, &entry.path);
    }
    if !entry.dbus {
        return Err(format!("{} has no Exec", entry.id));
    }
    // org.freedesktop.Application on the entry's well-known name.
    let object = format!("/{}", entry.id.replace('.', "/").replace('-', "_"));
    let mut argv: Vec<String> = ["busctl", "--user", "call", &entry.id, &object, "org.freedesktop.Application"]
        .map(str::to_owned)
        .to_vec();
    let platform = ["2", "activation-token", "s", token, "desktop-startup-id", "s", token].map(str::to_owned);
    if action.is_empty() {
        argv.extend(["Activate".into(), "a{sv}".into()]);
    } else {
        argv.extend(["ActivateAction".into(), "sava{sv}".into(), action.into(), "0".into()]);
    }
    argv.extend(platform);
    Ok(argv)
}

/// Starts `argv` in its own scope, its output in the journal under the
/// entry's id. Returns the scope's unit name and the child.
pub fn spawn(
    env: &Env,
    entry: &Entry,
    argv: &[String],
    token: &str,
    tab: &str,
) -> std::io::Result<(String, std::process::Child)> {
    let unit = format!("app-pocketnix-{}-{}", systemd_escape(&entry.id), random_hex());
    let mut cmd = Command::new("systemd-cat");
    cmd.arg(format!("--identifier={}", entry.id))
        .args(["systemd-run", "--user", "--scope", "--slice=app.slice", "--quiet"])
        .arg(format!("--unit={unit}"))
        .arg(format!("--description={}", entry.name))
        .arg("--")
        .args(argv)
        .env("XDG_ACTIVATION_TOKEN", token)
        .env("DESKTOP_STARTUP_ID", token)
        .stdin(Stdio::null());
    if tab.is_empty() {
        cmd.env_remove("POCKETNIX_TAB");
    } else {
        cmd.env("POCKETNIX_TAB", tab);
    }
    let dir = entry
        .working_dir
        .as_deref()
        .map(Path::new)
        .filter(|d| d.is_dir())
        .or(env.home.as_deref());
    if let Some(dir) = dir {
        cmd.current_dir(dir);
    }
    let child = cmd.spawn()?;
    Ok((unit, child))
}

/// Stops a launch's scope and its process.
pub fn stop(launch: &Launch) {
    if let Some(pid) = launch.pid {
        // SAFETY: kill with a pid we started and a valid signal.
        unsafe { libc::kill(pid as i32, libc::SIGTERM) };
    }
    let _ = Command::new("systemctl")
        .args(["--user", "stop", "--no-block", &format!("{}.scope", launch.unit)])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map(|mut c| std::thread::spawn(move || c.wait()));
}

/// Whether a process runs in a launch's scope.
pub fn in_scope(pid: u32, unit: &str) -> bool {
    let Ok(cgroup) = std::fs::read_to_string(format!("/proc/{pid}/cgroup")) else { return false };
    let want = format!("/{unit}.scope");
    cgroup.lines().any(|l| l.ends_with(&want) || l.contains(&format!("{want}/")))
}

/// `POCKETNIX_TAB` from a process's own environment, if it's a valid id.
pub fn environ_tab(pid: u32) -> Option<String> {
    let environ = std::fs::read(format!("/proc/{pid}/environ")).ok()?;
    environ
        .split(|b| *b == 0)
        .find_map(|var| var.strip_prefix(b"POCKETNIX_TAB="))
        .and_then(|v| std::str::from_utf8(v).ok())
        .filter(|v| valid_id(v))
        .map(str::to_owned)
}

/// A child's exit as `launch-failed`'s message, or None for status 0.
pub fn exit_message(status: std::process::ExitStatus) -> Option<String> {
    use std::os::unix::process::ExitStatusExt;
    if status.success() {
        return None;
    }
    Some(match (status.code(), status.signal()) {
        (Some(code), _) => format!("exited with status {code}"),
        (None, Some(sig)) => format!("killed by signal {sig}"),
        _ => "exited".into(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn escapes_like_systemd() {
        assert_eq!(systemd_escape("org.gnome.Calculator"), "org.gnome.Calculator");
        assert_eq!(systemd_escape("foot-server"), "foot\\x2dserver");
        assert_eq!(systemd_escape(".x y"), "\\x2ex\\x20y");
    }

    #[test]
    fn ids() {
        assert!(valid_id("0123456789abcdef"));
        assert!(valid_id("A_b-9"));
        assert!(!valid_id(""));
        assert!(!valid_id("a b"));
        assert!(!valid_id(&"a".repeat(65)));
    }
}
