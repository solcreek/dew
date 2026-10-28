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
