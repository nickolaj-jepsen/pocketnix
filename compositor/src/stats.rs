//! Counters and latency histograms for benchmarks, served as one JSON line
//! to each connection on the stats socket. Every number only grows, so a
//! reader takes two snapshots and subtracts them.

use std::collections::HashMap;
use std::collections::VecDeque;
use std::time::Duration;
use std::time::Instant;

use serde_json::Value;
use serde_json::json;

/// Buckets per power of two; a bucket's bound is 2^((i + 1) / 4) µs, ~19% wide.
/// The bench client and the app's `GuiStats` use the same scheme.
const PER_OCTAVE: f64 = 4.0;
const BUCKETS: usize = 112;

/// Seqs a window keeps waiting for PRESENTED: a hidden window is never
/// presented, and its oldest sends are forgotten.
const MAX_IN_FLIGHT: usize = 256;

#[derive(Clone)]
pub struct Histogram {
    buckets: [u64; BUCKETS],
    count: u64,
    sum_us: u64,
}

impl Default for Histogram {
    fn default() -> Self {
        Self { buckets: [0; BUCKETS], count: 0, sum_us: 0 }
    }
}

impl Histogram {
    pub fn record(&mut self, d: Duration) {
        let us = d.as_micros().min(u64::MAX as u128) as u64;
        let i = if us <= 1 { 0 } else { ((us as f64).log2() * PER_OCTAVE) as usize };
        self.buckets[i.min(BUCKETS - 1)] += 1;
        self.count += 1;
        self.sum_us += us;
    }

    /// `{"count", "sumUs", "buckets": [[bound µs, count], …]}`, empty buckets left out.
    pub fn to_json(&self) -> Value {
        let buckets: Vec<Value> = self
            .buckets
            .iter()
            .enumerate()
            .filter(|(_, n)| **n > 0)
            .map(|(i, n)| json!([bound_us(i), n]))
            .collect();
        json!({"count": self.count, "sumUs": self.sum_us, "buckets": buckets})
    }
}

fn bound_us(i: usize) -> u64 {
    2f64.powf((i + 1) as f64 / PER_OCTAVE).ceil() as u64
}

/// What the compositor did since it started.
#[derive(Default)]
pub struct Stats {
    counters: HashMap<&'static str, u64>,
    histograms: HashMap<&'static str, Histogram>,
    /// Per window, the groups sent and not yet presented: (last seq, when).
    in_flight: HashMap<u32, VecDeque<(u32, Instant)>>,
    /// Compression time per CPU, served as `compressCpu<n>Ns` counters.
    compress_cpu_ns: Vec<u64>,
}

impl Stats {
    pub fn add(&mut self, name: &'static str, n: u64) {
        *self.counters.entry(name).or_default() += n;
    }

    pub fn time(&mut self, name: &'static str, d: Duration) {
        self.add(name, d.as_nanos() as u64);
    }

    pub fn record(&mut self, name: &'static str, d: Duration) {
        self.histograms.entry(name).or_default().record(d);
    }

    /// One compression's shards: the slowest sets its time, the rest wait.
    pub fn shards(&mut self, shards: &[crate::protocol::Shard]) {
        let Some(slowest) = shards.iter().map(|s| s.time).max() else { return };
        self.time("compressShardMaxNs", slowest);
        self.time("compressShardNs", shards.iter().map(|s| s.time).sum());
        self.add("compressShards", shards.len() as u64);
        for s in shards {
            let Some(cpu) = s.cpu else { continue };
            if self.compress_cpu_ns.len() <= cpu {
                self.compress_cpu_ns.resize(cpu + 1, 0);
            }
            self.compress_cpu_ns[cpu] += s.time.as_nanos() as u64;
        }
    }

    /// A group for `window` ending at `seq` went to the writer.
    pub fn sent(&mut self, window: u32, seq: u32) {
        let q = self.in_flight.entry(window).or_default();
        if q.len() == MAX_IN_FLIGHT {
            q.pop_front();
        }
        q.push_back((seq, Instant::now()));
    }

    /// The app showed `window`'s groups up to `seq`.
    pub fn presented(&mut self, window: u32, seq: u32) {
        self.add("presented", 1);
        let now = Instant::now();
        let Some(q) = self.in_flight.get_mut(&window) else { return };
        while let Some(&(s, at)) = q.front() {
            // Seqs wrap only after 2^32 commits; compare them as a window would.
            if seq.wrapping_sub(s) > u32::MAX / 2 {
                break;
            }
            q.pop_front();
            self.histograms.entry("sentToPresented").or_default().record(now - at);
            *self.counters.entry("groupsPresented").or_default() += 1;
        }
    }

    /// Seqs restart with each connection.
    pub fn connection_changed(&mut self) {
        self.in_flight.clear();
    }

    /// The snapshot `source` serves: the counters with this process's CPU
    /// time, the histograms and `windows` as given.
    pub fn to_json(&self, source: &str, windows: Value) -> Value {
        let mut counters: serde_json::Map<String, Value> =
            self.counters.iter().map(|(k, v)| ((*k).to_owned(), json!(v))).collect();
        let (user, system) = cpu_time();
        counters.insert("cpuUserNs".into(), json!(user));
        counters.insert("cpuSystemNs".into(), json!(system));
        for (cpu, ns) in self.compress_cpu_ns.iter().enumerate().filter(|(_, ns)| **ns > 0) {
            counters.insert(format!("compressCpu{cpu}Ns"), json!(ns));
        }
        let histograms: serde_json::Map<String, Value> =
            self.histograms.iter().map(|(k, v)| ((*k).to_owned(), v.to_json())).collect();
        json!({
            "source": source,
            "pid": std::process::id(),
            "counters": counters,
            "histograms": histograms,
            "windows": windows,
        })
    }
}

/// The process's CPU time so far, user and system, in ns: every thread's,
/// the writer's and the compression shards' included.
fn cpu_time() -> (u64, u64) {
    // SAFETY: getrusage fills the struct it's given.
    let mut ru: libc::rusage = unsafe { std::mem::zeroed() };
    if unsafe { libc::getrusage(libc::RUSAGE_SELF, &mut ru) } != 0 {
        return (0, 0);
    }
    let ns = |t: libc::timeval| t.tv_sec as u64 * 1_000_000_000 + t.tv_usec as u64 * 1000;
    (ns(ru.ru_utime), ns(ru.ru_stime))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_bucket_bounds_the_values_in_it() {
        let mut h = Histogram::default();
        h.record(Duration::from_micros(1000));
        let json = h.to_json();
        let bound = json["buckets"][0][0].as_u64().unwrap();
        assert!((1000..1200).contains(&bound), "bound {bound}");
        assert_eq!(json["count"], 1);
        assert_eq!(json["sumUs"], 1000);
    }

    #[test]
    fn shards_count_the_slowest_and_each_cpu() {
        use crate::protocol::Shard;
        let mut s = Stats::default();
        s.shards(&[
            Shard { time: Duration::from_millis(3), cpu: Some(2) },
            Shard { time: Duration::from_millis(5), cpu: Some(7) },
            Shard { time: Duration::from_millis(1), cpu: None },
        ]);
        let c = &s.to_json("test", json!([]))["counters"];
        assert_eq!(c["compressShardMaxNs"], 5_000_000);
        assert_eq!(c["compressShardNs"], 9_000_000);
        assert_eq!(c["compressShards"], 3);
        assert_eq!(c["compressCpu7Ns"], 5_000_000);
        assert!(c.get("compressCpu0Ns").is_none());
    }

    #[test]
    fn presented_times_every_group_up_to_its_seq() {
        let mut s = Stats::default();
        s.sent(7, 1);
        s.sent(7, 2);
        s.sent(7, 3);
        s.presented(7, 2);
        let json = s.to_json("test", json!([]));
        assert_eq!(json["histograms"]["sentToPresented"]["count"], 2);
        assert_eq!(json["counters"]["presented"], 1);
        s.presented(7, 3);
        assert_eq!(s.to_json("test", json!([]))["histograms"]["sentToPresented"]["count"], 3);
    }
}
