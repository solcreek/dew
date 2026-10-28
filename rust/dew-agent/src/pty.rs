//! Pseudo-terminals for TTY exec (the role creack/pty plays in the Go agent).

use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

// The libc crate does not export these for Apple targets; values are from
// the macOS SDK's <sys/ttycom.h> (the same encoding as the other BSDs).
#[cfg(target_os = "macos")]
const TIOCSCTTY: libc::c_ulong = 0x2000_7461; // _IO('t', 97)
#[cfg(target_os = "macos")]
const TIOCSWINSZ: libc::c_ulong = 0x8008_7467; // _IOW('t', 103, struct winsize)
#[cfg(target_os = "macos")]
const TIOCGWINSZ: libc::c_ulong = 0x4008_7468; // _IOR('t', 104, struct winsize)
#[cfg(not(target_os = "macos"))]
use libc::{TIOCGWINSZ, TIOCSCTTY, TIOCSWINSZ};

/// A pty pair. The agent keeps `master`; the child gets `slave` as its
/// stdin/stdout/stderr and controlling terminal.
pub struct Pty {
    pub master: File,
    pub slave: OwnedFd,
}

fn winsize(rows: u16, cols: u16) -> libc::winsize {
    libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    }
}

// The libc crate exports ptsname_r for Linux but not Apple; macOS has it
// since 10.13.4 (<_stdlib.h>), and dew requires macOS 13.
#[cfg(target_os = "macos")]
extern "C" {
    fn ptsname_r(fd: libc::c_int, buf: *mut libc::c_char, buflen: libc::size_t) -> libc::c_int;
}
#[cfg(not(target_os = "macos"))]
use libc::ptsname_r;

fn check(rc: libc::c_int) -> io::Result<libc::c_int> {
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(rc)
}

/// Open a pty. The window size is applied only when `rows` or `cols` is
/// non-zero, as the Go agent calls pty.Setsize only then.
///
/// Both ends are created close-on-exec atomically (O_CLOEXEC), not marked
/// afterwards: the agent forks on other threads concurrently, and a child
/// forked between creation and an fcntl would inherit the slave and keep it
/// open, so this TTY exec would never see EIO. (openpty(3) cannot do this.)
pub fn open(rows: u16, cols: u16) -> io::Result<Pty> {
    let flags = libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC;
    let m = check(unsafe { libc::posix_openpt(flags) })?;
    let master = unsafe { File::from_raw_fd(m) };
    check(unsafe { libc::grantpt(m) })?;
    check(unsafe { libc::unlockpt(m) })?;
    let mut name = [0 as libc::c_char; 128];
    let rc = unsafe { ptsname_r(m, name.as_mut_ptr(), name.len()) };
    if rc != 0 {
        // glibc/musl return the error number; macOS returns -1 and sets errno.
        return Err(if rc > 0 {
            io::Error::from_raw_os_error(rc)
        } else {
            io::Error::last_os_error()
        });
    }
    let s = check(unsafe { libc::open(name.as_ptr(), flags) })?;
    let slave = unsafe { OwnedFd::from_raw_fd(s) };
    if rows > 0 || cols > 0 {
        set_size(&master, rows, cols)?;
    }
    Ok(Pty { master, slave })
}

/// Set the terminal window size (a resize from the host).
pub fn set_size(fd: &impl AsRawFd, rows: u16, cols: u16) -> io::Result<()> {
    let ws = winsize(rows, cols);
    if unsafe { libc::ioctl(fd.as_raw_fd(), TIOCSWINSZ as _, &ws) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Read the terminal window size as (rows, cols).
pub fn get_size(fd: &impl AsRawFd) -> io::Result<(u16, u16)> {
    let mut ws = winsize(0, 0);
    if unsafe { libc::ioctl(fd.as_raw_fd(), TIOCGWINSZ as _, &mut ws) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok((ws.ws_row, ws.ws_col))
}

/// For `Command::pre_exec` in the child, after stdin is the pty slave: start
/// a new session and make the slave its controlling terminal (pty.Start's
/// Setsid + Setctty). Only async-signal-safe calls.
pub fn become_session_leader_on_stdin() -> io::Result<()> {
    if unsafe { libc::setsid() } < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::ioctl(0, TIOCSCTTY as _, 0) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::fd::RawFd;

    fn is_cloexec(fd: RawFd) -> bool {
        unsafe { libc::fcntl(fd, libc::F_GETFD) & libc::FD_CLOEXEC != 0 }
    }

    #[test]
    fn open_applies_the_requested_size() {
        let p = open(24, 80).unwrap();
        assert_eq!(get_size(&p.slave).unwrap(), (24, 80));
        assert_eq!(get_size(&p.master).unwrap(), (24, 80));
    }

    #[test]
    fn open_without_size_leaves_it_unset() {
        let p = open(0, 0).unwrap();
        assert_eq!(get_size(&p.slave).unwrap(), (0, 0));
    }

    #[test]
    fn set_size_round_trips_through_the_master() {
        let p = open(24, 80).unwrap();
        set_size(&p.master, 50, 132).unwrap();
        assert_eq!(get_size(&p.slave).unwrap(), (50, 132));
    }

    #[test]
    fn both_ends_are_close_on_exec() {
        let p = open(0, 0).unwrap();
        assert!(is_cloexec(p.master.as_raw_fd()));
        assert!(is_cloexec(p.slave.as_raw_fd()));
    }

    #[test]
    fn concurrent_spawns_never_inherit_a_pty() {
        // The agent serves each conn on its own thread, so a pipe exec can
        // fork while a TTY exec is opening its pty. Any pty fd that is not
        // close-on-exec from birth leaks into that child, which then holds
        // the slave open and the TTY exec never sees EIO. The child's
        // stdio is null/pipe, so any terminal fd it can see was leaked.
        use std::process::{Command, Stdio};
        use std::sync::atomic::{AtomicBool, Ordering};
        use std::sync::Arc;
        let stop = Arc::new(AtomicBool::new(false));
        let s = stop.clone();
        let opener = std::thread::spawn(move || {
            while !s.load(Ordering::Relaxed) {
                drop(open(0, 0).unwrap());
            }
        });
        let probe = "for f in /dev/fd/*; do n=${f##*/}; [ \"$n\" -gt 2 ] && [ -t \"$n\" ] && echo LEAK; done; true";
        let mut leaks = 0;
        for _ in 0..300 {
            let out = Command::new("/bin/sh")
                .args(["-c", probe])
                .stdin(Stdio::null())
                .stderr(Stdio::null())
                .output()
                .unwrap();
            leaks += String::from_utf8_lossy(&out.stdout).matches("LEAK").count();
        }
        stop.store(true, Ordering::Relaxed);
        opener.join().unwrap();
        assert_eq!(
            leaks, 0,
            "pty fds leaked into concurrently spawned children"
        );
    }

    #[test]
    fn slave_is_a_terminal() {
        let p = open(0, 0).unwrap();
        assert_eq!(unsafe { libc::isatty(p.slave.as_raw_fd()) }, 1);
    }

    #[test]
    fn child_gets_the_pty_as_controlling_terminal() {
        // /dev/tty opens only when the process has a controlling terminal;
        // `stty size </dev/tty` then proves it is *our* pty (its size), not
        // one inherited from whoever runs the tests.
        use std::os::unix::process::CommandExt;
        use std::process::{Command, Stdio};
        let p = open(31, 97).unwrap();
        let mut cmd = Command::new("/bin/sh");
        cmd.args(["-c", "stty size </dev/tty"])
            .stdin(Stdio::from(p.slave.try_clone().unwrap()))
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        unsafe { cmd.pre_exec(become_session_leader_on_stdin) };
        let out = cmd.output().unwrap();
        assert!(
            out.status.success(),
            "stderr: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(String::from_utf8_lossy(&out.stdout).trim(), "31 97");
    }

    #[test]
    fn set_size_on_a_non_terminal_fails() {
        let f = File::open("/dev/null").unwrap();
        assert!(set_size(&f, 1, 1).is_err());
        assert!(get_size(&f).is_err());
    }
}
