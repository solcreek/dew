//! Pseudo-terminals for TTY exec (the role creack/pty plays in the Go agent).

use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};

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

fn cloexec(fd: RawFd) -> io::Result<()> {
    if unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Open a pty. The window size is applied only when `rows` or `cols` is
/// non-zero, as the Go agent calls pty.Setsize only then.
pub fn open(rows: u16, cols: u16) -> io::Result<Pty> {
    let (mut m, mut s) = (-1, -1);
    let mut ws = winsize(rows, cols);
    // termp/winp are `*const` on Linux and `*mut` on macOS; `as _` fits both.
    let wp: *mut libc::winsize = if rows > 0 || cols > 0 {
        &mut ws
    } else {
        std::ptr::null_mut()
    };
    let tp: *mut libc::termios = std::ptr::null_mut();
    let rc = unsafe { libc::openpty(&mut m, &mut s, std::ptr::null_mut(), tp as _, wp as _) };
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    // Own both fds before anything can fail, so an error can't leak them.
    let (master, slave) = unsafe { (File::from_raw_fd(m), OwnedFd::from_raw_fd(s)) };
    // Spawned children must inherit only the dup'd stdio, never these.
    cloexec(master.as_raw_fd())?;
    cloexec(slave.as_raw_fd())?;
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
