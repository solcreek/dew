//! AF_VSOCK listener (Linux guest only).

use std::fs::File;
use std::io;
use std::os::fd::{FromRawFd, OwnedFd};

/// Listen on `port` for any CID, as mdlayher/vsock's Listen(port, nil).
pub fn listen(port: u32) -> io::Result<OwnedFd> {
    unsafe {
        let fd = libc::socket(libc::AF_VSOCK, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0);
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let owned = OwnedFd::from_raw_fd(fd);
        let mut addr: libc::sockaddr_vm = std::mem::zeroed();
        addr.svm_family = libc::AF_VSOCK as libc::sa_family_t;
        addr.svm_port = port;
        addr.svm_cid = libc::VMADDR_CID_ANY;
        let len = std::mem::size_of::<libc::sockaddr_vm>() as libc::socklen_t;
        if libc::bind(fd, &addr as *const _ as *const libc::sockaddr, len) < 0
            || libc::listen(fd, 128) < 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(owned)
    }
}

/// Accept one connection as a File (read/write on the socket fd).
pub fn accept(listener: &OwnedFd) -> io::Result<File> {
    use std::os::fd::AsRawFd;
    let fd = unsafe {
        libc::accept4(
            listener.as_raw_fd(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            libc::SOCK_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(unsafe { File::from_raw_fd(fd) })
}
