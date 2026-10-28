//! The confine shim (mirrors maybeRunConfineShim / runConfineShim in
//! cmd/dew-agent/confine.go). A confined exec re-runs the agent binary as
//! `[exe, --dew-confine-shim, target, args...]` with the spec in
//! DEW_CONFINE_SPEC. That fresh process, single-threaded and still root,
//! applies the spec and then execs the target; any failure exits 126 with
//! `dew-confine: <reason>` on stderr, as the Go shim does.

use super::apply;
use super::ids::Db;
use dew_proto::Confinement;
use std::ffi::OsString;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::process::CommandExt;

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
    let steps = || -> Result<std::path::PathBuf, String> {
        // Read-only filesystem first: it needs the mount namespace and root.
        if c.read_only_root {
            apply::unshare_mount_namespace()?;
            apply::apply_read_only_fs(&c)?;
        }
        apply::apply_privilege_drop(&c, &exec_user, &Db::default(), CAP_LAST_CAP)?;
        // Resolve against the shim's PATH, which main has pinned to the
        // agent's (as the Go agent pins it before its shim runs), before
        // seccomp, so an allowlist can't EPERM the lookup.
        let name = String::from_utf8_lossy(prog.as_bytes()).into_owned();
        let path = crate::env::look_path(&name, &std::env::var("PATH").unwrap_or_default())
            .map_err(|e| format!("resolve {name:?}: {e}"))?;
        // Last: seccomp. Only the execve below follows.
        apply::apply_seccomp(&c)?;
        Ok(path)
    };
    let path = match steps() {
        Ok(p) => p,
        Err(e) => return e,
    };
    // The control channel must not leak into the confined process. The shim
    // is single-threaded, so mutating its environment is safe.
    std::env::remove_var(SPEC_ENV);
    std::env::remove_var(EXEC_USER_ENV);
    let err = std::process::Command::new(&path)
        .arg0(prog)
        .args(&target[1..])
        .exec();
    crate::gofmt::errno_text(&err)
}
