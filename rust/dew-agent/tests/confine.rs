//! --confine enforcement, for real: each case runs the built agent as the
//! confine shim and inspects the confined target from the inside
//! (/proc/self/status, the filesystem, failing syscalls). Linux only, as root
//! (they skip otherwise); run inside a dew guest or a disposable VM.
//!
//! DEW_AGENT_EXE overrides the agent binary (default: Cargo's build of it).
//! With DEW_GO_AGENT_EXE set to the Go agent (cmd/dew-agent), every case
//! also runs through the Go shim and must produce the same exit code and
//! output, so the two agents are checked to enforce identically.
#![cfg(target_os = "linux")]

use dew_proto::{Confinement, ExecRequest};
use std::os::unix::process::CommandExt;
use std::path::PathBuf;
use std::process::{Command, Stdio};

fn rust_agent() -> PathBuf {
    std::env::var_os("DEW_AGENT_EXE")
        .map(PathBuf::from)
        .unwrap_or_else(|| env!("CARGO_BIN_EXE_dew-agent").into())
}

fn go_agent() -> Option<PathBuf> {
    std::env::var_os("DEW_GO_AGENT_EXE").map(PathBuf::from)
}

/// Root with CAP_SYS_ADMIN is needed for every case; skip loudly otherwise.
fn root() -> bool {
    let ok = unsafe { libc::geteuid() } == 0;
    if !ok {
        eprintln!("skipping: confine tests need root");
    }
    ok
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct Out {
    code: i32,
    stdout: String,
    stderr: String,
}

/// Run `script` under `spec` through one agent's shim. The Go shim expects
/// its parent to have created the mount namespace (CLONE_NEWNS at clone);
/// the Rust shim creates its own, which `shim_has_its_own_mount_namespace`
/// checks, so only the Go run is given one here.
fn shim(exe: &PathBuf, go: bool, spec: &str, exec_user: &str, script: &str) -> Out {
    shim_argv(exe, go, spec, exec_user, &["/bin/sh", "-c", script])
}

fn shim_argv(exe: &PathBuf, go: bool, spec: &str, exec_user: &str, target: &[&str]) -> Out {
    // Each shim gets the environment its own agent gives it: both inherit the
    // agent's DEW_EXEC_USER, and the Rust parent also passes it explicitly
    // as DEW_CONFINE_EXEC_USER (the Go shim reads DEW_EXEC_USER).
    let mut cmd = Command::new(exe);
    cmd.arg("--dew-confine-shim")
        .args(target)
        .env("DEW_CONFINE_SPEC", spec)
        .env("DEW_EXEC_USER", exec_user)
        .stdin(Stdio::null());
    if !go {
        cmd.env("DEW_CONFINE_EXEC_USER", exec_user);
    }
    if go {
        unsafe {
            cmd.pre_exec(|| {
                if libc::unshare(libc::CLONE_NEWNS) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
    }
    let o = cmd.output().expect("run shim");
    Out {
        code: o.status.code().unwrap_or(-1),
        stdout: String::from_utf8_lossy(&o.stdout).into_owned(),
        stderr: String::from_utf8_lossy(&o.stderr).into_owned(),
    }
}

/// Run a case through the Rust shim, and through the Go shim when one is
/// given, requiring the same exit code and stdout. Returns the Rust result.
fn case(spec: &Confinement, exec_user: &str, script: &str) -> Out {
    case_json(&serde_json::to_string(spec).unwrap(), exec_user, script)
}

fn case_json(spec: &str, exec_user: &str, script: &str) -> Out {
    let rs = shim(&rust_agent(), false, spec, exec_user, script);
    if let Some(go) = go_agent() {
        let g = shim(&go, true, spec, exec_user, script);
        assert_eq!(
            (g.code, &g.stdout),
            (rs.code, &rs.stdout),
            "Go and Rust shims differ for spec {spec}\n go stderr: {}\n rs stderr: {}",
            g.stderr,
            rs.stderr
        );
    }
    rs
}

/// The confined process's view of itself.
const STATUS: &str = "grep -E '^(Uid|Gid|Groups|CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs|Seccomp):' /proc/self/status";

fn field(out: &str, name: &str) -> String {
    out.lines()
        .find_map(|l| l.strip_prefix(&format!("{name}:")))
        .map(|v| v.split_whitespace().collect::<Vec<_>>().join(" "))
        .unwrap_or_else(|| panic!("no {name} in:\n{out}"))
}

fn caps(out: &str, name: &str) -> u64 {
    u64::from_str_radix(&field(out, name), 16).unwrap()
}

fn spec(f: impl FnOnce(&mut Confinement)) -> Confinement {
    let mut c = Confinement::default();
    f(&mut c);
    c
}

fn strs(v: &[&str]) -> Vec<String> {
    v.iter().map(|s| s.to_string()).collect()
}

// ------------------------------------------------------------- identity

#[test]
fn user_drops_uid_gid_and_clears_groups() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.user = "65534".into()), "", STATUS);
    assert_eq!(o.code, 0, "{o:?}");
    assert_eq!(field(&o.stdout, "Uid"), "65534 65534 65534 65534");
    assert_eq!(field(&o.stdout, "Gid"), "65534 65534 65534 65534");
    assert_eq!(field(&o.stdout, "Groups"), "");
    assert_eq!(
        caps(&o.stdout, "CapEff"),
        0,
        "a non-root target has no effective caps"
    );
}

#[test]
fn group_only_keeps_root_uid() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.group = "1234".into()), "", STATUS);
    assert_eq!(field(&o.stdout, "Uid"), "0 0 0 0");
    assert_eq!(field(&o.stdout, "Gid"), "1234 1234 1234 1234");
    assert_eq!(field(&o.stdout, "Groups"), "");
}

#[test]
fn exec_user_applies_when_the_unit_names_no_user() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.no_new_privs = true), "65534", STATUS);
    assert_eq!(field(&o.stdout, "Uid"), "65534 65534 65534 65534");
    let o = case(&spec(|c| c.user = "0".into()), "65534", STATUS);
    assert_eq!(field(&o.stdout, "Uid"), "0 0 0 0", "the unit's User= wins");
}

#[test]
fn dynamic_user_is_nobody() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.dynamic_user = true), "", STATUS);
    assert_eq!(field(&o.stdout, "Uid"), "65534 65534 65534 65534");
}

#[test]
fn unknown_user_fails_closed_before_running_anything() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.user = "ghost-no-such".into()), "", "echo RAN");
    assert_eq!(o.code, 126);
    assert!(!o.stdout.contains("RAN"));
    assert_eq!(
        o.stderr.trim(),
        "dew-confine: user \"ghost-no-such\": user: unknown user ghost-no-such"
    );
}

#[test]
fn ids_that_are_not_a_uid_t_fail_closed() {
    if !root() {
        return;
    }
    // (uid_t)-1 would leave the ids unchanged and 2^32 would wrap to 0: the
    // target would run as root, so both shims refuse them.
    for (s, msg) in [
        (spec(|c| c.user = "4294967295".into()), "uid 4294967295"),
        (spec(|c| c.user = "4294967296".into()), "uid 4294967296"),
        (spec(|c| c.group = "4294967295".into()), "gid 4294967295"),
    ] {
        let o = case(&s, "", "echo RAN");
        assert_eq!(o.code, 126, "{msg}: {o:?}");
        assert!(!o.stdout.contains("RAN"), "{msg}: {o:?}");
        assert_eq!(o.stderr.trim(), format!("dew-confine: {msg} out of range"));
    }
}

// ------------------------------------------------------------- capabilities

#[test]
fn drop_caps_trims_the_bounding_set() {
    if !root() {
        return;
    }
    let o = case(
        &spec(|c| c.drop_caps = strs(&["cap_sys_admin", "cap_net_raw"])),
        "",
        STATUS,
    );
    let bnd = caps(&o.stdout, "CapBnd");
    assert_eq!(bnd & (1 << 21), 0, "cap_sys_admin gone");
    assert_eq!(bnd & (1 << 13), 0, "cap_net_raw gone");
    assert_ne!(bnd & (1 << 0), 0, "others kept");
}

#[test]
fn drop_all_caps_keeps_only_the_named_ones_for_root() {
    if !root() {
        return;
    }
    let o = case(
        &spec(|c| {
            c.drop_all_caps = true;
            c.keep_caps = strs(&["cap_chown"])
        }),
        "",
        STATUS,
    );
    assert_eq!(caps(&o.stdout, "CapBnd"), 1);
    assert_eq!(caps(&o.stdout, "CapEff"), 1);
}

#[test]
fn kept_caps_survive_the_drop_to_non_root_via_ambient() {
    if !root() {
        return;
    }
    let c = spec(|c| {
        c.user = "65534".into();
        c.drop_all_caps = true;
        c.keep_caps = strs(&["cap_net_bind_service"]);
    });
    let o = case(&c, "", STATUS);
    let bit = 1 << 10;
    assert_eq!(field(&o.stdout, "Uid"), "65534 65534 65534 65534");
    assert_eq!(caps(&o.stdout, "CapBnd"), bit);
    assert_eq!(caps(&o.stdout, "CapAmb"), bit);
    assert_eq!(caps(&o.stdout, "CapEff"), bit);
}

#[test]
fn no_new_privs_is_set() {
    if !root() {
        return;
    }
    let o = case(&spec(|c| c.no_new_privs = true), "", STATUS);
    assert_eq!(field(&o.stdout, "NoNewPrivs"), "1");
    let o = case(&spec(|c| c.group = "0".into()), "", STATUS);
    assert_eq!(
        field(&o.stdout, "NoNewPrivs"),
        "0",
        "only when asked (or for seccomp)"
    );
}

// ------------------------------------------------------------- filesystem

fn rw_dir(tag: &str) -> String {
    format!("/var/lib/dew-confine-test-{tag}-{}", std::process::id())
}

#[test]
fn read_only_root_with_writable_exceptions() {
    if !root() {
        return;
    }
    let rw = rw_dir("ro");
    let _ = std::fs::remove_dir_all(&rw);
    let c = spec(|c| {
        c.read_only_root = true;
        c.read_write_paths = vec![rw.clone()];
    });
    let script = format!(
        "touch /dew-ro-probe 2>/dev/null && echo root-writable || echo root-ro; \
         touch {rw}/ok && echo rw-ok; ls -d {rw}; touch /dev/null && echo dev-ok"
    );
    let o = case(&c, "", &script);
    assert_eq!(o.code, 0, "{o:?}");
    assert!(o.stdout.contains("root-ro"), "{o:?}");
    assert!(
        o.stdout.contains("rw-ok"),
        "missing path created as a writable dir: {o:?}"
    );
    assert!(
        o.stdout.contains("dev-ok"),
        "API filesystems stay writable: {o:?}"
    );
    // Outside the shim nothing changed: the root is still writable here.
    let probe = format!("/dew-after-{}", std::process::id());
    std::fs::write(&probe, "x").expect("test's own root must still be writable");
    let _ = std::fs::remove_file(&probe);
    let _ = std::fs::remove_dir_all(&rw);
}

#[test]
fn shim_has_its_own_mount_namespace() {
    if !root() {
        return;
    }
    // Only the Rust shim: it must unshare by itself (the Go one relies on its
    // parent), so a missing unshare can't hide behind an outer namespace.
    let ours = std::fs::read_link("/proc/self/ns/mnt").unwrap();
    let c = serde_json::to_string(&spec(|c| c.read_only_root = true)).unwrap();
    let o = shim(&rust_agent(), false, &c, "", "readlink /proc/self/ns/mnt");
    assert_eq!(o.code, 0, "{o:?}");
    assert_ne!(
        o.stdout.trim(),
        ours.to_str().unwrap(),
        "confined target shares the agent's mount namespace"
    );
}

#[test]
fn relative_read_write_path_is_rejected() {
    if !root() {
        return;
    }
    let c = spec(|c| {
        c.read_only_root = true;
        c.read_write_paths = strs(&["var/lib/rel"]);
    });
    let o = case(&c, "", "echo RAN");
    assert_eq!(o.code, 126);
    assert!(!o.stdout.contains("RAN"));
    assert_eq!(
        o.stderr.trim(),
        "dew-confine: read-write path \"var/lib/rel\" is not absolute"
    );
}

// ------------------------------------------------------------- seccomp

#[test]
fn address_family_denylist_blocks_inet_sockets() {
    if !root() {
        return;
    }
    let c = spec(|c| {
        c.address_families = strs(&["AF_INET", "AF_INET6"]);
        c.address_families_deny = true;
    });
    // Something that opens an AF_INET socket and reports EPERM: python3 where
    // present (OpenBSD nc on the CI runner is silent on failure without -v),
    // else BusyBox nc (the dew guest), which prints the error.
    let o = case(
        &c,
        "",
        &format!("{STATUS}; {{ if command -v python3 >/dev/null; then python3 -c 'import socket; socket.socket(socket.AF_INET)'; else nc -z -w 1 127.0.0.1 9; fi; }} 2>&1 | grep -o 'not permitted' | head -1"),
    );
    assert_eq!(field(&o.stdout, "Seccomp"), "2");
    assert_eq!(
        field(&o.stdout, "NoNewPrivs"),
        "1",
        "seccomp implies no_new_privs"
    );
    assert!(
        o.stdout.contains("not permitted"),
        "AF_INET socket allowed: {o:?}"
    );
}

#[test]
fn syscall_denylist_blocks_the_listed_calls() {
    if !root() {
        return;
    }
    let dir = format!("/tmp/dew-sc-{}", std::process::id());
    let c = spec(|c| {
        c.system_calls = strs(&["mkdir", "mkdirat"]);
        c.system_calls_deny = true;
    });
    let o = case(
        &c,
        "",
        &format!("mkdir {dir} 2>&1 | grep -o 'not permitted'; echo done"),
    );
    assert!(o.stdout.contains("not permitted"), "{o:?}");
    assert!(!std::path::Path::new(&dir).exists());
}

#[test]
fn syscall_allowlist_blocks_everything_else() {
    if !root() {
        return;
    }
    // Every syscall the arch knows except mkdir/mkdirat: the shell still runs,
    // mkdir is refused.
    let arch = dew_agent::confine::seccomp::Arch::native().unwrap();
    let allow: Vec<String> = arch
        .syscalls
        .iter()
        .map(|(n, _)| n.to_string())
        .filter(|n| n != "mkdir" && n != "mkdirat")
        .collect();
    let dir = format!("/tmp/dew-sc-allow-{}", std::process::id());
    let c = spec(|c| c.system_calls = allow);
    let o = case(
        &c,
        "",
        &format!("mkdir {dir} 2>&1 | grep -o 'not permitted'; echo still-running"),
    );
    assert!(
        o.stdout.contains("not permitted") && o.stdout.contains("still-running"),
        "{o:?}"
    );
    assert!(!std::path::Path::new(&dir).exists());
}

#[test]
fn shim_makes_no_syscall_between_seccomp_and_execve() {
    if !root() {
        return;
    }
    // An allowlist without rt_sigaction: the target (echo, which touches no
    // signals) must still start. Anything the shim did after installing the
    // filter other than execve, such as std's SIGPIPE reset in
    // Command::exec, would be refused and the exec would never happen.
    let arch = dew_agent::confine::seccomp::Arch::native().unwrap();
    let allow: Vec<String> = arch
        .syscalls
        .iter()
        .map(|(n, _)| n.to_string())
        .filter(|n| n != "rt_sigaction")
        .collect();
    let spec = serde_json::to_string(&spec(|c| c.system_calls = allow)).unwrap();
    let target = ["/bin/echo", "exec-ok"];
    let rs = shim_argv(&rust_agent(), false, &spec, "", &target);
    assert_eq!((rs.code, rs.stdout.as_str()), (0, "exec-ok\n"), "{rs:?}");
    if let Some(go) = go_agent() {
        let g = shim_argv(&go, true, &spec, "", &target);
        assert_eq!((g.code, &g.stdout), (rs.code, &rs.stdout), "go: {g:?}");
    }
}

#[test]
fn target_gets_default_sigpipe() {
    if !root() {
        return;
    }
    // Rust's runtime ignores SIGPIPE; the shim must hand the target the
    // default disposition (as Go's exec does), or `yes | head` never ends.
    let o = case(
        &spec(|c| c.no_new_privs = true),
        "",
        "grep '^SigIgn:' /proc/self/status",
    );
    let ign = u64::from_str_radix(field(&o.stdout, "SigIgn").trim(), 16).unwrap();
    assert_eq!(
        ign & (1 << (libc::SIGPIPE - 1)),
        0,
        "SIGPIPE ignored in the target: {o:?}"
    );
}

// ------------------------------------------------------------- the shim itself

#[test]
fn control_variables_do_not_leak_into_the_target() {
    if !root() {
        return;
    }
    let o = case(
        &spec(|c| c.no_new_privs = true),
        "65534",
        "env | grep -c '^DEW_CONFINE' || true",
    );
    assert_eq!(o.stdout.trim(), "0", "{o:?}");
}

#[test]
fn bad_spec_fails_closed() {
    if !root() {
        return;
    }
    let o = shim(&rust_agent(), false, "{not json", "", "echo RAN");
    assert_eq!(o.code, 126);
    assert!(!o.stdout.contains("RAN"));
    assert!(
        o.stderr.starts_with("dew-confine: bad confine spec: "),
        "{o:?}"
    );
}

#[test]
fn missing_target_is_reported() {
    if !root() {
        return;
    }
    let o = shim(&rust_agent(), false, "{}", "", "x");
    let missing = Command::new(rust_agent())
        .arg("--dew-confine-shim")
        .output()
        .unwrap();
    assert_eq!(o.code, 127, "sh -c x: command not found");
    assert_eq!(missing.status.code(), Some(126));
    assert_eq!(
        String::from_utf8_lossy(&missing.stderr).trim(),
        "dew-confine: no target command"
    );
}

// ------------------------------------------------------------- through exec

fn with_agent() {
    let _ = dew_agent::exec::SELF_EXE.set(rust_agent());
}

fn confined(script: &str, c: Confinement, env: &[&str]) -> ExecRequest {
    ExecRequest {
        command: "/bin/sh".into(),
        args: vec!["-c".into(), script.into()],
        env: strs(env),
        confine: Some(c),
        ..Default::default()
    }
}

#[test]
fn execute_runs_the_target_confined() {
    if !root() {
        return;
    }
    with_agent();
    let r = dew_agent::exec::execute_as(
        &confined("id -u", spec(|c| c.user = "65534".into()), &[]),
        None,
        None,
    );
    assert_eq!(
        (r.exit_code, r.stdout.trim(), r.error.as_str()),
        (0, "65534", ""),
        "{r:?}"
    );
}

#[test]
fn execute_reports_a_shim_failure_like_go() {
    if !root() {
        return;
    }
    with_agent();
    let r = dew_agent::exec::execute_as(
        &confined("echo RAN", spec(|c| c.user = "ghost-x".into()), &[]),
        None,
        None,
    );
    assert_eq!(r.exit_code, 126);
    assert_eq!(
        r.stderr.trim(),
        "dew-confine: user \"ghost-x\": user: unknown user ghost-x"
    );
    assert!(!r.stdout.contains("RAN"));
}

#[test]
fn request_env_cannot_replace_the_spec_or_the_exec_user() {
    if !root() {
        return;
    }
    with_agent();
    // A request trying to swap in an empty spec and a root exec user: the
    // agent's own spec and DEW_EXEC_USER still apply.
    let req = confined(
        "id -u",
        spec(|c| c.no_new_privs = true),
        &["DEW_CONFINE_SPEC={}", "DEW_CONFINE_EXEC_USER=0"],
    );
    let r = dew_agent::exec::execute_as(&req, None, Some("65534"));
    assert_eq!(r.stdout.trim(), "65534", "{r:?}");
}
