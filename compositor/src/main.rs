//! pocketnix-compositor: the guest's Wayland compositor. It shows no
//! output of its own; the pocketnix app draws its windows, over the GUI
//! protocol (docs/gui-protocol.md), as Android windows.

mod apps;
mod damage;
mod flush;
mod gui;
mod icons;
mod keyboard;
mod launch;
mod launcher;
mod popup;
mod protocol;
mod state;
mod stats;
mod surface;
mod text_input;
mod touch;
mod watch;
mod windows;

use std::collections::HashMap;
use std::ffi::OsString;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;
use smithay::reexports::calloop::EventLoop;
use smithay::reexports::calloop::Interest;
use smithay::reexports::calloop::Mode;
use smithay::reexports::calloop::PostAction;
use smithay::reexports::calloop::channel;
use smithay::reexports::calloop::channel::Event;
use smithay::reexports::calloop::generic::Generic;
use smithay::reexports::calloop::ping::make_ping;
use smithay::reexports::wayland_server::Display;
use smithay::wayland::compositor::CompositorClientState;
use smithay::wayland::socket::ListeningSocketSource;
use tracing::info;

use crate::gui::Gui;
use crate::state::Channels;
use crate::state::ClientState;
use crate::state::State;

const USAGE: &str =
    "usage: pocketnix-compositor [--gui-socket PATH] [--wayland-display NAME] [--stats-socket PATH] [--log-hashes]";

struct Args {
    gui_socket: Option<PathBuf>,
    stats_socket: Option<PathBuf>,
    wayland_display: String,
    log_hashes: bool,
}

fn parse_args() -> Result<Args> {
    let mut args = Args {
        gui_socket: None,
        stats_socket: None,
        wayland_display: "wayland-0".into(),
        log_hashes: std::env::var("POCKETNIX_COMPOSITOR_LOG_HASHES").is_ok_and(|v| v == "1"),
    };
    let mut it = std::env::args_os().skip(1);
    while let Some(arg) = it.next() {
        let mut value = |name: &str| it.next().with_context(|| format!("{name} needs a value\n{USAGE}"));
        match arg.to_str() {
            Some("--gui-socket") => args.gui_socket = Some(value("--gui-socket")?.into()),
            Some("--stats-socket") => args.stats_socket = Some(value("--stats-socket")?.into()),
            Some("--wayland-display") => {
                args.wayland_display = value("--wayland-display")?
                    .into_string()
                    .map_err(|_| anyhow::anyhow!("--wayland-display must be UTF-8"))?
            }
            Some("--log-hashes") => args.log_hashes = true,
            Some("--help" | "-h") => {
                println!("{USAGE}");
                std::process::exit(0);
            }
            _ => bail!("unknown argument {arg:?}\n{USAGE}"),
        }
    }
    Ok(args)
}

/// Listening sockets passed by systemd socket activation, by
/// FileDescriptorName. The LISTEN_* variables are unset either way.
fn activated_sockets() -> HashMap<String, OwnedFd> {
    let mut out = HashMap::new();
    let pid = std::env::var("LISTEN_PID").ok().and_then(|p| p.parse::<u32>().ok());
    let count = std::env::var("LISTEN_FDS").ok().and_then(|n| n.parse::<i32>().ok());
    let names = std::env::var("LISTEN_FDNAMES").unwrap_or_default();
    // SAFETY: still single-threaded, before anything reads the environment.
    unsafe {
        std::env::remove_var("LISTEN_PID");
        std::env::remove_var("LISTEN_FDS");
        std::env::remove_var("LISTEN_FDNAMES");
    }
    let (Some(pid), Some(count)) = (pid, count) else { return out };
    if pid != std::process::id() {
        return out;
    }
    let names: Vec<&str> = names.split(':').collect();
    for i in 0..count {
        let fd = 3 + i;
        // SAFETY: systemd passed these descriptors to this process to own.
        unsafe {
            libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC);
            out.insert(names.get(i as usize).unwrap_or(&"").to_string(), OwnedFd::from_raw_fd(fd));
        }
    }
    out
}

fn runtime_dir() -> Result<PathBuf> {
    std::env::var_os("XDG_RUNTIME_DIR")
        .map(PathBuf::from)
        .context("XDG_RUNTIME_DIR is not set")
}

/// The name a client finds an activated Wayland socket by: relative to
/// XDG_RUNTIME_DIR when it's there, else its path.
fn socket_display_name(listener: &UnixListener) -> Option<OsString> {
    let addr = listener.local_addr().ok()?;
    let path = addr.as_pathname()?;
    match runtime_dir().ok().and_then(|dir| path.strip_prefix(dir).ok().map(Path::to_path_buf)) {
        Some(rel) => Some(rel.into_os_string()),
        None => Some(path.as_os_str().to_owned()),
    }
}

fn gui_listener(fd: Option<OwnedFd>, path: Option<PathBuf>) -> Result<UnixListener> {
    let listener = match fd {
        Some(fd) => UnixListener::from(fd),
        None => {
            let path = match path {
                Some(path) => path,
                None => runtime_dir()?.join("pocketnix-gui.sock"),
            };
            let listener = bind_private(&path)?;
            info!("GUI socket {}", path.display());
            listener
        }
    };
    listener.set_nonblocking(true)?;
    Ok(listener)
}

/// A listening socket at `path` only its owner may connect to.
fn bind_private(path: &Path) -> Result<UnixListener> {
    // A socket left by a compositor that died; nothing listens on it.
    if std::os::unix::net::UnixStream::connect(path).is_err() {
        let _ = std::fs::remove_file(path);
    }
    let listener = UnixListener::bind(path).with_context(|| format!("binding {}", path.display()))?;
    std::fs::set_permissions(path, std::os::unix::fs::PermissionsExt::from_mode(0o600))?;
    Ok(listener)
}

/// Answers each connection with one line of the stats' JSON (see `stats`).
fn serve_stats(listener: &UnixListener, state: &State) {
    while let Ok((mut stream, _)) = listener.accept() {
        if !gui::peer_allowed(&stream) {
            continue;
        }
        let mut line = state.stats.to_json("compositor", state.stats_windows()).to_string();
        line.push('\n');
        let _ = stream.set_write_timeout(Some(std::time::Duration::from_secs(1)));
        let _ = std::io::Write::write_all(&mut stream, line.as_bytes());
    }
}

fn main() -> Result<()> {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into());
    tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .init();
    // Before any thread starts, so that every thread leaves SIGHUP to it.
    let sighup = watch::sighup_fd().context("SIGHUP")?;
    let args = parse_args()?;
    let mut sockets = activated_sockets();

    let mut event_loop: EventLoop<State> = EventLoop::try_new()?;
    let mut display: Display<State> = Display::new()?;
    let dh = display.handle();

    let (wake, wake_source) = make_ping()?;
    event_loop
        .handle()
        .insert_source(wake_source, |_, _, _| {})
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    let gui = Gui::new(args.log_hashes, wake);
    let (icon_done, icon_rx) = channel::channel();
    let (launch_exits, exit_rx) = channel::channel();
    let channels = Channels {
        icon_jobs: launcher::icon_worker(icon_done),
        launch_exits,
    };
    insert(&event_loop, icon_rx, |event, _, state: &mut State| {
        if let Event::Msg((conn, frame)) = event {
            state.icon_done(conn, frame);
        }
    })?;
    insert(&event_loop, exit_rx, |event, _, state: &mut State| {
        if let Event::Msg((id, status)) = event {
            state.launch_exited(id, status);
        }
    })?;
    let mut state = State::new(&dh, event_loop.handle(), gui, channels)?;

    insert(&event_loop, Generic::new(sighup, Interest::READ, Mode::Level), |_, fd, state: &mut State| {
        if watch::drain(&**fd) {
            info!("SIGHUP: rescanning the app list");
            state.schedule_rescan();
        }
        Ok(PostAction::Continue)
    })?;
    match watch::Inotify::new() {
        Ok(inotify) => {
            let fd = inotify.fd().try_clone()?;
            state.inotify = Some(inotify);
            insert(&event_loop, Generic::new(fd, Interest::READ, Mode::Level), |_, _, state: &mut State| {
                if state.inotify.as_ref().is_some_and(|i| i.changed()) {
                    state.schedule_rescan();
                }
                Ok(PostAction::Continue)
            })?;
        }
        Err(e) => tracing::warn!("inotify: {e}; the app list rescans on SIGHUP only"),
    }

    let gui_listener = gui_listener(sockets.remove("gui"), args.gui_socket)?;
    event_loop
        .handle()
        .insert_source(Generic::new(gui_listener, Interest::READ, Mode::Level), |_, listener, state| {
            state.accept_gui(listener);
            Ok(PostAction::Continue)
        })
        .map_err(|e| anyhow::anyhow!("{e}"))?;

    let stats_path = match args.stats_socket {
        Some(path) => path,
        None => runtime_dir()?.join("pocketnix-compositor-stats.sock"),
    };
    let stats_listener = bind_private(&stats_path)?;
    stats_listener.set_nonblocking(true)?;
    insert(&event_loop, Generic::new(stats_listener, Interest::READ, Mode::Level), |_, listener, state: &mut State| {
        serve_stats(listener, state);
        Ok(PostAction::Continue)
    })?;

    let new_client = |dh: &mut smithay::reexports::wayland_server::DisplayHandle, stream| {
        let data = Arc::new(ClientState {
            compositor_state: CompositorClientState::default(),
        });
        if let Err(e) = dh.insert_client(stream, data) {
            tracing::warn!("adding a Wayland client: {e}");
        }
    };
    let wayland_display = match sockets.remove("wayland") {
        Some(fd) => {
            let listener = UnixListener::from(fd);
            listener.set_nonblocking(true)?;
            let name = socket_display_name(&listener).context("the activated Wayland socket has no path")?;
            let mut dh = dh.clone();
            event_loop
                .handle()
                .insert_source(Generic::new(listener, Interest::READ, Mode::Level), move |_, listener, _| {
                    while let Ok((stream, _)) = listener.accept() {
                        new_client(&mut dh, stream);
                    }
                    Ok(PostAction::Continue)
                })
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            name
        }
        None => {
            let source = ListeningSocketSource::with_name(&args.wayland_display)
                .with_context(|| format!("binding Wayland socket {}", args.wayland_display))?;
            let name = source.socket_name().to_owned();
            let mut dh = dh.clone();
            event_loop
                .handle()
                .insert_source(source, move |stream, _, _| new_client(&mut dh, stream))
                .map_err(|e| anyhow::anyhow!("{e}"))?;
            name
        }
    };
    info!("Wayland socket {}", String::from_utf8_lossy(wayland_display.as_bytes()));
    // SAFETY: the threads running so far never read the environment.
    unsafe { std::env::set_var("WAYLAND_DISPLAY", &wayland_display) };
    state.scan_apps();

    let poll_fd = display.backend().poll_fd().try_clone_to_owned()?;
    event_loop
        .handle()
        .insert_source(Generic::new(poll_fd, Interest::READ, Mode::Level), move |_, _, state| {
            display.dispatch_clients(state)?;
            Ok(PostAction::Continue)
        })
        .map_err(|e| anyhow::anyhow!("{e}"))?;

    event_loop.run(None, &mut state, |state| {
        state.flush();
        if let Err(e) = state.dh.flush_clients() {
            tracing::warn!("flushing Wayland clients: {e}");
        }
    })?;
    Ok(())
}

fn insert<S, F>(event_loop: &EventLoop<State>, source: S, callback: F) -> Result<()>
where
    S: smithay::reexports::calloop::EventSource + 'static,
    F: FnMut(S::Event, &mut S::Metadata, &mut State) -> S::Ret + 'static,
{
    event_loop
        .handle()
        .insert_source(source, callback)
        .map(|_| ())
        .map_err(|e| anyhow::anyhow!("{e}"))
}
