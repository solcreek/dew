//! The confine shim. A confined exec re-runs the agent binary as
//! `[exe, --dew-confine-shim, target, args...]` with the spec in
//! DEW_CONFINE_SPEC. That fresh process, single-threaded and still root,
//! applies the spec and then execs the target; any failure exits 126 with
//! `dew-confine: <reason>` on stderr.

use super::apply;
use super::ids::Db;
use dew_proto::Confinement;
use std::ffi::{CString, OsString};
use std::os::unix::ffi::OsStrExt;

pub use super::{EXEC_USER_ENV, SHIM_MARKER, SPEC_ENV};

const CAP_LAST_CAP: &str = "/proc/sys/kernel/cap_last_cap";

/// Run as the shim and never return if argv says so; otherwise return so the
/// agent starts normally. Call first in main, after the PATH pin, so the
/// target resolves the same way.
pub fn maybe_run() {
    let mut args = std::env::args_os();
    if args.nth(1).as_deref() != Some(std::ffi::OsStr::new(SHIM_MARKER)) {
        return;
    }
    let target: Vec<OsString> = args.collect();
    let e = run(&target);
    eprintln!("dew-confine: {e}");
    std::process::exit(126);
}

fn spec_from_env() -> Result<Confinement, String> {
    match std::env::var(SPEC_ENV) {
        Ok(s) if !s.is_empty() => {
            serde_json::from_str(&s).map_err(|e| format!("bad confine spec: {e}"))
        }
        _ => Ok(Confinement::default()),
    }
}

/// Apply the spec and exec the target; returns only the reason it failed.
fn run(target: &[OsString]) -> String {
    let Some(prog) = target.first() else {
        return "no target command".into();
    };
    let c = match spec_from_env() {
        Ok(c) => c,
        Err(e) => return e,
    };
    let exec_user = std::env::var(EXEC_USER_ENV).unwrap_or_default();
    let steps = || -> Result<Exec, String> {
        // Read-only filesystem first: it needs the mount namespace and root.
        if c.read_only_root {
            apply::unshare_mount_namespace()?;
            apply::apply_read_only_fs(&c)?;
        }
        apply::apply_privilege_drop(&c, &exec_user, &Db::default(), CAP_LAST_CAP)?;
        // Resolve against the shim's PATH, which main has pinned to the
        // agent's, before seccomp, so an allowlist can't EPERM the lookup.
        let name = String::from_utf8_lossy(prog.as_bytes()).into_owned();
        let path = crate::env::look_path(&name, &std::env::var("PATH").unwrap_or_default())
            .map_err(|e| format!("resolve {name:?}: {e}"))?;
        // Everything the exec needs is prepared now, while any syscall is
        // still allowed: the control channel is dropped from the environment
        // (the shim is single-threaded, so mutating it is safe), argv/envp are
        // built, and SIGPIPE goes back to default (Rust's runtime ignores it,
        // and the target must not inherit that; std's Command::exec would
        // reset it with an rt_sigaction, after the filter).
        std::env::remove_var(SPEC_ENV);
        std::env::remove_var(EXEC_USER_ENV);
        let exec = Exec::new(&path, target)?;
        // Nothing owned may be freed after the filter goes on.
        drop((name, path));
        unsafe { libc::signal(libc::SIGPIPE, libc::SIG_DFL) };
        // Last: seccomp. The only syscall after it is execve, so an allowlist
        // need not cover anything else.
        apply::apply_seccomp(&c)?;
        Ok(exec)
    };
    match steps() {
        Ok(exec) => exec.run(),
        Err(e) => e,
    }
}

/// A prepared execve: path, argv (argv[0] is the name as given) and envp,
/// with their NUL-terminated pointer arrays built up front, so running it
/// is a single syscall and nothing allocates once seccomp is on.
struct Exec {
    path: CString,
    // The pointer arrays point into these; kept alive alongside them.
    _argv: Vec<CString>,
    _envp: Vec<CString>,
    argv_ptrs: Vec<*const libc::c_char>,
    envp_ptrs: Vec<*const libc::c_char>,
}

fn ptrs(v: &[CString]) -> Vec<*const libc::c_char> {
    v.iter()
        .map(|s| s.as_ptr())
        .chain(std::iter::once(std::ptr::null()))
        .collect()
}

impl Exec {
    fn new(path: &std::path::Path, target: &[OsString]) -> Result<Exec, String> {
        let c = |b: &[u8]| CString::new(b).map_err(|_| "argument contains a NUL byte".to_string());
        let argv: Vec<CString> = target
            .iter()
            .map(|a| c(a.as_bytes()))
            .collect::<Result<_, _>>()?;
        let envp: Vec<CString> = std::env::vars_os()
            .map(|(k, v)| c(&[k.as_bytes(), b"=", v.as_bytes()].concat()))
            .collect::<Result<_, _>>()?;
        Ok(Exec {
            path: c(path.as_os_str().as_bytes())?,
            argv_ptrs: ptrs(&argv),
            envp_ptrs: ptrs(&envp),
            _argv: argv,
            _envp: envp,
        })
    }

    /// execve and nothing else. Returns only if it failed; formatting that
    /// reason may allocate, but the shim exits 126 either way.
    fn run(self) -> String {
        unsafe {
            libc::execve(
                self.path.as_ptr(),
                self.argv_ptrs.as_ptr(),
                self.envp_ptrs.as_ptr(),
            )
        };
        crate::gofmt::errno_text(&std::io::Error::last_os_error())
    }
}
