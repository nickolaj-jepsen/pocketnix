//! The app list, icons, launches and activation on the compositor's side
//! (docs/gui-protocol.md, The app list, Launching).

use std::collections::VecDeque;
use std::process::ExitStatus;
use std::sync::mpsc;
use std::time::Duration;
use std::time::Instant;

use anyhow::Result;
use anyhow::bail;
use serde::Deserialize;
use serde_json::json;
use smithay::reexports::calloop::channel::Sender;
use smithay::reexports::calloop::timer::TimeoutAction;
use smithay::reexports::calloop::timer::Timer;
use smithay::reexports::wayland_server::backend::ClientId;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Serial;
use smithay::wayland::xdg_activation::XdgActivationHandler;
use smithay::wayland::xdg_activation::XdgActivationState;
use smithay::wayland::xdg_activation::XdgActivationToken;
use smithay::wayland::xdg_activation::XdgActivationTokenData;
use tracing::debug;
use tracing::info;
use tracing::warn;

use crate::apps;
use crate::icons;
use crate::icons::IconSet;
use crate::launch;
use crate::launch::Launch;
use crate::launch::Outcome;
use crate::protocol;
use crate::state::State;
use crate::state::surface_id;

/// How long a client's own token stays valid, and how recent the input
/// event it was minted with must be.
const CLIENT_TOKEN_AGE: Duration = Duration::from_secs(10);
const RESCAN_DEBOUNCE: Duration = Duration::from_secs(1);

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct LaunchMsg {
    id: String,
    app: String,
    action: String,
    tab: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct IdMsg {
    id: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct IconMsg {
    icon: String,
    size: u32,
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct MoveMsg {
    id: u32,
    tab: String,
}

/// A request for the icon renderer: which connection asked, and what.
pub struct IconJob {
    pub conn: u64,
    pub key: String,
    pub size: u32,
    pub files: Option<IconSet>,
}

/// Renders icons off the event loop, answering through `done`.
pub fn icon_worker(done: Sender<(u64, Vec<u8>)>) -> mpsc::Sender<IconJob> {
    let (tx, rx) = mpsc::channel::<IconJob>();
    std::thread::Builder::new()
        .name("icons".into())
        .spawn(move || {
            while let Ok(job) = rx.recv() {
                let png = job
                    .files
                    .and_then(|f| icons::render(&f, job.size))
                    .filter(|p| 3 + job.key.len() + p.len() <= protocol::MAX_FRAME_PAYLOAD)
                    .unwrap_or_default();
                let mut payload = Vec::with_capacity(3 + job.key.len() + png.len());
                payload.extend_from_slice(&(job.size as u16).to_be_bytes());
                payload.push(job.key.len() as u8);
                payload.extend_from_slice(job.key.as_bytes());
                payload.extend_from_slice(&png);
                if done.send((job.conn, protocol::frame(protocol::ICON, &payload))).is_err() {
                    return;
                }
            }
        })
        .expect("spawning the icon thread");
    tx
}

/// Input serials the compositor sent, for checking a client's own tokens.
#[derive(Default)]
pub struct InputSerials(VecDeque<(Serial, Instant, ClientId)>);

impl InputSerials {
    pub fn push(&mut self, serial: Serial, client: ClientId) {
        if self.0.len() >= 32 {
            self.0.pop_front();
        }
        self.0.push_back((serial, Instant::now(), client));
    }

    fn recent(&self, serial: Serial, client: &ClientId) -> bool {
        self.0
            .iter()
            .any(|(s, at, c)| *s == serial && c == client && at.elapsed() < CLIENT_TOKEN_AGE)
    }
}

impl State {
    /// Rescans the app list now.
    pub fn scan_apps(&mut self) {
        let list = apps::scan(&self.env, &mut self.hashes);
        if let Some(inotify) = &mut self.inotify {
            inotify.set(&list.watch);
        }
        debug!("{} apps", list.entries.len());
        self.apps = list;
    }

    /// Rescans the app list now, and tells the app if it changed.
    pub fn rescan_apps(&mut self) {
        self.scan_apps();
        self.send_apps();
    }

    /// Rescans once things have been quiet for `RESCAN_DEBOUNCE`.
    pub fn schedule_rescan(&mut self) {
        if let Some(token) = self.rescan_timer.take() {
            self.lh.remove(token);
        }
        let timer = self.lh.insert_source(Timer::from_duration(RESCAN_DEBOUNCE), |_, _, state| {
            state.rescan_timer = None;
            state.rescan_apps();
            TimeoutAction::Drop
        });
        match timer {
            Ok(token) => self.rescan_timer = Some(token),
            Err(e) => warn!("rescan timer: {e}"),
        }
    }

    pub fn send_apps(&mut self) {
        let message = self.apps.message.clone();
        if let Some(conn) = self.gui.active_conn_mut()
            && conn.apps.as_ref() != Some(&message)
        {
            conn.send(protocol::json_frame(&message));
            conn.apps = Some(message);
        }
    }

    pub fn on_icon(&mut self, conn: u64, m: IconMsg) {
        let size = m.size.clamp(16, 512);
        let files = self.apps.icons.get(&m.icon).cloned();
        let key: String = m.icon.chars().take(255).collect();
        let _ = self.icon_jobs.send(IconJob { conn, key, size, files });
    }

    pub fn icon_done(&mut self, conn: u64, frame: Vec<u8>) {
        if let Some(conn) = self.gui.conns.get(&conn) {
            conn.send(frame);
        }
    }

    pub fn on_launch(&mut self, m: LaunchMsg) -> Result<()> {
        if !launch::valid_id(&m.id) {
            bail!("malformed launch id {:?}", m.id);
        }
        if self.launches.contains_key(&m.id) {
            self.resend_launch(&m.id);
            return Ok(());
        }
        let tab = if launch::valid_id(&m.tab) { m.tab } else { String::new() };
        let mut l = Launch {
            id: m.id.clone(),
            app: m.app.clone(),
            tab,
            token: String::new(),
            unit: String::new(),
            pid: None,
            started: Instant::now(),
            outcome: Outcome::Pending,
        };
        let entry = self
            .apps
            .get(&m.app)
            .filter(|e| m.action.is_empty() || e.action(&m.action).is_some())
            .cloned();
        let Some(entry) = entry else {
            let message = match self.apps.get(&m.app) {
                Some(_) => format!("{} has no action {}", m.app, m.action),
                None => format!("no app {}", m.app),
            };
            self.launches.insert(m.id.clone(), l);
            self.fail_launch(&m.id, "unknown", message);
            return Ok(());
        };
        l.token = self.xdg_activation_state.create_external_token(None).0.as_str().to_owned();
        let started = launch::command(&entry, &m.action, &l.token)
            .and_then(|argv| launch::spawn(&self.env, &entry, &argv, &l.token, &l.tab, &self.keyboard_layout).map_err(|e| format!("couldn't start: {e}")));
        let (unit, mut child) = match started {
            Ok(s) => s,
            Err(message) => {
                self.launches.insert(m.id.clone(), l);
                self.fail_launch(&m.id, "exec", message);
                return Ok(());
            }
        };
        info!("launch {}: {} in {unit}", m.id, entry.id);
        l.unit = unit;
        l.pid = Some(child.id());
        self.launches.insert(m.id.clone(), l);
        let exits = self.launch_exits.clone();
        let id = m.id.clone();
        std::thread::spawn(move || {
            if let Ok(status) = child.wait() {
                let _ = exits.send((id, status));
            }
        });
        let id = m.id.clone();
        if let Err(e) = self.lh.insert_source(Timer::from_duration(launch::TIMEOUT), move |_, _, state| {
            if state.launches.get(&id).is_some_and(|l| l.outcome == Outcome::Pending) {
                state.fail_launch(&id, "timeout", "no window within 90 s".into());
            }
            TimeoutAction::Drop
        }) {
            warn!("launch timer: {e}");
        }
        self.send_json(json!({"type": "launch-started", "id": m.id}));
        Ok(())
    }

    pub fn on_launch_cancel(&mut self, m: IdMsg) {
        let Some(l) = self.launches.get(&m.id) else { return };
        if l.outcome != Outcome::Pending {
            return;
        }
        launch::stop(l);
        self.end_launch(&m.id, Outcome::Cancelled);
    }

    pub fn launch_exited(&mut self, id: String, status: ExitStatus) {
        let Some(l) = self.launches.get_mut(&id) else { return };
        // Reaped, so the pid may be another process's by now; a 0 exit stays pending
        // (a single-instance hand-off, busctl), and a cancel still stops its scope.
        l.pid = None;
        let pending = l.outcome == Outcome::Pending;
        if pending && let Some(message) = launch::exit_message(status) {
            self.fail_launch(&id, "exec", message);
        }
    }

    /// Ends a pending launch: its token stops being valid.
    pub fn end_launch(&mut self, id: &str, outcome: Outcome) {
        let Some(l) = self.launches.get_mut(id) else { return };
        if !l.token.is_empty() {
            self.xdg_activation_state.remove_token(&XdgActivationToken::from(l.token.clone()));
        }
        l.outcome = outcome;
    }

    fn fail_launch(&mut self, id: &str, reason: &'static str, message: String) {
        info!("launch {id} failed ({reason}): {message}");
        self.end_launch(id, Outcome::Failed { reason, message });
        self.resend_launch(id);
    }

    /// Tells the app a launch's state, for a resent `launch` or its end.
    fn resend_launch(&mut self, id: &str) {
        let Some(l) = self.launches.get(id) else { return };
        let msg = match &l.outcome {
            Outcome::Pending => json!({"type": "launch-started", "id": id}),
            Outcome::Window(t) => match self.tree.of.get(t) {
                Some((window, _)) => json!({"type": "activate", "window": window, "launch": id}),
                None => json!({"type": "launch-failed", "id": id, "reason": "closed", "message": "its window closed"}),
            },
            Outcome::Failed { reason, message } => {
                json!({"type": "launch-failed", "id": id, "reason": reason, "message": message})
            }
            Outcome::Cancelled => return,
        };
        self.send_json(msg);
    }

    /// The `move` message: the window's new tab, echoed in its `toplevel`.
    pub fn on_move(&mut self, m: MoveMsg) {
        if self.toplevel_by_id(m.id).is_none() || self.is_dialog(m.id) {
            return;
        }
        let tab = if launch::valid_id(&m.tab) { m.tab } else { String::new() };
        self.meta.entry(m.id).or_default().tab = tab;
        if let Some(conn) = self.gui.active_conn_mut()
            && let Some(sent) = conn.toplevels.get_mut(&m.id)
        {
            sent.1 = serde_json::Value::Null;
        }
    }
}

impl XdgActivationHandler for State {
    fn activation_state(&mut self) -> &mut XdgActivationState {
        &mut self.xdg_activation_state
    }

    /// A client's own token counts only when minted with a recent input
    /// event's serial that went to it.
    fn token_created(&mut self, _token: XdgActivationToken, data: XdgActivationTokenData) -> bool {
        self.xdg_activation_state
            .retain_tokens(|_, d| d.client_id.is_none() || d.timestamp.elapsed() < CLIENT_TOKEN_AGE);
        match (&data.serial, &data.client_id) {
            (Some((serial, _)), Some(client)) => self.input_serials.recent(*serial, client),
            _ => false,
        }
    }

    fn request_activation(&mut self, token: XdgActivationToken, data: XdgActivationTokenData, surface: WlSurface) {
        if self.toplevel(&surface).is_none() {
            return;
        }
        let Some(id) = surface_id(&surface) else { return };
        let launch = self
            .launches
            .values()
            .find(|l| l.outcome == Outcome::Pending && l.token == token.as_str())
            .map(|l| l.id.clone());
        if launch.is_none() {
            self.xdg_activation_state.remove_token(&token);
            if data.client_id.is_none() || data.timestamp.elapsed() >= CLIENT_TOKEN_AGE {
                return;
            }
        }
        debug!("activation of {id} with launch {launch:?}");
        let Some((window, _)) = self.tree.of.get(&id).copied() else {
            // Not mapped yet: it binds to the launch when it maps.
            if let Some(launch) = launch {
                self.activations.insert(id, launch);
            }
            return;
        };
        if let Some(launch) = &launch {
            self.end_launch(launch, Outcome::Window(id));
        }
        if window != id {
            self.pressed_on(window, id);
        }
        self.send_json(json!({"type": "activate", "window": window, "launch": launch.unwrap_or_default()}));
    }
}
