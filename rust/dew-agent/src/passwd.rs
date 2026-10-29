//! /etc/passwd and /etc/group lookups with the parsing rules of Go's pure
//! (cgo-free) os/user, which earlier releases' agent used, so names keep
//! resolving to the same ids: lines are trimmed, blank lines and `#`
//! comments skipped, names starting with `+`/`-` ignored, numeric id fields
//! required, and the first matching line wins.

use std::io;

/// A passwd entry's name and its uid/gid fields as written.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct User {
    pub name: String,
    pub uid: String,
    pub gid: String,
}

fn lines(path: &str) -> io::Result<Vec<String>> {
    Ok(std::fs::read_to_string(path)?
        .lines()
        .map(|l| l.trim().to_string())
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .collect())
}

fn numeric(s: &str) -> bool {
    // strconv.Atoi: optional sign, then decimal digits.
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    !digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit())
}

fn usable_name(n: &str) -> bool {
    !n.is_empty() && !n.starts_with(['+', '-'])
}

/// The first passwd entry whose field `idx` (0 = name, 2 = uid) equals
/// `value`. Needs at least six colons and numeric uid/gid, as os/user.
fn find_user(path: &str, value: &str, idx: usize) -> io::Result<Option<User>> {
    for line in lines(path)? {
        if line.matches(':').count() < 6 {
            continue;
        }
        let parts: Vec<&str> = line.splitn(7, ':').collect();
        if parts.len() < 6 || parts[idx] != value || !usable_name(parts[0]) {
            continue;
        }
        if !numeric(parts[2]) || !numeric(parts[3]) {
            continue;
        }
        return Ok(Some(User {
            name: parts[0].into(),
            uid: parts[2].into(),
            gid: parts[3].into(),
        }));
    }
    Ok(None)
}

/// user.Lookup: by user name.
pub fn user_by_name(path: &str, name: &str) -> io::Result<Option<User>> {
    find_user(path, name, 0)
}

/// user.LookupId: by the uid field, compared as a string ("010" ≠ "10").
pub fn user_by_id(path: &str, uid: &str) -> io::Result<Option<User>> {
    find_user(path, uid, 2)
}

/// user.LookupGroup: a group name to its gid field. Needs at least three
/// colons and a numeric gid.
pub fn group_by_name(path: &str, name: &str) -> io::Result<Option<String>> {
    for line in lines(path)? {
        if line.matches(':').count() < 3 {
            continue;
        }
        let parts: Vec<&str> = line.splitn(4, ':').collect();
        if parts.len() < 4 || parts[0] != name || !usable_name(parts[0]) || !numeric(parts[2]) {
            continue;
        }
        return Ok(Some(parts[2].into()));
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(tag: &str, body: &str) -> String {
        let p = std::env::temp_dir().join(format!("dew-passwd-{tag}-{}", std::process::id()));
        std::fs::write(&p, body).unwrap();
        p.to_str().unwrap().to_string()
    }

    const PASSWD: &str = "\
# comment line

root:x:0:0:root:/root:/bin/sh
  dew:x:1000:1001:Dew,,,:/home/dew:/bin/sh
short:x:7:7
+nis:x:5:5::/:/bin/sh
-neg:x:6:6::/:/bin/sh
badid:x:abc:1::/:/bin/sh
badgid:x:9:xyz::/:/bin/sh
dup:x:2000:2000::/:/bin/sh
dup:x:3000:3000::/:/bin/sh
zero:x:010:10::/:/bin/sh
";

    #[test]
    fn user_lookups_follow_go_os_user() {
        let p = file("u", PASSWD);
        let u = |n| user_by_name(&p, n).unwrap();
        assert_eq!(u("root").unwrap().uid, "0");
        assert_eq!(
            u("dew"),
            Some(User {
                name: "dew".into(),
                uid: "1000".into(),
                gid: "1001".into()
            }),
            "trimmed line"
        );
        assert_eq!(u("short"), None, "fewer than six colons");
        assert_eq!(u("+nis"), None, "+name ignored");
        assert_eq!(u("-neg"), None, "-name ignored");
        assert_eq!(u("badid"), None, "non-numeric uid");
        assert_eq!(u("badgid"), None, "non-numeric gid");
        assert_eq!(u("dup").unwrap().uid, "2000", "first match wins");
        assert_eq!(u("nobody-here"), None);
        assert_eq!(user_by_id(&p, "1000").unwrap().unwrap().name, "dew");
        assert_eq!(
            user_by_id(&p, "10").unwrap(),
            None,
            "uid compared as a string"
        );
        assert_eq!(user_by_id(&p, "010").unwrap().unwrap().name, "zero");
        let _ = std::fs::remove_file(&p);
        assert!(user_by_name("/no/such/passwd", "root").is_err());
    }

    #[test]
    fn group_lookups_follow_go_os_user() {
        let p = file(
            "g",
            "# c\nwheel:x:10:root\n  staff:x:50:  \nshort:x:7\nbad:x:nope:\n+plus:x:1:\n",
        );
        let g = |n| group_by_name(&p, n).unwrap();
        assert_eq!(g("wheel"), Some("10".into()));
        assert_eq!(g("staff"), Some("50".into()), "trimmed, empty member list");
        assert_eq!(g("short"), None, "fewer than three colons");
        assert_eq!(g("bad"), None, "non-numeric gid");
        assert_eq!(g("+plus"), None);
        let _ = std::fs::remove_file(&p);
        assert!(group_by_name("/no/such/group", "wheel").is_err());
    }

    #[test]
    fn numeric_is_atoi_shaped() {
        assert!(numeric("0") && numeric("+5") && numeric("-1") && numeric("123"));
        assert!(!numeric("") && !numeric("+") && !numeric("1a") && !numeric(" 1"));
    }
}
