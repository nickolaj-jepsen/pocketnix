//! A stand-in for guest RAM and the agent's frame buffers in it
//! (gui-protocol.md's Guest RAM), for an app without a VM: it answers
//! `frame-buffers` requests on a socket of its own with a memfd, makes up a
//! guest-physical address for each page, and reads PAGES commits back
//! through those addresses.

use std::io::BufRead;
use std::io::BufReader;
use std::io::Write;
use std::os::fd::AsRawFd;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::net::UnixListener;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::Arc;
use std::sync::Mutex;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;
use serde_json::Value;
use serde_json::json;

const PAGE: usize = 4096;
/// Where the made-up addresses start: AVF's guest RAM base, as on a phone.
const BASE: u64 = 0x8000_0000;

/// One frame-buffers set, mapped here for as long as its requester holds it.
struct Region {
    addr: u64,
    map: *mut u8,
    len: usize,
}

// SAFETY: the map belongs to the region and is only reached under GuestRam's lock.
unsafe impl Send for Region {}

impl Drop for Region {
    fn drop(&mut self) {
        // SAFETY: unmaps the mapping made for this region, which nothing borrows past the lock.
        unsafe { libc::munmap(self.map.cast(), self.len) };
    }
}

#[derive(Default)]
struct Ram {
    regions: Vec<Region>,
    /// Addresses are never reused, so a stale page list can't read a newer set.
    next: u64,
}

#[derive(Clone)]
pub struct GuestRam(Arc<Mutex<Ram>>);

impl GuestRam {
    /// Serves frame-buffers requests on a new socket at `socket`, on a thread
    /// of its own.
    pub fn serve(socket: &Path) -> Result<Self> {
        let _ = std::fs::remove_file(socket);
        let listener = UnixListener::bind(socket).with_context(|| format!("binding {}", socket.display()))?;
        let ram = Self(Arc::new(Mutex::new(Ram { regions: Vec::new(), next: BASE })));
        let r = ram.clone();
        std::thread::spawn(move || {
            for stream in listener.incoming().flatten() {
                let r = r.clone();
                std::thread::spawn(move || {
                    if let Err(e) = r.answer(stream) {
                        eprintln!("guest RAM stand-in: {e:#}");
                    }
                });
            }
        });
        Ok(ram)
    }

    /// Answers one request, and holds its set until the requester closes.
    fn answer(&self, stream: UnixStream) -> Result<()> {
        let mut reader = BufReader::new(stream.try_clone()?);
        let mut line = String::new();
        reader.read_line(&mut line)?;
        let req: Value = serde_json::from_str(&line).context("the request")?;
        if req["type"] != "frame-buffers" {
            return reply(&stream, &json!({"error": format!("the stand-in serves only frame-buffers, not {}", req["type"])}), None);
        }
        let len = (req["bytes"].as_u64().context("the request's bytes")? as usize).div_ceil(PAGE) * PAGE;
        if len == 0 {
            return reply(&stream, &json!({"error": "frame buffers of 0 bytes"}), None);
        }
        // SAFETY: a fresh memfd, sized here; this process owns the descriptor.
        let fd = unsafe {
            let fd = libc::memfd_create(c"pocketnix-frames".as_ptr(), libc::MFD_CLOEXEC);
            if fd < 0 {
                bail!("memfd: {}", std::io::Error::last_os_error());
            }
            OwnedFd::from_raw_fd(fd)
        };
        // SAFETY: sizes the memfd just made.
        if unsafe { libc::ftruncate(fd.as_raw_fd(), len as libc::off_t) } != 0 {
            bail!("sizing the memfd: {}", std::io::Error::last_os_error());
        }
        // SAFETY: maps the memfd sized to len; the region owns the map.
        let map = unsafe { libc::mmap(std::ptr::null_mut(), len, libc::PROT_READ, libc::MAP_SHARED, fd.as_raw_fd(), 0) };
        if map == libc::MAP_FAILED {
            bail!("mapping the memfd: {}", std::io::Error::last_os_error());
        }
        let addr = {
            let mut ram = self.0.lock().unwrap();
            let addr = ram.next;
            ram.next += len as u64;
            ram.regions.push(Region { addr, map: map.cast(), len });
            addr
        };
        let pages: Vec<u64> = (0..len / PAGE).map(|i| addr + (i * PAGE) as u64).collect();
        let sent = reply(&stream, &json!({"pages": pages}), Some(&fd));
        drop(fd);
        if sent.is_ok() {
            // The requester never writes after asking: EOF is its close.
            let _ = reader.fill_buf();
        }
        self.0.lock().unwrap().regions.retain(|r| r.addr != addr);
        sent
    }

    /// Copies the first `len` bytes of the pages a PAGES commit's data
    /// lists into `out`; fails for an address outside every set.
    pub fn gather(&self, list: &[u8], len: usize, out: &mut Vec<u8>) -> Result<()> {
        let count = u32::from_be_bytes(list.get(..4).context("a page list without a count")?.try_into()?) as usize;
        if list.len() != 4 + 8 * count || count != len.div_ceil(PAGE) {
            bail!("a list of {count} pages in {} bytes for {len} bytes of data", list.len());
        }
        out.clear();
        out.reserve(len);
        let ram = self.0.lock().unwrap();
        for i in 0..count {
            let addr = u64::from_be_bytes(list[4 + 8 * i..12 + 8 * i].try_into()?);
            let r = ram
                .regions
                .iter()
                .find(|r| addr >= r.addr && addr + PAGE as u64 <= r.addr + r.len as u64)
                .with_context(|| format!("page {addr:#x} is in no frame buffer set"))?;
            let n = PAGE.min(len - i * PAGE);
            // SAFETY: the page lies inside the region's map, which the lock keeps mapped.
            out.extend_from_slice(unsafe { std::slice::from_raw_parts(r.map.add((addr - r.addr) as usize), n) });
        }
        Ok(())
    }
}

/// Writes `v` as one JSON line, carrying `fd` (`SCM_RIGHTS`) when given.
fn reply(stream: &UnixStream, v: &Value, fd: Option<&OwnedFd>) -> Result<()> {
    let line = format!("{v}\n").into_bytes();
    let Some(fd) = fd else {
        return Ok((&*stream).write_all(&line)?);
    };
    let mut iov = libc::iovec { iov_base: line.as_ptr() as *mut _, iov_len: line.len() };
    let mut control = [0u64; 4];
    // SAFETY: an all-zero msghdr is a valid empty one; the CMSG_* macros fill
    // in one SCM_RIGHTS header with a descriptor this process owns.
    let sent = unsafe {
        let mut msg: libc::msghdr = std::mem::zeroed();
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = libc::CMSG_SPACE(4) as _;
        let c = libc::CMSG_FIRSTHDR(&msg);
        (*c).cmsg_level = libc::SOL_SOCKET;
        (*c).cmsg_type = libc::SCM_RIGHTS;
        (*c).cmsg_len = libc::CMSG_LEN(4) as _;
        libc::CMSG_DATA(c).cast::<libc::c_int>().write_unaligned(fd.as_raw_fd());
        libc::sendmsg(stream.as_raw_fd(), &msg, 0)
    };
    if sent < 0 {
        bail!("sending the frame buffers: {}", std::io::Error::last_os_error());
    }
    Ok((&*stream).write_all(&line[sent as usize..])?)
}
