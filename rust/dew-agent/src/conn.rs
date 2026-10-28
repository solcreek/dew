//! The byte stream a request arrives on. In the guest it is an accepted
//! AF_VSOCK socket; tests use a Unix socketpair. Everything above this
//! trait is transport-agnostic, so it builds and tests on macOS too.

use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::net::UnixStream;

pub trait Conn: Read + Write + Send + Sized + 'static {
    /// A second handle on the same socket (for a concurrent reader/writer).
    fn try_clone(&self) -> std::io::Result<Self>;
    /// Shut the socket down in both directions, so a thread blocked reading
    /// a clone returns. Mirrors the Go agent's deferred conn.Close.
    fn shutdown(&self);
}

impl Conn for File {
    fn try_clone(&self) -> std::io::Result<Self> {
        File::try_clone(self)
    }
    fn shutdown(&self) {
        unsafe { libc::shutdown(self.as_raw_fd(), libc::SHUT_RDWR) };
    }
}

impl Conn for UnixStream {
    fn try_clone(&self) -> std::io::Result<Self> {
        UnixStream::try_clone(self)
    }
    fn shutdown(&self) {
        let _ = UnixStream::shutdown(self, std::net::Shutdown::Both);
    }
}

/// SO_RCVTIMEO on any socket fd (File has no set_read_timeout); `None`
/// clears it. A read past the timeout fails with WouldBlock/TimedOut.
pub fn set_read_timeout(fd: &impl AsRawFd, d: Option<std::time::Duration>) -> std::io::Result<()> {
    let d = d.unwrap_or_default();
    // Assign through `as _`: musl's time_t/suseconds_t aliases are deprecated
    // (they widen in musl 1.2), and the field types are what matter.
    let mut tv: libc::timeval = unsafe { std::mem::zeroed() };
    tv.tv_sec = d.as_secs() as _;
    tv.tv_usec = d.subsec_micros() as _;
    let rc = unsafe {
        libc::setsockopt(
            fd.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_RCVTIMEO,
            &tv as *const libc::timeval as *const libc::c_void,
            std::mem::size_of::<libc::timeval>() as libc::socklen_t,
        )
    };
    if rc < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// Copy bytes both ways between `a` and `tcp` until both directions end. No
/// half-close, as in the Go agent's connect and expose proxies.
pub fn proxy<C: Conn>(mut a: C, mut tcp: std::net::TcpStream) {
    let (Ok(mut a_in), Ok(mut tcp_out)) = (a.try_clone(), tcp.try_clone()) else {
        return;
    };
    let up = std::thread::spawn(move || {
        let _ = std::io::copy(&mut a_in, &mut tcp_out);
    });
    let _ = std::io::copy(&mut tcp, &mut a);
    let _ = up.join();
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::time::{Duration, Instant};

    #[test]
    fn read_timeout_applies_and_clears() {
        let (mut a, mut b) = UnixStream::pair().unwrap();
        set_read_timeout(&a, Some(Duration::from_millis(50))).unwrap();
        let t = Instant::now();
        assert!(a.read(&mut [0u8; 1]).is_err(), "read must time out");
        assert!(t.elapsed() < Duration::from_secs(2));

        // Cleared: a read now waits past the old timeout for late data, as
        // an idle exposed connection must.
        set_read_timeout(&a, None).unwrap();
        let w = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(300));
            b.write_all(b"x").unwrap();
        });
        let mut buf = [0u8; 1];
        assert_eq!(a.read(&mut buf).unwrap(), 1);
        w.join().unwrap();
    }

    #[test]
    fn read_timeout_on_a_non_socket_fails() {
        let f = File::open("/dev/null").unwrap();
        assert!(set_read_timeout(&f, Some(Duration::from_millis(1))).is_err());
    }
}
