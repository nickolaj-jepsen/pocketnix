//! Snapshots taken at both ends of a measured window, and their difference:
//! stats sockets and the guest's /proc.

use std::io::Read;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::path::PathBuf;
use std::time::Duration;

use anyhow::Context;
use anyhow::Result;
use serde_json::Map;
use serde_json::Value;
use serde_json::json;

/// The processes whose CPU time a run reports, by `/proc/<pid>/comm`
/// (which the kernel cuts to 15 bytes).
const WATCHED: &[(&str, &str)] = &[("compositor", "pocketnix-compo"), ("agent", "pocketnix-agent")];

pub fn compositor_socket() -> Result<PathBuf> {
    let dir = std::env::var_os("XDG_RUNTIME_DIR")
        .map(PathBuf::from)
        // SAFETY: getuid can't fail.
        .unwrap_or_else(|| PathBuf::from(format!("/run/user/{}", unsafe { libc::getuid() })));
    Ok(dir.join("pocketnix-compositor-stats.sock"))
}

pub fn read_socket(path: &Path) -> Result<Value> {
    let mut stream = UnixStream::connect(path).with_context(|| format!("connecting to {}", path.display()))?;
    stream.set_read_timeout(Some(Duration::from_secs(5)))?;
    let mut line = String::new();
    stream.read_to_string(&mut line).with_context(|| format!("reading {}", path.display()))?;
    serde_json::from_str(line.trim()).with_context(|| format!("parsing {}'s snapshot", path.display()))
}

/// `after` less `before`: counters and histograms subtract (a counter
/// missing before counts from 0); everything else is `after`'s.
pub fn diff(before: &Value, after: &Value) -> Value {
    let mut out = after.clone();
    if let Some(counters) = out.get_mut("counters").and_then(Value::as_object_mut) {
        for (k, v) in counters.iter_mut() {
            let was = before["counters"][k].as_u64().unwrap_or(0);
            *v = json!(v.as_u64().unwrap_or(0).saturating_sub(was));
        }
    }
    if let Some(histograms) = out.get_mut("histograms").and_then(Value::as_object_mut) {
        for (k, h) in histograms.iter_mut() {
            *h = diff_histogram(&before["histograms"][k], h);
        }
    }
    out
}

fn diff_histogram(before: &Value, after: &Value) -> Value {
    let sub = |f: &str| after[f].as_u64().unwrap_or(0).saturating_sub(before[f].as_u64().unwrap_or(0));
    let was = |bound: u64| {
        before["buckets"]
            .as_array()
            .and_then(|b| b.iter().find(|e| e[0].as_u64() == Some(bound)))
            .and_then(|e| e[1].as_u64())
            .unwrap_or(0)
    };
    let buckets: Vec<Value> = after["buckets"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|e| {
            let bound = e[0].as_u64()?;
            let n = e[1].as_u64()?.saturating_sub(was(bound));
            (n > 0).then(|| json!([bound, n]))
        })
        .collect();
    json!({"count": sub("count"), "sumUs": sub("sumUs"), "buckets": buckets})
}

/// The bound of the bucket holding quantile `q` of a histogram's JSON.
pub fn quantile_us(h: &Value, q: f64) -> Option<u64> {
    let count = h["count"].as_u64()?;
    if count == 0 {
        return None;
    }
    let target = (count as f64 * q).ceil().max(1.0) as u64;
    let mut seen = 0;
    for e in h["buckets"].as_array()? {
        seen += e[1].as_u64()?;
        if seen >= target {
            return e[0].as_u64();
        }
    }
    None
}

/// The guest's CPU counters at one moment.
pub struct Proc {
    /// Busy and idle jiffies over every CPU (`/proc/stat`'s first line).
    busy: u64,
    idle: u64,
    /// utime + stime jiffies of each watched process, summed over its pids.
    watched: Vec<u64>,
}

pub fn proc_sample() -> Proc {
    let (busy, idle) = std::fs::read_to_string("/proc/stat")
        .ok()
        .and_then(|s| {
            let f: Vec<u64> = s.lines().next()?.split_whitespace().skip(1).filter_map(|v| v.parse().ok()).collect();
            // user nice system idle iowait irq softirq steal
            let get = |i: usize| f.get(i).copied().unwrap_or(0);
            Some((get(0) + get(1) + get(2) + get(5) + get(6) + get(7), get(3) + get(4)))
        })
        .unwrap_or((0, 0));
    let mut watched = vec![0; WATCHED.len()];
    for entry in std::fs::read_dir("/proc").into_iter().flatten().flatten() {
        let path = entry.path();
        let Ok(comm) = std::fs::read_to_string(path.join("comm")) else { continue };
        let Some(i) = WATCHED.iter().position(|(_, c)| *c == comm.trim_end()) else { continue };
        watched[i] += proc_ticks(&path.join("stat")).unwrap_or(0);
    }
    Proc { busy, idle, watched }
}

fn proc_ticks(stat: &Path) -> Option<u64> {
    let s = std::fs::read_to_string(stat).ok()?;
    // Fields after the parenthesised comm, which may hold spaces.
    let rest = &s[s.rfind(')')? + 2..];
    let f: Vec<&str> = rest.split_whitespace().collect();
    Some(f.get(11)?.parse::<u64>().ok()? + f.get(12)?.parse::<u64>().ok()?)
}

/// The guest's CPU use between two samples taken `secs` apart: each watched
/// process as % of one CPU, and the whole guest as % of all of them.
pub fn proc_diff(a: &Proc, b: &Proc, secs: f64) -> Value {
    // SAFETY: sysconf has no preconditions.
    let hz = unsafe { libc::sysconf(libc::_SC_CLK_TCK) }.max(1) as f64;
    let cpus = std::thread::available_parallelism().map_or(1, |n| n.get());
    let busy = b.busy.saturating_sub(a.busy) as f64;
    let idle = b.idle.saturating_sub(a.idle) as f64;
    let mut procs = Map::new();
    for (i, (name, _)) in WATCHED.iter().enumerate() {
        let ticks = b.watched[i].saturating_sub(a.watched[i]) as f64;
        procs.insert((*name).into(), json!(round1(ticks / hz / secs * 100.0)));
    }
    json!({
        "cpus": cpus,
        "busyPctOfAll": round1(if busy + idle > 0.0 { busy / (busy + idle) * 100.0 } else { 0.0 }),
        "procPctOfOne": procs,
    })
}

pub fn round1(v: f64) -> f64 {
    (v * 10.0).round() / 10.0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn diff_subtracts_counters_and_buckets() {
        let before = json!({"counters": {"a": 3}, "histograms": {"h": {"count": 2, "sumUs": 10, "buckets": [[4, 2]]}}});
        let after = json!({"counters": {"a": 5, "b": 1}, "histograms": {"h": {"count": 5, "sumUs": 40, "buckets": [[4, 3], [8, 2]]}}, "windows": [1]});
        let d = diff(&before, &after);
        assert_eq!(d["counters"], json!({"a": 2, "b": 1}));
        assert_eq!(d["histograms"]["h"], json!({"count": 3, "sumUs": 30, "buckets": [[4, 1], [8, 2]]}));
        assert_eq!(d["windows"], json!([1]));
    }

    #[test]
    fn quantiles_come_from_bucket_bounds() {
        let h = json!({"count": 10, "sumUs": 0, "buckets": [[4, 5], [8, 4], [16, 1]]});
        assert_eq!(quantile_us(&h, 0.5), Some(4));
        assert_eq!(quantile_us(&h, 0.9), Some(8));
        assert_eq!(quantile_us(&h, 0.99), Some(16));
    }
}
