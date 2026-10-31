//! The benchmark tools against a real compositor: the client's frames reach
//! the sink, and both stats sockets account for them.

use std::path::Path;
use std::path::PathBuf;
use std::process::Child;
use std::process::Command;
use std::time::Duration;
use std::time::Instant;

use serde_json::Value;

struct Killed(Child);

impl Drop for Killed {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn wait_for(path: &Path) {
    let deadline = Instant::now() + Duration::from_secs(30);
    while !path.exists() {
        assert!(Instant::now() < deadline, "{} never appeared", path.display());
        std::thread::sleep(Duration::from_millis(20));
    }
}

fn dir(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("pocketnix-bench-test-{}-{name}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Runs the client's `mode` against a compositor and a sink, the sink
/// standing in for guest RAM with `guest_ram`, and returns its report.
fn run(mode: &str, guest_ram: bool) -> Value {
    let dir = dir(&format!("{mode}-{guest_ram}"));
    let bench = env!("CARGO_BIN_EXE_pocketnix-gui-bench");
    let mut compositor = Command::new(env!("CARGO_BIN_EXE_pocketnix-compositor"));
    compositor
        .args(["--wayland-display", "wl-bench"])
        .arg("--gui-socket")
        .arg(dir.join("gui.sock"))
        .arg("--stats-socket")
        .arg(dir.join("compositor.sock"))
        .env("XDG_RUNTIME_DIR", &dir);
    let mut sink = Command::new(bench);
    sink.args(["sink", "--width", "400", "--height", "300", "--scale", "2", "--refresh", "60000"])
        .arg("--socket")
        .arg(dir.join("gui.sock"))
        .arg("--stats-socket")
        .arg(dir.join("sink.sock"));
    if guest_ram {
        compositor.arg("--guest-socket").arg(dir.join("guest.sock"));
        sink.arg("--guest-ram").arg(dir.join("guest.sock"));
    }
    let _compositor = Killed(compositor.spawn().unwrap());
    wait_for(&dir.join("gui.sock"));
    wait_for(&dir.join("wl-bench"));
    let _sink = Killed(sink.spawn().unwrap());
    wait_for(&dir.join("sink.sock"));

    let out = Command::new(bench)
        .args(["client", "--mode", mode, "--duration", "1", "--warmup", "0.2", "--timeout", "20", "--no-default-stats"])
        .arg("--stats")
        .arg(format!("compositor={}", dir.join("compositor.sock").display()))
        .arg("--stats")
        .arg(format!("sink={}", dir.join("sink.sock").display()))
        .env("XDG_RUNTIME_DIR", &dir)
        .env("WAYLAND_DISPLAY", "wl-bench")
        .output()
        .unwrap();
    assert!(out.status.success(), "client: {}", String::from_utf8_lossy(&out.stderr));
    let _ = std::fs::remove_dir_all(&dir);
    serde_json::from_slice(&out.stdout).unwrap()
}

#[test]
fn a_client_run_reports_its_frames_and_the_stages_behind_them() {
    let r = run("rect", false);
    // The sink's 60 Hz acks pace the client.
    assert!(r["presented"].as_u64().unwrap() > 10, "{r}");
    assert_eq!(r["window"]["bufferWidth"], 400);
    let compositor = &r["sources"]["compositor"];
    assert!(compositor["counters"]["commitsDamage"].as_u64().unwrap() > 0, "{compositor}");
    assert!(compositor["histograms"]["sentToPresented"]["count"].as_u64().unwrap() > 0, "{compositor}");
    assert!(r["sources"]["sink"]["counters"]["groups"].as_u64().unwrap() > 0, "{r}");
}

#[test]
fn with_guest_ram_whole_frames_reach_the_sink_as_pages() {
    let r = run("noise", true);
    assert!(r["presented"].as_u64().unwrap() > 10, "{r}");
    let pages = r["sources"]["sink"]["counters"]["commitsPages"].as_u64().unwrap_or(0);
    assert!(pages > 10, "{r}");
}
