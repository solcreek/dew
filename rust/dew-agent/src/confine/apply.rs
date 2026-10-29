//! The Linux syscalls that enforce a confinement. Runs only in the
//! single-threaded confine shim, as root, just before it execs the target.

use super::caps::{caps_to_drop, kept_caps, last_capability};
use super::ids::{resolve_drop_id, Db};
use super::seccomp::{
    family_filter, needs_seccomp, resolve_families, syscall_filter, Arch, SockFilter,
};
use dew_proto::Confinement;
use std::ffi::CString;
use std::io;
use std::path::Path;

fn cstr(s: &str) -> Result<CString, String> {
    CString::new(s).map_err(|_| format!("path {s:?} contains a NUL byte"))
}

fn err(what: &str) -> String {
    format!(
        "{what}: {}",
        crate::gofmt::errno_text(&io::Error::last_os_error())
    )
}

fn mount(src: &str, target: &str, flags: libc::c_ulong) -> Result<(), String> {
    let (s, t) = (cstr(src)?, cstr(target)?);
    let rc = unsafe {
        libc::mount(
            s.as_ptr(),
            t.as_ptr(),
            std::ptr::null(),
            flags,
            std::ptr::null(),
        )
    };
    if rc < 0 {
        return Err(crate::gofmt::errno_text(&io::Error::last_os_error()));
    }
    Ok(())
}

/// Enter a private mount namespace (the shim does it itself, before touching
/// any mount, rather than asking for CLONE_NEWNS at clone time).
pub fn unshare_mount_namespace() -> Result<(), String> {
    if unsafe { libc::unshare(libc::CLONE_NEWNS) } < 0 {
        return Err(err("unshare mount namespace"));
    }
    Ok(())
}

/// ProtectSystem=strict + ReadWritePaths=: the root mount read-only inside
/// this (already private) mount namespace, API filesystems and the listed
/// paths left writable.
pub fn apply_read_only_fs(c: &Confinement) -> Result<(), String> {
    // Relative entries would be resolved against the cwd (req.dir) and mount
    // somewhere unintended; systemd's are always absolute.
    for p in &c.read_write_paths {
        if !Path::new(p).is_absolute() {
            return Err(format!("read-write path {p:?} is not absolute"));
        }
    }
    // Every mount private, so the remounts below never propagate back to the
    // agent's or host's view.
    mount("", "/", libc::MS_REC | libc::MS_PRIVATE).map_err(|e| format!("make / rprivate: {e}"))?;
    // Materialise missing writable paths while the tree is still writable. A
    // missing entry is always created as a directory (a state dir such as
    // /var/lib/app); a writable *file* exception must already exist.
    for p in &c.read_write_paths {
        match std::fs::metadata(p) {
            Ok(_) => {}
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                std::fs::create_dir_all(p).map_err(|e| {
                    format!(
                        "create read-write path {p:?}: {}",
                        crate::gofmt::errno_text(&e)
                    )
                })?;
            }
            Err(e) => {
                return Err(format!(
                    "stat read-write path {p:?}: {}",
                    crate::gofmt::errno_text(&e)
                ))
            }
        }
    }
    // Bind / onto itself (recursively, keeping submounts) so the read-only
    // flip below is per-mount, not on the shared superblock, which would make
    // the root read-only for the host and agent too.
    mount("/", "/", libc::MS_BIND | libc::MS_REC).map_err(|e| format!("bind-mount / : {e}"))?;
    // Only the root mount, non-recursively: /proc, /sys, /dev, /tmp, /run, the
    // cgroup mount and --share dirs keep their own flags, as systemd leaves
    // API filesystems writable.
    mount("", "/", libc::MS_BIND | libc::MS_REMOUNT | libc::MS_RDONLY)
        .map_err(|e| format!("remount / read-only: {e}"))?;
    for p in &c.read_write_paths {
        bind_read_write(p).map_err(|e| format!("read-write path {p:?}: {e}"))?;
    }
    Ok(())
}

/// Restore write access to `p` over the read-only root: bind it onto itself
/// (MS_REC only for a directory; a recursive bind of a file is invalid), then
/// remount the bind without MS_RDONLY.
fn bind_read_write(p: &str) -> Result<(), String> {
    let mut flags = libc::MS_BIND;
    if std::fs::metadata(p).is_ok_and(|m| m.is_dir()) {
        flags |= libc::MS_REC;
    }
    mount(p, p, flags).map_err(|e| format!("bind: {e}"))?;
    mount("", p, libc::MS_BIND | libc::MS_REMOUNT).map_err(|e| format!("remount rw: {e}"))?;
    Ok(())
}

fn prctl(op: libc::c_int, a: libc::c_ulong) -> io::Result<()> {
    if unsafe {
        libc::prctl(
            op,
            a,
            0 as libc::c_ulong,
            0 as libc::c_ulong,
            0 as libc::c_ulong,
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

const LINUX_CAPABILITY_VERSION_3: u32 = 0x2008_0522;

#[repr(C)]
struct CapHeader {
    version: u32,
    pid: libc::c_int,
}

#[repr(C)]
#[derive(Default, Clone, Copy)]
struct CapData {
    effective: u32,
    permitted: u32,
    inheritable: u32,
}

/// Add `caps` to this thread's inheritable set, keeping permitted/effective.
/// As root (full permitted) this is allowed for any cap still in the
/// bounding set.
fn add_inheritable(caps: &[u32]) -> io::Result<()> {
    let mut hdr = CapHeader {
        version: LINUX_CAPABILITY_VERSION_3,
        pid: 0,
    };
    let mut data = [CapData::default(); 2];
    if unsafe { libc::syscall(libc::SYS_capget, &mut hdr, data.as_mut_ptr()) } < 0 {
        return Err(io::Error::last_os_error());
    }
    for c in caps {
        data[(c >> 5) as usize].inheritable |= 1 << (c & 31);
    }
    if unsafe { libc::syscall(libc::SYS_capset, &mut hdr, data.as_ptr()) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn errno(what: String, e: io::Error) -> String {
    format!("{what}: {}", crate::gofmt::errno_text(&e))
}

/// The native uid/gid/capability/no_new_privs drop, after the mount work
/// (which needs root). Order matters:
/// 1. bounding set (needs CAP_SETPCAP, held as root);
/// 2. keeping caps as non-root: add them to the inheritable set while
///    CAP_SETUID is still held, and PR_SET_KEEPCAPS so permitted survives the
///    uid change;
/// 3. clear supplementary groups, then gid, then uid;
/// 4. raise the kept caps into the ambient set (effective after execve);
/// 5. no_new_privs last; a seccomp spec implies it.
///
/// The shim is single-threaded, so the thread that drops privilege is the one
/// that execs (credentials are per-thread in the kernel).
pub fn apply_privilege_drop(
    c: &Confinement,
    exec_user: &str,
    db: &Db,
    cap_last_cap_file: &str,
) -> Result<(), String> {
    let id = resolve_drop_id(c, exec_user, db)?;
    let last = last_capability(cap_last_cap_file);
    let drop = caps_to_drop(c, last)?;
    let keep = kept_caps(c, last)?;
    let want_seccomp = needs_seccomp(c);
    if id.uid.is_none() && id.gid.is_none() && drop.is_empty() && !c.no_new_privs && !want_seccomp {
        return Ok(());
    }
    for cap in &drop {
        prctl(libc::PR_CAPBSET_DROP, *cap as libc::c_ulong)
            .map_err(|e| errno(format!("drop bounding cap {cap}"), e))?;
    }
    // For a root target the bounding set already preserves the kept caps
    // across execve; only a non-root target needs the ambient set.
    let want_ambient = id.uid.is_some_and(|u| u != 0) && !keep.is_empty();
    if want_ambient {
        add_inheritable(&keep).map_err(|e| errno("set inheritable caps".into(), e))?;
        prctl(libc::PR_SET_KEEPCAPS, 1).map_err(|e| errno("set keepcaps".into(), e))?;
    }
    // Clear supplementary groups whenever the identity changes (as the old
    // setpriv --clear-groups), then gid before uid, each still allowed as root.
    if (id.uid.is_some() || id.gid.is_some()) && unsafe { libc::setgroups(0, std::ptr::null()) } < 0
    {
        return Err(err("setgroups"));
    }
    if let Some(g) = id.gid {
        if unsafe { libc::setresgid(g, g, g) } < 0 {
            return Err(err("setresgid"));
        }
    }
    if let Some(u) = id.uid {
        if unsafe { libc::setresuid(u, u, u) } < 0 {
            return Err(err("setresuid"));
        }
    }
    if want_ambient {
        for cap in &keep {
            let rc = unsafe {
                libc::prctl(
                    libc::PR_CAP_AMBIENT,
                    libc::PR_CAP_AMBIENT_RAISE as libc::c_ulong,
                    *cap as libc::c_ulong,
                    0 as libc::c_ulong,
                    0 as libc::c_ulong,
                )
            };
            if rc < 0 {
                return Err(err(&format!("raise ambient cap {cap}")));
            }
        }
    }
    if c.no_new_privs || want_seccomp {
        prctl(libc::PR_SET_NO_NEW_PRIVS, 1).map_err(|e| errno("set no_new_privs".into(), e))?;
    }
    Ok(())
}

/// Load one classic-BPF program on this thread (no TSYNC: the shim is
/// single-threaded and execs on this thread).
fn install_seccomp_filter(prog: &[SockFilter]) -> Result<(), String> {
    if prog.is_empty() {
        return Err("install seccomp filter: empty program".into());
    }
    let len = u16::try_from(prog.len())
        .map_err(|_| "install seccomp filter: program too long".to_string())?;
    #[repr(C)]
    struct SockFprog {
        len: u16,
        filter: *const SockFilter,
    }
    let fprog = SockFprog {
        len,
        filter: prog.as_ptr(),
    };
    let rc = unsafe {
        libc::syscall(
            libc::SYS_seccomp,
            libc::SECCOMP_SET_MODE_FILTER,
            0,
            &fprog as *const SockFprog,
        )
    };
    if rc < 0 {
        return Err(err("install seccomp filter"));
    }
    Ok(())
}

/// Install the spec's seccomp filters, last thing before exec (no_new_privs
/// was set by apply_privilege_drop). The address-family filter goes first:
/// once a default-deny syscall allowlist is active, a further seccomp(2) call
/// would itself be blocked.
pub fn apply_seccomp(c: &Confinement) -> Result<(), String> {
    if !needs_seccomp(c) {
        return Ok(());
    }
    let arch = Arch::native().ok_or("seccomp: unsupported arch")?;
    // Build every program before installing any: once a (possibly
    // default-deny) syscall allowlist is on, nothing may allocate, free or
    // call anything but the execve that follows; musl's allocator can mmap,
    // munmap or brk on either.
    let mut progs = Vec::with_capacity(2);
    if !c.address_families.is_empty() {
        let fams = resolve_families(&c.address_families)?;
        progs.push(family_filter(arch, &fams, c.address_families_deny)?);
    }
    if !c.system_calls.is_empty() {
        progs.push(syscall_filter(arch, &c.system_calls, c.system_calls_deny)?);
    }
    for p in &progs {
        install_seccomp_filter(p)?;
    }
    // Not freed: execve replaces the image anyway, and a free could syscall.
    std::mem::forget(progs);
    Ok(())
}
