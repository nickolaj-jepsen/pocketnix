//! What makes the compositor rescan the app list: inotify on the directories
//! a scan read, and SIGHUP.

use std::collections::HashMap;
use std::collections::HashSet;
use std::ffi::OsStr;
use std::ffi::OsString;
use std::io::Error;
use std::os::fd::AsRawFd;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;

/// Blocks SIGHUP for the process and returns a signalfd that reads it. Call
/// before starting threads, so all of them inherit the mask (std's spawn
/// clears it for children).
pub fn sighup_fd() -> std::io::Result<OwnedFd> {
    // SAFETY: plain libc calls on a local sigset_t.
    unsafe {
        let mut set: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, libc::SIGHUP);
        if libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut()) != 0 {
            return Err(Error::last_os_error());
        }
        let fd = libc::signalfd(-1, &set, libc::SFD_NONBLOCK | libc::SFD_CLOEXEC);
        if fd < 0 {
            return Err(Error::last_os_error());
        }
        Ok(OwnedFd::from_raw_fd(fd))
    }
}

/// Drains a nonblocking fd; true if it had anything.
pub fn drain(fd: &impl AsRawFd) -> bool {
    let mut buf = [0u8; 4096];
    let mut any = false;
    loop {
        // SAFETY: reading into a local buffer of its length.
        let n = unsafe { libc::read(fd.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len()) };
        if n <= 0 {
            return any;
        }
        any = true;
    }
}

/// A directory to watch: every entry in it, or only the named ones (the
/// symlinks a profile switch replaces, in a directory that sees other writes).
pub type Watch = (PathBuf, Option<HashSet<OsString>>);

pub struct Inotify {
    fd: OwnedFd,
    watches: HashMap<PathBuf, (i32, Option<HashSet<OsString>>)>,
}

impl Inotify {
    pub fn new() -> std::io::Result<Self> {
        // SAFETY: inotify_init1 returns a new descriptor or -1.
        let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
        if fd < 0 {
            return Err(Error::last_os_error());
        }
        // SAFETY: a fresh descriptor we own.
        Ok(Self {
            fd: unsafe { OwnedFd::from_raw_fd(fd) },
            watches: HashMap::new(),
        })
    }

    pub fn fd(&self) -> &OwnedFd {
        &self.fd
    }

    /// Watches exactly `dirs` for entries appearing, changing or going.
    pub fn set(&mut self, dirs: &[Watch]) {
        let mut want: HashMap<PathBuf, Option<HashSet<OsString>>> = HashMap::new();
        for (dir, names) in dirs {
            match (want.get_mut(dir), names) {
                (Some(Some(have)), Some(names)) => have.extend(names.iter().cloned()),
                (Some(have), None) => *have = None,
                (Some(None), Some(_)) => {}
                (None, names) => {
                    want.insert(dir.clone(), names.clone());
                }
            }
        }
        self.watches.retain(|path, (wd, _)| {
            if want.contains_key(path) {
                return true;
            }
            // SAFETY: removing a watch descriptor this fd returned.
            unsafe { libc::inotify_rm_watch(self.fd.as_raw_fd(), *wd) };
            false
        });
        let mask = libc::IN_CREATE
            | libc::IN_DELETE
            | libc::IN_MOVED_FROM
            | libc::IN_MOVED_TO
            | libc::IN_CLOSE_WRITE
            | libc::IN_ATTRIB
            | libc::IN_DELETE_SELF
            | libc::IN_MOVE_SELF;
        for (dir, names) in want {
            if let Some(w) = self.watches.get_mut(&dir) {
                w.1 = names;
                continue;
            }
            let Ok(c) = std::ffi::CString::new(dir.as_os_str().as_bytes()) else { continue };
            // SAFETY: a valid path string and mask.
            let wd = unsafe { libc::inotify_add_watch(self.fd.as_raw_fd(), c.as_ptr(), mask) };
            if wd >= 0 {
                self.watches.insert(dir, (wd, names));
            }
        }
    }

    /// Reads the pending events; true if any concerns a watched entry.
    pub fn changed(&self) -> bool {
        let mut relevant = false;
        let mut buf = [0u8; 4096];
        loop {
            // SAFETY: reading into a local buffer of its length.
            let n = unsafe { libc::read(self.fd.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len()) };
            if n <= 0 {
                return relevant;
            }
            let mut at = 0;
            let head = std::mem::size_of::<libc::inotify_event>();
            while at + head <= n as usize {
                // SAFETY: the kernel wrote a whole event header here.
                let event: libc::inotify_event = unsafe { std::ptr::read_unaligned(buf[at..].as_ptr().cast()) };
                let name = &buf[at + head..at + head + event.len as usize];
                let name = OsStr::from_bytes(name.split(|b| *b == 0).next().unwrap_or_default());
                at += head + event.len as usize;
                let filter = self.watches.values().find(|(wd, _)| *wd == event.wd).map(|(_, f)| f);
                relevant |= match filter {
                    Some(Some(names)) => names.contains(name),
                    Some(None) => true,
                    None => false,
                };
            }
        }
    }
}
