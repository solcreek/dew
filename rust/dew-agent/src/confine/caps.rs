//! Capability names and the bounding-set plan (mirrors the pure half of
//! the retired Go agent's caps.go). No syscalls here; `apply` performs the drop.

use dew_proto::Confinement;

/// libcap names (as confine.Plan emits them) to kernel capability numbers.
/// The numbers are ABI-stable, so they are keyed directly.
const CAP_BY_NAME: &[(&str, u32)] = &[
    ("cap_chown", 0),
    ("cap_dac_override", 1),
    ("cap_dac_read_search", 2),
    ("cap_fowner", 3),
    ("cap_fsetid", 4),
    ("cap_kill", 5),
    ("cap_setgid", 6),
    ("cap_setuid", 7),
    ("cap_setpcap", 8),
    ("cap_linux_immutable", 9),
    ("cap_net_bind_service", 10),
    ("cap_net_broadcast", 11),
    ("cap_net_admin", 12),
    ("cap_net_raw", 13),
    ("cap_ipc_lock", 14),
    ("cap_ipc_owner", 15),
    ("cap_sys_module", 16),
    ("cap_sys_rawio", 17),
    ("cap_sys_chroot", 18),
    ("cap_sys_ptrace", 19),
    ("cap_sys_pacct", 20),
    ("cap_sys_admin", 21),
    ("cap_sys_boot", 22),
    ("cap_sys_nice", 23),
    ("cap_sys_resource", 24),
    ("cap_sys_time", 25),
    ("cap_sys_tty_config", 26),
    ("cap_mknod", 27),
    ("cap_lease", 28),
    ("cap_audit_write", 29),
    ("cap_audit_control", 30),
    ("cap_setfcap", 31),
    ("cap_mac_override", 32),
    ("cap_mac_admin", 33),
    ("cap_syslog", 34),
    ("cap_wake_alarm", 35),
    ("cap_block_suspend", 36),
    ("cap_audit_read", 37),
    ("cap_perfmon", 38),
    ("cap_bpf", 39),
    ("cap_checkpoint_restore", 40),
];

/// The build-time highest capability (golang.org/x/sys CAP_LAST_CAP), used
/// when /proc/sys/kernel/cap_last_cap can't be read.
pub const CAP_LAST_CAP_FALLBACK: u32 = 40;

/// A libcap name to its capability number, case-insensitively. Unknown names
/// fail closed.
pub fn resolve_cap(name: &str) -> Result<u32, String> {
    let lower = name.to_lowercase();
    CAP_BY_NAME
        .iter()
        .find(|(n, _)| *n == lower)
        .map(|(_, v)| *v)
        .ok_or_else(|| format!("unknown capability {name:?}"))
}

/// The highest capability the running kernel knows, so the bounding-set
/// sweep never PR_CAPBSET_DROPs a number it rejects with EINVAL.
pub fn last_capability(proc_file: &str) -> u32 {
    std::fs::read_to_string(proc_file)
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(CAP_LAST_CAP_FALLBACK)
}

/// Capabilities to remove from the bounding set, within `last_cap`.
/// DropAllCaps: every cap except KeepCaps. Otherwise only DropCaps, skipping
/// any the kernel lacks (dropping those is a no-op that would EINVAL).
pub fn caps_to_drop(c: &Confinement, last_cap: u32) -> Result<Vec<u32>, String> {
    if c.drop_all_caps {
        let keep = c
            .keep_caps
            .iter()
            .map(|n| resolve_cap(n))
            .collect::<Result<Vec<_>, _>>()?;
        return Ok((0..=last_cap).filter(|i| !keep.contains(i)).collect());
    }
    let mut drop = Vec::new();
    for n in &c.drop_caps {
        let id = resolve_cap(n)?;
        if id <= last_cap {
            drop.push(id);
        }
    }
    Ok(drop)
}

/// KeepCaps as numbers (only meaningful with DropAllCaps), skipping caps the
/// kernel lacks: those can't be made inheritable or ambient (EINVAL).
pub fn kept_caps(c: &Confinement, last_cap: u32) -> Result<Vec<u32>, String> {
    if !c.drop_all_caps {
        return Ok(Vec::new());
    }
    let mut keep = Vec::new();
    for n in &c.keep_caps {
        let id = resolve_cap(n)?;
        if id <= last_cap {
            keep.push(id);
        }
    }
    Ok(keep)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    // Port of TestResolveCap.
    #[test]
    fn resolve_cap_by_name() {
        assert_eq!(resolve_cap("cap_net_bind_service"), Ok(10));
        assert_eq!(resolve_cap("CAP_SYS_ADMIN"), Ok(21), "case-insensitive");
        assert_eq!(
            resolve_cap("cap_not_a_real_cap"),
            Err("unknown capability \"cap_not_a_real_cap\"".into())
        );
    }

    #[test]
    fn cap_table_is_dense_and_ordered() {
        // Every number 0..=40 appears exactly once, in order: a typo'd or
        // duplicated entry would silently mis-drop a capability.
        let nums: Vec<u32> = CAP_BY_NAME.iter().map(|(_, n)| *n).collect();
        assert_eq!(nums, (0..=CAP_LAST_CAP_FALLBACK).collect::<Vec<_>>());
    }

    // Port of TestCapsToDrop.
    #[test]
    fn caps_to_drop_plans() {
        let c = Confinement {
            drop_all_caps: true,
            keep_caps: s(&["cap_net_bind_service", "cap_chown"]),
            ..Default::default()
        };
        assert_eq!(
            caps_to_drop(&c, 12).unwrap(),
            vec![1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12]
        );

        let c = Confinement {
            drop_all_caps: true,
            ..Default::default()
        };
        assert_eq!(caps_to_drop(&c, 2).unwrap(), vec![0, 1, 2]);

        let c = Confinement {
            drop_caps: s(&["cap_sys_admin", "cap_net_raw"]),
            ..Default::default()
        };
        assert_eq!(
            caps_to_drop(&c, 40).unwrap(),
            vec![21, 13],
            "named caps, in order"
        );

        let c = Confinement {
            drop_caps: s(&["cap_sys_admin", "cap_checkpoint_restore"]),
            ..Default::default()
        };
        assert_eq!(
            caps_to_drop(&c, 39).unwrap(),
            vec![21],
            "40 is past the kernel's last cap"
        );

        let c = Confinement {
            drop_all_caps: true,
            keep_caps: s(&["cap_bogus"]),
            ..Default::default()
        };
        assert!(
            caps_to_drop(&c, 40).is_err(),
            "unknown KeepCaps fails closed"
        );

        let c = Confinement {
            drop_caps: s(&["cap_bogus"]),
            ..Default::default()
        };
        assert!(
            caps_to_drop(&c, 40).is_err(),
            "unknown DropCaps fails closed"
        );

        assert!(caps_to_drop(&Confinement::default(), 40)
            .unwrap()
            .is_empty());
    }

    // Port of TestKeptCaps.
    #[test]
    fn kept_caps_plans() {
        let c = Confinement {
            drop_all_caps: true,
            keep_caps: s(&["cap_net_bind_service"]),
            ..Default::default()
        };
        assert_eq!(kept_caps(&c, 40).unwrap(), vec![10]);

        let c = Confinement {
            keep_caps: s(&["cap_chown"]),
            ..Default::default()
        };
        assert!(
            kept_caps(&c, 40).unwrap().is_empty(),
            "only with DropAllCaps"
        );

        let c = Confinement {
            drop_all_caps: true,
            keep_caps: s(&["cap_chown", "cap_checkpoint_restore"]),
            ..Default::default()
        };
        assert_eq!(kept_caps(&c, 39).unwrap(), vec![0], "40 skipped");

        let c = Confinement {
            drop_all_caps: true,
            keep_caps: s(&["nope"]),
            ..Default::default()
        };
        assert!(kept_caps(&c, 40).is_err());
    }

    #[test]
    fn last_capability_reads_proc_or_falls_back() {
        let p = std::env::temp_dir().join(format!("dew-caplast-{}", std::process::id()));
        std::fs::write(&p, "38\n").unwrap();
        assert_eq!(last_capability(p.to_str().unwrap()), 38);
        std::fs::write(&p, "garbage").unwrap();
        assert_eq!(last_capability(p.to_str().unwrap()), CAP_LAST_CAP_FALLBACK);
        let _ = std::fs::remove_file(&p);
        assert_eq!(
            last_capability("/no/such/cap_last_cap"),
            CAP_LAST_CAP_FALLBACK
        );
    }
}
