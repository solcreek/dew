//! The identity a confined exec drops to (mirrors lookupUser / lookupGroup /
//! resolveDropID in cmd/dew-agent/caps.go). Pure except for passwd/group
//! file reads.

use crate::gofmt;
use crate::passwd;
use dew_proto::Confinement;

/// Fallback uid for DynamicUser=yes with no concrete User=; the host normally
/// pre-resolves it. Must match confine.DynamicUserUID (nobody).
pub const DYNAMIC_USER_UID: &str = "65534";

/// Where names are looked up (/etc/passwd, /etc/group in the guest).
#[derive(Debug, Clone)]
pub struct Db {
    pub passwd: String,
    pub group: String,
}

impl Default for Db {
    fn default() -> Self {
        Db {
            passwd: "/etc/passwd".into(),
            group: "/etc/group".into(),
        }
    }
}

/// The resolved identity. uid and gid are independent: a Group=-only unit
/// drops the gid (and clears supplementary groups) and leaves the uid.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct IdSpec {
    pub uid: Option<u32>,
    pub gid: Option<u32>,
}

/// An id as the kernel's uid_t/gid_t, checked as Go's checkID does. Negative
/// ids are rejected, and so is anything that isn't a real uid_t:
/// (uid_t)-1 (4294967295) means "leave unchanged" to setresuid, and anything
/// wider wraps (4294967296 becomes 0, root). Both would leave the target as
/// root, so both fail closed.
fn to_id(kind: &str, n: i64) -> Result<u32, String> {
    if n < 0 {
        return Err(format!("negative {kind} {n}"));
    }
    match u32::try_from(n) {
        Ok(v) if v != u32::MAX => Ok(v),
        _ => Err(format!("{kind} {n} out of range")),
    }
}

fn atoi(s: &str) -> Option<i64> {
    // strconv.Atoi: optional sign then digits; i64 covers Go's int.
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse().ok()
}

fn open_error(path: &str, e: &std::io::Error) -> String {
    format!("open {path}: {}", gofmt::errno_text(e))
}

/// A User= spec (numeric uid or name) to (uid, primary gid). A numeric uid
/// with no passwd entry uses gid == uid.
pub fn lookup_user(spec: &str, db: &Db) -> Result<(u32, u32), String> {
    if let Some(n) = atoi(spec) {
        let uid = to_id("uid", n)?;
        let mut gid = uid;
        if let Ok(Some(u)) = passwd::user_by_id(&db.passwd, spec) {
            if let Some(g) = atoi(&u.gid) {
                gid = to_id("gid", g)?;
            }
        }
        return Ok((uid, gid));
    }
    let u = match passwd::user_by_name(&db.passwd, spec) {
        Ok(Some(u)) => u,
        Ok(None) => return Err(format!("user {spec:?}: user: unknown user {spec}")),
        Err(e) => return Err(format!("user {spec:?}: {}", open_error(&db.passwd, &e))),
    };
    let uid = atoi(&u.uid).ok_or_else(|| format!("uid {:?}: invalid syntax", u.uid))?;
    let gid = atoi(&u.gid).ok_or_else(|| format!("gid {:?}: invalid syntax", u.gid))?;
    Ok((to_id("uid", uid)?, to_id("gid", gid)?))
}

/// A Group= spec (numeric gid or name) to its gid.
pub fn lookup_group(spec: &str, db: &Db) -> Result<u32, String> {
    if let Some(n) = atoi(spec) {
        return to_id("gid", n);
    }
    let gid = match passwd::group_by_name(&db.group, spec) {
        Ok(Some(g)) => g,
        Ok(None) => return Err(format!("group {spec:?}: group: unknown group {spec}")),
        Err(e) => return Err(format!("group {spec:?}: {}", open_error(&db.group, &e))),
    };
    let n = atoi(&gid).ok_or_else(|| format!("gid {gid:?}: invalid syntax"))?;
    to_id("gid", n)
}

/// The final identity. The unit's User=, else DynamicUser's fixed uid, else
/// DEW_EXEC_USER, sets uid and its primary gid; Group= then sets or
/// overrides only the gid. Neither: nothing is set (the target keeps root).
pub fn resolve_drop_id(c: &Confinement, exec_user: &str, db: &Db) -> Result<IdSpec, String> {
    let mut id = IdSpec::default();
    let mut name = c.user.as_str();
    if name.is_empty() && c.dynamic_user {
        name = DYNAMIC_USER_UID;
    }
    if name.is_empty() {
        name = exec_user;
    }
    if !name.is_empty() {
        let (uid, gid) = lookup_user(name, db)?;
        id.uid = Some(uid);
        id.gid = Some(gid);
    }
    if !c.group.is_empty() {
        id.gid = Some(lookup_group(&c.group, db)?);
    }
    Ok(id)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn db(tag: &str) -> Db {
        let dir = std::env::temp_dir();
        let p = dir.join(format!("dew-ids-passwd-{tag}-{}", std::process::id()));
        let g = dir.join(format!("dew-ids-group-{tag}-{}", std::process::id()));
        std::fs::write(&p, "root:x:0:0:root:/root:/bin/sh\napp:x:1500:1600::/srv:/bin/sh\nalias:x:1000:4242::/:/bin/sh\n").unwrap();
        std::fs::write(&g, "root:x:0:\nwheel:x:10:root\n").unwrap();
        Db {
            passwd: p.to_str().unwrap().into(),
            group: g.to_str().unwrap().into(),
        }
    }

    fn conf(f: impl FnOnce(&mut Confinement)) -> Confinement {
        let mut c = Confinement::default();
        f(&mut c);
        c
    }

    // Port of TestLookupRejectsNegative.
    #[test]
    fn negative_ids_are_rejected() {
        let d = db("neg");
        assert_eq!(lookup_user("-1", &d), Err("negative uid -1".into()));
        assert_eq!(lookup_group("-1", &d), Err("negative gid -1".into()));
        assert_eq!(lookup_user("0", &d), Ok((0, 0)));
    }

    #[test]
    fn ids_that_would_leave_root_are_rejected() {
        // The kernel would turn both into "still root".
        let d = db("range");
        assert_eq!(
            lookup_user("4294967295", &d),
            Err("uid 4294967295 out of range".into()),
            "(uid_t)-1 = unchanged"
        );
        assert_eq!(
            lookup_user("4294967296", &d),
            Err("uid 4294967296 out of range".into()),
            "would wrap to 0"
        );
        assert_eq!(
            lookup_group("4294967295", &d),
            Err("gid 4294967295 out of range".into())
        );
        assert_eq!(
            lookup_user("4294967294", &d),
            Ok((4294967294, 4294967294)),
            "the largest real id is fine"
        );
    }

    #[test]
    fn numeric_user_takes_the_passwd_primary_gid() {
        let d = db("num");
        assert_eq!(
            lookup_user("1000", &d),
            Ok((1000, 4242)),
            "uid 1000's entry has gid 4242"
        );
        assert_eq!(
            lookup_user("1001", &d),
            Ok((1001, 1001)),
            "no entry: gid = uid"
        );
        assert_eq!(
            lookup_user("+1000", &d),
            Ok((1000, 1000)),
            "LookupId compares strings: '+1000' has no entry"
        );
    }

    #[test]
    fn names_resolve_through_passwd_and_group() {
        let d = db("names");
        assert_eq!(lookup_user("app", &d), Ok((1500, 1600)));
        assert_eq!(
            lookup_user("ghost", &d),
            Err("user \"ghost\": user: unknown user ghost".into())
        );
        assert_eq!(lookup_group("wheel", &d), Ok(10));
        assert_eq!(
            lookup_group("ghost", &d),
            Err("group \"ghost\": group: unknown group ghost".into())
        );
        let missing = Db {
            passwd: "/no/such/passwd".into(),
            group: "/no/such/group".into(),
        };
        assert_eq!(
            lookup_user("app", &missing),
            Err("user \"app\": open /no/such/passwd: no such file or directory".into())
        );
        assert!(lookup_group("wheel", &missing)
            .unwrap_err()
            .starts_with("group \"wheel\": open /no/such/group:"));
    }

    // Port of TestResolveDropID.
    #[test]
    fn drop_identity_plans() {
        let d = db("drop");
        let r = |c: &Confinement, eu: &str| resolve_drop_id(c, eu, &d).unwrap();
        assert_eq!(
            r(&conf(|c| c.user = "1000".into()), ""),
            IdSpec {
                uid: Some(1000),
                gid: Some(4242)
            }
        );
        assert_eq!(
            r(
                &conf(|c| {
                    c.user = "1000".into();
                    c.group = "2000".into()
                }),
                ""
            ),
            IdSpec {
                uid: Some(1000),
                gid: Some(2000)
            },
            "Group= overrides the gid"
        );
        assert_eq!(
            r(&conf(|c| c.group = "2000".into()), ""),
            IdSpec {
                uid: None,
                gid: Some(2000)
            },
            "Group-only keeps the uid"
        );
        assert_eq!(
            r(&conf(|c| c.dynamic_user = true), ""),
            IdSpec {
                uid: Some(65534),
                gid: Some(65534)
            }
        );
        assert_eq!(
            r(&Confinement::default(), "1001"),
            IdSpec {
                uid: Some(1001),
                gid: Some(1001)
            },
            "DEW_EXEC_USER fallback"
        );
        assert_eq!(
            r(&conf(|c| c.user = "1000".into()), "1001").uid,
            Some(1000),
            "unit User= wins"
        );
        assert_eq!(
            r(&conf(|c| c.group = "wheel".into()), "").gid,
            Some(10),
            "group by name"
        );
        assert_eq!(
            r(&Confinement::default(), ""),
            IdSpec::default(),
            "nothing: stay root"
        );
        assert!(
            resolve_drop_id(&conf(|c| c.user = "ghost".into()), "", &d).is_err(),
            "unknown user fails closed"
        );
        assert!(
            resolve_drop_id(&conf(|c| c.group = "ghost".into()), "", &d).is_err(),
            "unknown group fails closed"
        );
    }
}
