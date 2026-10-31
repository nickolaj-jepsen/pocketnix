//! Frame buffers for an app that maps guest RAM (gui-protocol.md's PAGES):
//! the agent pins them and says where each page is in guest RAM, so a
//! commit's data is a list of page addresses instead of the pixels. A commit
//! lends its slot to the app until the app RELEASEs it.

use std::io::Read;
use std::io::Write;
use std::os::fd::AsRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;
use std::time::Instant;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;

use crate::gui::read_with_fd;

/// The agent's guest socket: go/internal/protocol's GuestSocket.
pub const GUEST_SOCKET: &str = "/run/pocketnix/guest.sock";
const PAGE: usize = 4096;
/// How long the agent may take to pin a buffer set.
const AGENT_TIMEOUT: Duration = Duration::from_secs(5);
/// The first wait before asking again after a failure, such as the agent
/// restarting or refusing for now; it doubles up to [RETRY_MAX].
const RETRY_FIRST: Duration = Duration::from_secs(1);
const RETRY_MAX: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, PartialEq, Debug)]
enum Slot {
    Free,
    /// Written for a commit not yet sent.
    Filled,
    /// The app may still read it: commit seq hasn't been released.
    Lent(u32),
}

/// One pinned buffer set, held as long as its agent connection.
struct Buffers {
    id: u64,
    agent: UnixStream,
    map: *mut u8,
    len: usize,
    /// Each page's guest-physical address.
    pages: Vec<u64>,
    slot_len: usize,
    slots: Vec<Slot>,
}

// SAFETY: the map belongs to Buffers alone and is only reached through it.
unsafe impl Send for Buffers {}

impl Buffers {
    /// Asks for `slots` slots of at least `slot_len` bytes. Their total must
    /// stay within the agent's maxFrameBuffers (go/internal/agent), which
    /// refuses a bigger set.
    fn request(socket: &Path, id: u64, slot_len: usize, slots: usize) -> Result<Self> {
        let slot_len = slot_len.div_ceil(PAGE) * PAGE;
        let len = slot_len * slots;
        let mut agent = UnixStream::connect(socket).with_context(|| format!("connecting to {}", socket.display()))?;
        agent.set_read_timeout(Some(AGENT_TIMEOUT))?;
        writeln!(agent, "{}", serde_json::json!({"type": "frame-buffers", "bytes": len}))?;
        let (line, fd) = read_reply(&mut agent)?;
        let reply: serde_json::Value = serde_json::from_slice(&line).context("the agent's reply")?;
        if let Some(e) = reply.get("error").and_then(|e| e.as_str()) {
            bail!("the agent: {e}");
        }
        let fd = fd.context("the agent's reply carries no memfd")?;
        let pages: Vec<u64> = serde_json::from_value(reply.get("pages").cloned().unwrap_or_default()).context("the agent's pages")?;
        if pages.len() != len / PAGE {
            bail!("the agent gave {} pages for {len} bytes", pages.len());
        }
        // SAFETY: maps the memfd the agent sized to len; the map lives as long as Buffers.
        let map = unsafe { libc::mmap(std::ptr::null_mut(), len, libc::PROT_READ | libc::PROT_WRITE, libc::MAP_SHARED, fd.as_raw_fd(), 0) };
        if map == libc::MAP_FAILED {
            bail!("mapping the frame buffers: {}", std::io::Error::last_os_error());
        }
        agent.set_read_timeout(None)?;
        agent.set_nonblocking(true)?;
        Ok(Self { id, agent, map: map.cast(), len, pages, slot_len, slots: vec![Slot::Free; slots] })
    }

    /// Whether the agent still holds the pin: its end of the connection is open.
    fn alive(&self) -> bool {
        let mut pfd = libc::pollfd { fd: self.agent.as_raw_fd(), events: libc::POLLIN, revents: 0 };
        // SAFETY: polls one descriptor this struct owns, without waiting.
        let n = unsafe { libc::poll(&mut pfd, 1, 0) };
        // The agent never writes after its reply: readable means closed.
        n == 0
    }

    fn lent(&self) -> bool {
        self.slots.iter().any(|s| *s != Slot::Free)
    }
}

impl Drop for Buffers {
    fn drop(&mut self) {
        // SAFETY: unmaps the mapping made in request, which nothing borrows past self.
        unsafe { libc::munmap(self.map.cast(), self.len) };
    }
}

/// Reads the agent's one-line reply and the descriptor that comes with it.
fn read_reply(agent: &mut UnixStream) -> Result<(Vec<u8>, Option<OwnedFd>)> {
    let mut buf = vec![0u8; 64 << 10];
    let (n, fd) = read_with_fd(agent, &mut buf).context("reading the agent's reply")?;
    let mut line = buf[..n].to_vec();
    while !line.ends_with(b"\n") {
        let n = agent.read(&mut buf).context("reading the agent's reply")?;
        if n == 0 {
            bail!("the agent closed before replying");
        }
        line.extend_from_slice(&buf[..n]);
    }
    line.pop();
    Ok((line, fd))
}

/// Where a commit's data went: which buffer set and slot.
#[derive(Clone, Copy, Debug)]
pub struct Loan {
    buffers: u64,
    slot: usize,
}

/// A connection's frame buffers.
pub struct Frames {
    /// The agent's guest socket, or a stand-in's.
    socket: Arc<Path>,
    /// Asks for a set: [Buffers::request], or a local one in tests.
    request: fn(&Path, u64, usize, usize) -> Result<Buffers>,
    /// Slots per set.
    slots: usize,
    current: Option<Buffers>,
    /// Sets replaced by bigger ones while the app still had slots of them.
    retired: Vec<Buffers>,
    next_id: u64,
    /// After a failure, when to ask again, and the wait after that.
    retry_at: Option<Instant>,
    backoff: Duration,
}

impl Frames {
    /// Frame buffers asked of the agent on `socket`, for a connection that
    /// keeps up to `frames_in_flight` commits of a visible window unreleased:
    /// each set has a slot more.
    pub fn new(socket: Arc<Path>, frames_in_flight: usize) -> Self {
        Self { socket, request: Buffers::request, slots: frames_in_flight + 1, current: None, retired: Vec::new(), next_id: 0, retry_at: None, backoff: RETRY_FIRST }
    }

    /// Copies `data` into a free slot; returns the slot and the commit's data
    /// for it (page count, then addresses). None when there are no buffers or
    /// none free, or while asking for them waits out a failure: the data then
    /// goes on the stream.
    pub fn put(&mut self, data: &[u8]) -> Option<(Loan, Vec<u8>)> {
        if self.retry_at.is_some_and(|t| Instant::now() < t) {
            return None;
        }
        if !self.current.as_ref().is_some_and(|b| b.alive() && b.slot_len >= data.len()) {
            if let Some(old) = self.current.take().filter(|b| b.alive() && b.lent()) {
                self.retired.push(old);
            }
            self.next_id += 1;
            match (self.request)(&self.socket, self.next_id, data.len(), self.slots) {
                Ok(b) => {
                    self.current = Some(b);
                    self.retry_at = None;
                    self.backoff = RETRY_FIRST;
                }
                Err(e) => {
                    tracing::warn!("frame buffers: {e:#}; frames go on the stream, asking again in {:?}", self.backoff);
                    self.retry_at = Some(Instant::now() + self.backoff);
                    self.backoff = (self.backoff * 2).min(RETRY_MAX);
                    return None;
                }
            }
        }
        let b = self.current.as_mut()?;
        let slot = b.slots.iter().position(|s| *s == Slot::Free)?;
        // SAFETY: the slot is slot_len bytes inside the map, and data fits it.
        unsafe { std::ptr::copy_nonoverlapping(data.as_ptr(), b.map.add(slot * b.slot_len), data.len()) };
        b.slots[slot] = Slot::Filled;
        let first = slot * b.slot_len / PAGE;
        let pages = &b.pages[first..first + data.len().div_ceil(PAGE)];
        let mut out = Vec::with_capacity(4 + 8 * pages.len());
        out.extend_from_slice(&(pages.len() as u32).to_be_bytes());
        for p in pages {
            out.extend_from_slice(&p.to_be_bytes());
        }
        Some((Loan { buffers: b.id, slot }, out))
    }

    /// The commit that `loan`'s data went out with is `seq`.
    pub fn lend(&mut self, loan: Loan, seq: u32) {
        if let Some(b) = self.sets().find(|b| b.id == loan.buffers) {
            b.slots[loan.slot] = Slot::Lent(seq);
        }
    }

    /// The app has copied commit `seq`'s pages.
    pub fn release(&mut self, seq: u32) {
        for b in self.sets() {
            for s in &mut b.slots {
                if *s == Slot::Lent(seq) {
                    *s = Slot::Free;
                }
            }
        }
        self.retired.retain(|b| b.lent());
    }

    fn sets(&mut self) -> impl Iterator<Item = &mut Buffers> {
        self.current.iter_mut().chain(self.retired.iter_mut())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A set over a local memfd whose page n claims guest address n × PAGE, held by a socketpair.
    fn local(_: &Path, id: u64, slot_len: usize, slots: usize) -> Result<Buffers> {
        let slot_len = slot_len.div_ceil(PAGE) * PAGE;
        let len = slot_len * slots;
        let (agent, peer) = UnixStream::pair()?;
        std::mem::forget(peer);
        // SAFETY: a fresh memfd, sized and mapped here; the map lives as long as Buffers.
        let map = unsafe {
            let fd = libc::memfd_create(c"test".as_ptr(), libc::MFD_CLOEXEC);
            assert!(fd >= 0 && libc::ftruncate(fd, len as libc::off_t) == 0);
            let map = libc::mmap(std::ptr::null_mut(), len, libc::PROT_READ | libc::PROT_WRITE, libc::MAP_SHARED, fd, 0);
            libc::close(fd);
            map
        };
        let pages = (0..len / PAGE).map(|n| (n * PAGE) as u64).collect();
        Ok(Buffers { id, agent, map: map.cast(), len, pages, slot_len, slots: vec![Slot::Free; slots] })
    }

    fn unreachable(_: &Path, _: u64, _: usize, _: usize) -> Result<Buffers> {
        Err(std::io::Error::from(std::io::ErrorKind::ConnectionRefused).into())
    }

    /// An agent that answers with an error, as one does with all its guest connections taken.
    fn refusing(_: &Path, _: u64, _: usize, _: usize) -> Result<Buffers> {
        bail!("the agent: too many guest connections")
    }

    const IN_FLIGHT: usize = 3;

    fn frames() -> Frames {
        Frames { request: local, ..Frames::new(Path::new("").into(), IN_FLIGHT) }
    }

    fn addresses(data: &[u8]) -> Vec<u64> {
        let n = u32::from_be_bytes(data[..4].try_into().unwrap()) as usize;
        assert_eq!(data.len(), 4 + 8 * n);
        data[4..].chunks(8).map(|a| u64::from_be_bytes(a.try_into().unwrap())).collect()
    }

    #[test]
    fn data_goes_to_the_pages_of_a_free_slot() {
        let mut f = frames();
        let (loan, first) = f.put(&[1u8; PAGE + 1]).unwrap();
        f.lend(loan, 1);
        let (_, second) = f.put(&[2u8; PAGE + 1]).unwrap();
        assert_eq!(addresses(&first), vec![0, PAGE as u64]);
        // Slots are two pages long here, so the next one starts two pages in.
        assert_eq!(addresses(&second), vec![2 * PAGE as u64, 3 * PAGE as u64]);
    }

    #[test]
    fn lent_slots_wait_for_their_release() {
        let mut f = frames();
        for seq in 1..=4 {
            let (loan, _) = f.put(&[0u8; PAGE]).unwrap();
            f.lend(loan, seq);
        }
        assert!(f.put(&[0u8; PAGE]).is_none());
        f.release(2);
        let (_, data) = f.put(&[0u8; PAGE]).unwrap();
        assert_eq!(addresses(&data), vec![PAGE as u64]);
    }

    #[test]
    fn more_frames_in_flight_get_more_slots() {
        let mut f = Frames { request: local, ..Frames::new(Path::new("").into(), 5) };
        for seq in 1..=6 {
            let (loan, _) = f.put(&[0u8; PAGE]).unwrap();
            f.lend(loan, seq);
        }
        assert!(f.put(&[0u8; PAGE]).is_none());
    }

    #[test]
    fn a_bigger_frame_keeps_the_old_set_until_released() {
        let mut f = frames();
        let (loan, _) = f.put(&[0u8; PAGE]).unwrap();
        f.lend(loan, 1);
        let (loan, _) = f.put(&[0u8; 3 * PAGE]).unwrap();
        f.lend(loan, 2);
        assert_eq!(f.retired.len(), 1);
        f.release(1);
        assert!(f.retired.is_empty());
    }

    #[test]
    fn a_closed_agent_drops_the_set() {
        let mut f = frames();
        f.put(&[0u8; PAGE]).unwrap();
        let old = f.current.as_ref().unwrap().id;
        f.current.as_ref().unwrap().agent.shutdown(std::net::Shutdown::Both).unwrap();
        f.put(&[0u8; PAGE]).unwrap();
        assert_ne!(f.current.as_ref().unwrap().id, old);
    }

    #[test]
    fn an_unreachable_agent_is_asked_again_after_a_wait() {
        let mut f = Frames { request: unreachable, ..frames() };
        assert!(f.put(&[0u8; PAGE]).is_none());
        f.request = local;
        assert!(f.put(&[0u8; PAGE]).is_none());
        f.retry_at = Some(Instant::now());
        assert!(f.put(&[0u8; PAGE]).is_some());
    }

    #[test]
    fn a_refusing_agent_is_asked_again_after_a_wait() {
        let mut f = Frames { request: refusing, ..frames() };
        assert!(f.put(&[0u8; PAGE]).is_none());
        f.request = local;
        assert!(f.put(&[0u8; PAGE]).is_none());
        f.retry_at = Some(Instant::now());
        assert!(f.put(&[0u8; PAGE]).is_some());
    }
}
