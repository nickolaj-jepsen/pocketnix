//! pocketnix-gui-bench: benchmarks for the GUI pipeline. `client` is a
//! Wayland client that draws synthetic content as fast as frame callbacks
//! let it (or at a fixed rate) and reports what it got; `sink` stands in for
//! the app on a GUI socket; `stats` prints a stats socket's snapshot.

mod client;
mod sink;
mod snapshot;

#[allow(dead_code)]
#[path = "../protocol.rs"]
mod protocol;
#[allow(dead_code)]
#[path = "../stats.rs"]
mod stats;

use std::path::PathBuf;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;

const USAGE: &str = "usage: pocketnix-gui-bench <command> [flags]

commands:
  client [--mode noise|gradient|scroll|rect|idle|touch] [--duration SECS] [--warmup SECS]
         [--rate HZ] [--res FACTOR] [--scroll PX] [--rect PX] [--fullscreen]
         [--stats NAME=SOCKET]... [--no-default-stats] [--timeout SECS]
      Opens one window and draws into it: a new frame on every frame callback
      (--rate 0, the default) or at most HZ a second. Prints one JSON object
      on stdout: frame rate, latencies, CPU and every stats socket's change
      over the measured window. Progress goes to stderr, with MEASURE-START
      and MEASURE-END lines around the measured window.
        noise     every frame a whole buffer of noise (incompressible)
        gradient  every frame a whole buffer of a shifted gradient
        scroll    text-like rows scrolling --scroll PX a frame, damaging the
                  whole buffer (as terminals do)
        rect      a --rect PX square changing colour, damaging only it
        idle      one frame, then nothing
        touch     a new colour on each touch or button press: the input's
                  latency to its frame callback
      --res scales the buffer against the window's pixels; a viewport shows
      it at the window's size.
      --stats adds a stats socket to snapshot (the compositor's,
      $XDG_RUNTIME_DIR/pocketnix-compositor-stats.sock, is the default).
  sink --socket PATH [--width PX] [--height PX] [--scale F] [--refresh MHZ]
       [--stats-socket PATH] [--no-decode]
      Speaks the app's side of a GUI socket: opens each window at the
      output's size, decodes every commit and acknowledges each group at
      the next tick of a --refresh clock.
  stats [--socket PATH]
      Prints one snapshot of a stats socket (default the compositor's).
";

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let result = match args.first().map(String::as_str) {
        Some("client") => client::run(&args[1..]),
        Some("sink") => sink::run(&args[1..]),
        Some("stats") => stats_cmd(&args[1..]),
        Some("--help" | "-h") => {
            print!("{USAGE}");
            Ok(())
        }
        _ => {
            eprint!("{USAGE}");
            std::process::exit(2);
        }
    };
    if let Err(e) = result {
        eprintln!("pocketnix-gui-bench: {e:#}");
        std::process::exit(1);
    }
}

fn stats_cmd(args: &[String]) -> Result<()> {
    let mut socket = None;
    let mut it = args.iter();
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--socket" => socket = Some(PathBuf::from(value(&mut it, arg)?)),
            _ => bail!("unknown argument {arg:?}\n{USAGE}"),
        }
    }
    let socket = match socket {
        Some(s) => s,
        None => snapshot::compositor_socket()?,
    };
    println!("{}", snapshot::read_socket(&socket)?);
    Ok(())
}

/// The value after flag `name`.
fn value<'a>(it: &mut impl Iterator<Item = &'a String>, name: &str) -> Result<&'a str> {
    it.next().map(String::as_str).with_context(|| format!("{name} needs a value\n{USAGE}"))
}

fn parsed<T: std::str::FromStr>(it: &mut std::slice::Iter<'_, String>, name: &str) -> Result<T>
where
    T::Err: std::fmt::Display,
{
    let v = value(it, name)?;
    v.parse().map_err(|e| anyhow::anyhow!("{name} {v:?}: {e}"))
}
