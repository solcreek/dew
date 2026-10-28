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

/// Read one frame's payload within `budget` for the *whole* frame, as Go's
/// ReadJSONTimeout bounds the full ReadJSON. SO_RCVTIMEO alone would bound
/// each read, so a peer trickling bytes could stretch a frame indefinitely;
/// here every read gets only what is left of the one deadline. The socket's
/// read timeout is cleared again before returning.
pub fn read_frame_bytes_within<F>(
    conn: &mut F,
    budget: std::time::Duration,
) -> std::io::Result<Vec<u8>>
where
    F: std::io::Read + AsRawFd,
{
    struct Deadline<'a, F> {
        inner: &'a mut F,
        until: std::time::Instant,
    }
    impl<F: std::io::Read + AsRawFd> std::io::Read for Deadline<'_, F> {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            let left = self
                .until
                .saturating_duration_since(std::time::Instant::now());
            if left.is_zero() {
                return Err(std::io::ErrorKind::TimedOut.into());
            }
            // Never 0: a zero SO_RCVTIMEO means "no timeout".
            set_read_timeout(
                self.inner,
                Some(left.max(std::time::Duration::from_millis(1))),
            )?;
            self.inner.read(buf)
        }
    }
    let until = std::time::Instant::now() + budget;
    let frame = dew_proto::read_frame_bytes(&mut Deadline {
        inner: &mut *conn,
        until,
    });
    set_read_timeout(conn, None)?;
    frame
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

    fn frame(payload: &[u8]) -> Vec<u8> {
        let mut f = (payload.len() as u32).to_be_bytes().to_vec();
        f.extend_from_slice(payload);
        f
    }

    #[test]
    fn frame_within_budget_is_read_and_timeout_cleared() {
        let (mut a, mut b) = UnixStream::pair().unwrap();
        b.write_all(&frame(b"{\"ok\":true}")).unwrap();
        let got = read_frame_bytes_within(&mut a, Duration::from_secs(2)).unwrap();
        assert_eq!(got, b"{\"ok\":true}");
        // Cleared: a later read waits past the budget for late data.
        let w = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(300));
            b.write_all(b"x").unwrap();
        });
        let mut one = [0u8; 1];
        set_read_timeout(&a, None).unwrap();
        assert_eq!(a.read(&mut one).unwrap(), 1);
        w.join().unwrap();
    }

    #[test]
    fn trickled_frame_fails_at_the_one_deadline() {
        let (mut a, mut b) = UnixStream::pair().unwrap();
        let f = frame(b"{\"ok\":true}");
        let w = std::thread::spawn(move || {
            for byte in f {
                if b.write_all(&[byte]).is_err() {
                    return;
                }
                std::thread::sleep(Duration::from_millis(50));
            }
        });
        let t = Instant::now();
        assert!(read_frame_bytes_within(&mut a, Duration::from_millis(200)).is_err());
        assert!(
            t.elapsed() < Duration::from_millis(600),
            "{:?}",
            t.elapsed()
        );
        drop(a);
        w.join().unwrap();
    }

    #[test]
    fn zero_budget_times_out_instead_of_waiting_forever() {
        let (mut a, _b) = UnixStream::pair().unwrap();
        let t = Instant::now();
        assert!(read_frame_bytes_within(&mut a, Duration::ZERO).is_err());
        assert!(t.elapsed() < Duration::from_millis(500));
    }

    #[test]
    fn read_timeout_on_a_non_socket_fails() {
        let f = File::open("/dev/null").unwrap();
        assert!(set_read_timeout(&f, Some(Duration::from_millis(1))).is_err());
    }
}
