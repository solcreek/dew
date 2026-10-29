//! Exec environment and command resolution (with Go's exec.LookPath rules
//! and error text).

use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

/// PATH guaranteed for guest exec. /usr/bin precedes /bin (and /usr/sbin
/// precedes /sbin) so util-linux tools win over BusyBox applets of the same
/// name; the confine shim depends on that for `setpriv`.
pub const DEFAULT_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";

/// The child env: `base` minus an empty `PATH=`, plus DEFAULT_PATH when base
/// has no PATH, then `extra` appended. Later entries win, as with Go's
/// exec.Cmd.Env, so a request's PATH overrides the agent's.
pub fn exec_env(base: &[String], extra: &[String]) -> Vec<String> {
    let mut has_path = false;
    let mut out = Vec::with_capacity(base.len() + extra.len() + 1);
    for e in base {
        if let Some(v) = e.strip_prefix("PATH=") {
            if v.is_empty() {
                continue;
            }
            has_path = true;
        }
        out.push(e.clone());
    }
    if !has_path {
        out.push(format!("PATH={DEFAULT_PATH}"));
    }
    out.extend(extra.iter().cloned());
    out
}

/// Split `KEY=VALUE` entries into pairs, last value per key winning.
pub fn env_pairs(env: &[String]) -> Vec<(String, String)> {
    let mut out: Vec<(String, String)> = Vec::with_capacity(env.len());
    for e in env {
        let (k, v) = e.split_once('=').unwrap_or((e.as_str(), ""));
        if let Some(slot) = out.iter_mut().find(|(ek, _)| ek == k) {
            slot.1 = v.to_string();
        } else {
            out.push((k.to_string(), v.to_string()));
        }
    }
    out
}

/// Resolve `name` against the agent's own PATH, like exec.LookPath: a name
/// with a slash is used as is; otherwise the first executable regular file
/// on `path` wins. Resolving here (not via the child env) means a request's
/// PATH never changes which binary runs.
pub fn look_path(name: &str, path: &str) -> Result<PathBuf, String> {
    if name.contains('/') {
        return Ok(PathBuf::from(name));
    }
    for dir in path.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let candidate = Path::new(dir).join(name);
        if let Ok(md) = candidate.metadata() {
            if md.is_file() && md.permissions().mode() & 0o111 != 0 {
                return Ok(candidate);
            }
        }
    }
    Err(format!(
        "exec: \"{name}\": executable file not found in $PATH"
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn path_of(env: &[String]) -> String {
        env.iter()
            .filter_map(|e| e.strip_prefix("PATH="))
            .next_back()
            .unwrap_or_default()
            .to_string()
    }
    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    #[test]
    fn default_path_puts_util_linux_before_busybox() {
        let dirs: Vec<_> = DEFAULT_PATH.split(':').collect();
        let idx = |d| dirs.iter().position(|x| *x == d).unwrap();
        assert!(idx("/usr/bin") < idx("/bin"));
        assert!(idx("/usr/sbin") < idx("/sbin"));
    }

    #[test]
    fn injects_default_when_missing() {
        assert_eq!(path_of(&exec_env(&s(&["HOME=/root"]), &[])), DEFAULT_PATH);
    }

    #[test]
    fn injects_default_when_empty() {
        let env = exec_env(&s(&["PATH=", "HOME=/root"]), &[]);
        assert_eq!(path_of(&env), DEFAULT_PATH);
        assert!(!env.contains(&"PATH=".to_string()));
    }

    #[test]
    fn preserves_existing_path() {
        assert_eq!(
            path_of(&exec_env(&s(&["PATH=/custom/bin"]), &[])),
            "/custom/bin"
        );
    }

    #[test]
    fn extra_overrides_path() {
        let env = exec_env(&s(&["PATH=/custom/bin"]), &s(&["PATH=/override/bin"]));
        let pairs = env_pairs(&env);
        assert_eq!(
            pairs.iter().find(|(k, _)| k == "PATH").unwrap().1,
            "/override/bin"
        );
    }

    #[test]
    fn appends_extra() {
        assert!(exec_env(&s(&["PATH=/bin"]), &s(&["FOO=bar"])).contains(&"FOO=bar".to_string()));
    }

    #[test]
    fn look_path_skips_non_executables_and_directories() {
        let root = std::env::temp_dir().join(format!("dew-lookpath-{}", std::process::id()));
        let (a, b) = (root.join("a"), root.join("b"));
        std::fs::create_dir_all(a.join("tool")).unwrap(); // a/tool is a directory
        std::fs::create_dir_all(&b).unwrap();
        std::fs::write(b.join("tool"), "").unwrap(); // b/tool is not executable
        let exe = root.join("c");
        std::fs::create_dir_all(&exe).unwrap();
        std::fs::write(exe.join("tool"), "").unwrap();
        std::fs::set_permissions(exe.join("tool"), std::fs::Permissions::from_mode(0o755)).unwrap();
        let path = format!("{}:{}:{}", a.display(), b.display(), exe.display());
        assert_eq!(look_path("tool", &path).unwrap(), exe.join("tool"));
        assert!(look_path("tool", &format!("{}:{}", a.display(), b.display())).is_err());
        let _ = std::fs::remove_dir_all(root);
    }

    #[test]
    fn look_path_resolves_and_reports_errors() {
        assert_eq!(
            look_path("sh", "/nonexistent:/bin").unwrap(),
            PathBuf::from("/bin/sh")
        );
        assert_eq!(look_path("/x/y", "").unwrap(), PathBuf::from("/x/y"));
        assert_eq!(
            look_path("no-such-cmd-dew", "/bin").unwrap_err(),
            "exec: \"no-such-cmd-dew\": executable file not found in $PATH"
        );
    }
}
