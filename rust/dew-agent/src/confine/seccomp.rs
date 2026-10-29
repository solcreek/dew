//! seccomp classic-BPF programs for --confine (for SystemCallFilter=, the
//! program assembly of go-seccomp-bpf v1.6.0). testdata/seccomp holds
//! recorded programs, emitted by the former Go agent; the tests here rebuild
//! every one, so the filters stay instruction for instruction what earlier
//! releases enforced. They are frozen: a deliberate filter change updates
//! them in the same change.

use super::syscalls;
use dew_proto::Confinement;
use std::collections::HashMap;

/// struct sock_filter.
#[repr(C)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SockFilter {
    pub code: u16,
    pub jt: u8,
    pub jf: u8,
    pub k: u32,
}

// linux/filter.h opcodes the programs use.
pub const BPF_LD_W_ABS: u16 = 0x20;
pub const BPF_JMP_JEQ_K: u16 = 0x15;
pub const BPF_JMP_JGE_K: u16 = 0x35;
pub const BPF_JMP_JA: u16 = 0x05;
pub const BPF_RET_K: u16 = 0x06;

// linux/seccomp.h return values and errnos.
pub const SECCOMP_RET_ALLOW: u32 = 0x7fff_0000;
pub const SECCOMP_RET_ERRNO: u32 = 0x0005_0000;
pub const SECCOMP_RET_DATA: u32 = 0x0000_ffff;
const EPERM: u32 = 1;
const ENOSYS: u32 = 38;
/// __X32_SYSCALL_BIT: x32-ABI syscalls on x86_64 have it set.
const X32_SYSCALL_BIT: u32 = 0x4000_0000;

// struct seccomp_data offsets (little-endian; args[0]'s low word at 16).
pub const SC_NR: u32 = 0;
pub const SC_ARCH: u32 = 4;
pub const SC_ARG0: u32 = 16;

/// A guest architecture: its audit arch, syscall table and the numbers the
/// address-family filter matches.
#[derive(Debug)]
pub struct Arch {
    pub name: &'static str,
    pub audit: u32,
    /// Block the x32 ABI (go-seccomp-bpf does this for x86_64 so a denylist
    /// can't be bypassed through x32 syscall numbers).
    pub x32_guard: bool,
    pub syscalls: &'static [(&'static str, u32)],
}

pub const AARCH64: Arch = Arch {
    name: "aarch64",
    audit: 0xc000_00b7,
    x32_guard: false,
    syscalls: syscalls::AARCH64,
};
pub const X86_64: Arch = Arch {
    name: "x86_64",
    audit: 0xc000_003e,
    x32_guard: true,
    syscalls: syscalls::X86_64,
};

impl Arch {
    /// The arch this agent is built for.
    pub fn native() -> Option<&'static Arch> {
        if cfg!(target_arch = "aarch64") {
            Some(&AARCH64)
        } else if cfg!(target_arch = "x86_64") {
            Some(&X86_64)
        } else {
            None
        }
    }

    pub fn by_name(name: &str) -> Option<&'static Arch> {
        [&AARCH64, &X86_64].into_iter().find(|a| a.name == name)
    }

    pub fn syscall(&self, name: &str) -> Option<u32> {
        self.syscalls
            .binary_search_by(|(n, _)| (*n).cmp(name))
            .ok()
            .map(|i| self.syscalls[i].1)
    }
}

/// RestrictAddressFamilies= names to AF_* numbers (arch-independent on
/// Linux). Unknown names fail closed.
const ADDRESS_FAMILIES: &[(&str, u32)] = &[
    ("af_unix", 1),
    ("af_local", 1),
    ("af_inet", 2),
    ("af_inet6", 10),
    ("af_netlink", 16),
    ("af_packet", 17),
    ("af_vsock", 40),
    ("af_unspec", 0),
    ("af_ipx", 4),
    ("af_appletalk", 5),
    ("af_x25", 9),
    ("af_ax25", 3),
    ("af_bluetooth", 31),
    ("af_alg", 38),
    ("af_can", 29),
    ("af_key", 15),
    ("af_nfc", 39),
    ("af_rds", 21),
];

pub fn resolve_address_family(name: &str) -> Result<u32, String> {
    let key = name.trim().to_lowercase();
    ADDRESS_FAMILIES
        .iter()
        .find(|(n, _)| *n == key)
        .map(|(_, v)| *v)
        .ok_or_else(|| format!("unknown address family {name:?}"))
}

/// Resolve and de-duplicate (aliases such as AF_UNIX/AF_LOCAL repeat a
/// number), keeping first-seen order so the program stays minimal.
pub fn resolve_families(names: &[String]) -> Result<Vec<u32>, String> {
    let mut out = Vec::with_capacity(names.len());
    for n in names {
        let f = resolve_address_family(n)?;
        if !out.contains(&f) {
            out.push(f);
        }
    }
    Ok(out)
}

/// The spec carries a seccomp directive the shim applies.
pub fn needs_seccomp(c: &Confinement) -> bool {
    !c.address_families.is_empty() || !c.system_calls.is_empty()
}

fn stmt(code: u16, k: u32) -> SockFilter {
    SockFilter {
        code,
        jt: 0,
        jf: 0,
        k,
    }
}

fn jump(code: u16, k: u32, jt: u8, jf: u8) -> SockFilter {
    SockFilter { code, jt, jf, k }
}

/// The RestrictAddressFamilies= program (socketFamilyFilter): restrict the
/// domain argument of socket(2)/socketpair(2). deny=false allows only the
/// families listed, deny=true blocks them; every other syscall, and every
/// call on a foreign arch, is allowed.
pub fn family_filter(arch: &Arch, families: &[u32], deny: bool) -> Result<Vec<SockFilter>, String> {
    let sys_socket = arch.syscall("socket").ok_or("arch has no socket syscall")?;
    let sys_socketpair = arch
        .syscall("socketpair")
        .ok_or("arch has no socketpair syscall")?;
    let n = families.len();
    // 0 LD arch | 1 JEQ native | 2 LD nr | 3 JEQ socket | 4 JEQ socketpair |
    // 5 LD args[0] | 6..6+n JEQ family | 6+n fallthrough RET | 7+n match RET
    let domain = 5;
    let (fallthrough, matched) = (6 + n, 7 + n);
    let (allow_idx, deny_idx) = if deny {
        (fallthrough, matched)
    } else {
        (matched, fallthrough)
    };
    let off = |from: usize, to: usize| -> Result<u8, String> {
        let d = to as isize - (from as isize + 1);
        u8::try_from(d).map_err(|_| format!("seccomp jump offset {d} out of range"))
    };
    let mut p = Vec::with_capacity(8 + n);
    p.push(stmt(BPF_LD_W_ABS, SC_ARCH));
    p.push(jump(BPF_JMP_JEQ_K, arch.audit, 0, off(1, allow_idx)?));
    p.push(stmt(BPF_LD_W_ABS, SC_NR));
    p.push(jump(BPF_JMP_JEQ_K, sys_socket, off(3, domain)?, 0));
    p.push(jump(
        BPF_JMP_JEQ_K,
        sys_socketpair,
        off(4, domain)?,
        off(4, allow_idx)?,
    ));
    p.push(stmt(BPF_LD_W_ABS, SC_ARG0));
    for (i, fam) in families.iter().enumerate() {
        let target = if deny { deny_idx } else { allow_idx };
        p.push(jump(BPF_JMP_JEQ_K, *fam, off(domain + 1 + i, target)?, 0));
    }
    let allow = stmt(BPF_RET_K, SECCOMP_RET_ALLOW);
    let denied = stmt(BPF_RET_K, SECCOMP_RET_ERRNO | (EPERM & SECCOMP_RET_DATA));
    if deny {
        p.extend([allow, denied]);
    } else {
        p.extend([denied, allow]);
    }
    Ok(p)
}

/// Always allowed in a syscall allowlist so the shim can exec and the target
/// can exit (as systemd adds implicitly).
pub const SYSCALL_IMPLICIT_ALLOW: &[&str] =
    &["execve", "execveat", "exit", "exit_group", "rt_sigreturn"];

/// buildSyscallPolicy: the names to match, normalised (trimmed, lower-case),
/// de-duplicated and limited to the arch's table. Unknown names are dropped
/// so a unit written for another arch still loads, which also means a typo
/// in a denylist silently blocks nothing.
pub fn policy_names(arch: &Arch, names: &[String], deny: bool) -> Vec<String> {
    let mut want: Vec<&str> = names.iter().map(String::as_str).collect();
    if !deny {
        want.extend(SYSCALL_IMPLICIT_ALLOW);
    }
    let mut out: Vec<String> = Vec::new();
    for n in want {
        let n = n.trim().to_lowercase();
        if n.is_empty() || out.contains(&n) || arch.syscall(&n).is_none() {
            continue;
        }
        out.push(n);
    }
    out
}

/// A port of go-seccomp-bpf's Program for the instructions a single-group,
/// condition-free policy uses (conditional JEQ jumps to labels, RETs).
/// Jumps are resolved after the fact; a conditional jump further than 255
/// instructions gets a copy of its RET target inserted within reach, exactly
/// as the Go assembler does, so the output is identical.
struct Program {
    insts: Vec<SockFilter>,
    jumps: Vec<Jump>,
    labels: HashMap<usize, Vec<usize>>,
    next_label: usize,
}

#[derive(Clone, Copy)]
struct Jump {
    index: usize,
    t: usize,
    f: usize,
}

impl Program {
    fn new() -> Program {
        Program {
            insts: Vec::new(),
            jumps: Vec::new(),
            labels: HashMap::new(),
            next_label: 1,
        }
    }

    fn new_label(&mut self) -> usize {
        self.next_label += 1;
        self.next_label
    }

    fn set_label(&mut self, l: usize) {
        let i = self.insts.len();
        self.labels.entry(l).or_default().push(i);
    }

    fn jeq(&mut self, k: u32, t: usize, f: usize) {
        self.jumps.push(Jump {
            index: self.insts.len(),
            t,
            f,
        });
        self.insts.push(jump(BPF_JMP_JEQ_K, k, 0, 0));
    }

    fn jeq_true(&mut self, k: u32, t: usize) {
        let next = self.new_label();
        self.jeq(k, t, next);
        self.set_label(next);
    }

    fn ret(&mut self, action: u32) {
        let k = if action == SECCOMP_RET_ERRNO {
            action | EPERM
        } else {
            action
        };
        self.insts.push(stmt(BPF_RET_K, k));
    }

    fn skip(&self, j: &Jump, label: usize) -> isize {
        self.labels[&label][0] as isize - j.index as isize - 1
    }

    fn resolve(&mut self, j: &Jump, label: usize) -> Result<u8, String> {
        let mut skip = self.skip(j, label);
        while skip < 0 {
            let dest = self.labels.get_mut(&label).unwrap();
            dest.remove(0);
            if dest.is_empty() {
                return Err("backward jumps are not supported".into());
            }
            skip = self.skip(j, label);
        }
        if skip > 255 {
            // The furthest jump still within a short jump of this one: the
            // last in slice order below the limit, as Go's findInsertAfter.
            let max = j.index + 255;
            let after = self
                .jumps
                .iter()
                .rfind(|x| x.index < max)
                .copied()
                .unwrap_or(*j);
            let dest = self.insts[self.labels[&label][0]];
            let inst = if dest.code == BPF_RET_K {
                dest
            } else {
                // Not reachable for these programs (labels only target RETs);
                // mirrors the Go assembler's long jump for completeness.
                stmt(BPF_JMP_JA, (skip - after.index as isize) as u32)
            };
            let at = after.index + 1;
            self.insts.insert(at, inst);
            for x in &mut self.jumps {
                if x.index >= at {
                    x.index += 1;
                }
            }
            for v in self.labels.values_mut() {
                for p in v.iter_mut() {
                    if *p >= at {
                        *p += 1;
                    }
                }
            }
            self.labels.get_mut(&label).unwrap().insert(0, at);
            skip = self.skip(j, label);
        }
        Ok(skip as u8)
    }

    fn assemble(mut self) -> Result<Vec<SockFilter>, String> {
        for i in 0..self.jumps.len() {
            // A copy taken now, as Go's range loop does: indices updated by
            // later insertions are seen by later iterations, not this one.
            let j = self.jumps[i];
            let t = self.resolve(&j, j.t)?;
            let f = self.resolve(&j, j.f)?;
            if t == 0 && f == 0 {
                return Err("useless jump found".into());
            }
            self.insts[j.index].jt = t;
            self.insts[j.index].jf = f;
        }
        Ok(self.insts)
    }
}

/// The SystemCallFilter= program (assembled as go-seccomp-bpf does).
/// Denylist: default allow, listed syscalls return EPERM. Allowlist: default
/// EPERM, listed (plus the implicit exec/exit set) allowed. A foreign arch
/// takes the default action; on x86_64 the x32 ABI returns ENOSYS.
pub fn syscall_filter(
    arch: &Arch,
    names: &[String],
    deny: bool,
) -> Result<Vec<SockFilter>, String> {
    let (default, action) = if deny {
        (SECCOMP_RET_ALLOW, SECCOMP_RET_ERRNO)
    } else {
        (SECCOMP_RET_ERRNO, SECCOMP_RET_ALLOW)
    };
    let nums: Vec<u32> = policy_names(arch, names, deny)
        .iter()
        .filter_map(|n| arch.syscall(n))
        .collect();

    let mut p = Program::new();
    if !nums.is_empty() {
        let (act, next_group) = (p.new_label(), p.new_label());
        for (i, nr) in nums.iter().enumerate() {
            if i < nums.len() - 1 {
                p.jeq_true(*nr, act);
            } else {
                p.jeq(*nr, act, next_group);
            }
        }
        p.set_label(act);
        p.ret(action);
        p.set_label(next_group);
    }
    p.ret(default);
    let body = p.assemble()?;

    let x32: Vec<SockFilter> = if arch.x32_guard {
        vec![
            jump(BPF_JMP_JGE_K, X32_SYSCALL_BIT, 0, 1),
            stmt(BPF_RET_K, SECCOMP_RET_ERRNO | ENOSYS),
        ]
    } else {
        Vec::new()
    };
    let to_default = x32.len() + body.len();
    let mut prog = vec![stmt(BPF_LD_W_ABS, SC_ARCH)];
    if to_default <= 255 {
        prog.push(jump(BPF_JMP_JEQ_K, arch.audit, 0, to_default as u8));
    } else {
        prog.push(jump(BPF_JMP_JEQ_K, arch.audit, 1, 0));
        prog.push(stmt(BPF_JMP_JA, to_default as u32));
    }
    prog.push(stmt(BPF_LD_W_ABS, SC_NR));
    prog.extend(x32);
    prog.extend(body);
    Ok(prog)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    /// A classic-BPF interpreter for the opcodes these programs use; returns
    /// the SECCOMP_RET_* for a synthetic seccomp_data.
    fn run(prog: &[SockFilter], nr: u32, arch: u32, arg0: u32) -> u32 {
        let load = |off: u32| match off {
            SC_NR => nr,
            SC_ARCH => arch,
            SC_ARG0 => arg0,
            o => panic!("unexpected load offset {o}"),
        };
        let (mut a, mut pc) = (0u32, 0usize);
        loop {
            let i = prog
                .get(pc)
                .unwrap_or_else(|| panic!("ran off the end at {pc}"));
            match i.code {
                BPF_LD_W_ABS => {
                    a = load(i.k);
                    pc += 1
                }
                BPF_JMP_JEQ_K => pc += 1 + if a == i.k { i.jt } else { i.jf } as usize,
                BPF_JMP_JGE_K => pc += 1 + if a >= i.k { i.jt } else { i.jf } as usize,
                BPF_JMP_JA => pc += 1 + i.k as usize,
                BPF_RET_K => return i.k,
                c => panic!("unexpected opcode {c:#x} at {pc}"),
            }
        }
    }

    const EPERM_RET: u32 = SECCOMP_RET_ERRNO | EPERM;

    #[test]
    fn address_families_resolve() {
        assert_eq!(resolve_address_family("AF_INET"), Ok(2));
        assert_eq!(resolve_address_family("  af_unix "), Ok(1));
        assert!(resolve_address_family("AF_BOGUS").is_err());
        assert_eq!(
            resolve_families(&s(&["AF_INET", "AF_UNIX", "AF_LOCAL", "AF_INET"])),
            Ok(vec![2, 1])
        );
        assert!(resolve_families(&s(&["AF_INET", "AF_BOGUS"])).is_err());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn address_family_numbers_match_libc() {
        let want = [
            ("af_unix", libc::AF_UNIX),
            ("af_inet", libc::AF_INET),
            ("af_inet6", libc::AF_INET6),
            ("af_netlink", libc::AF_NETLINK),
            ("af_packet", libc::AF_PACKET),
            ("af_vsock", libc::AF_VSOCK),
            ("af_alg", libc::AF_ALG),
            ("af_can", libc::AF_CAN),
            ("af_bluetooth", libc::AF_BLUETOOTH),
        ];
        for (n, v) in want {
            assert_eq!(resolve_address_family(n), Ok(v as u32), "{n}");
        }
    }

    // Checked on both real arches.
    #[test]
    fn family_filter_behaves() {
        for arch in [&AARCH64, &X86_64] {
            let sock = arch.syscall("socket").unwrap();
            let pair = arch.syscall("socketpair").unwrap();
            let read = arch.syscall("read").unwrap();
            let p = family_filter(arch, &[1], false).unwrap();
            assert_eq!(run(&p, sock, arch.audit, 1), SECCOMP_RET_ALLOW);
            assert_eq!(run(&p, sock, arch.audit, 2), EPERM_RET);
            assert_eq!(run(&p, pair, arch.audit, 2), EPERM_RET);
            assert_eq!(
                run(&p, read, arch.audit, 2),
                SECCOMP_RET_ALLOW,
                "other syscalls untouched"
            );
            assert_eq!(
                run(&p, sock, 0xbb, 2),
                SECCOMP_RET_ALLOW,
                "foreign arch passes"
            );

            let p = family_filter(arch, &[2], true).unwrap();
            assert_eq!(run(&p, sock, arch.audit, 2), EPERM_RET);
            assert_eq!(run(&p, sock, arch.audit, 1), SECCOMP_RET_ALLOW);

            let p = family_filter(arch, &[1, 2], false).unwrap();
            assert_eq!(run(&p, sock, arch.audit, 1), SECCOMP_RET_ALLOW);
            assert_eq!(run(&p, sock, arch.audit, 2), SECCOMP_RET_ALLOW);
            assert_eq!(run(&p, sock, arch.audit, 16), EPERM_RET);
        }
        let too_many: Vec<u32> = (0..300).collect();
        assert!(
            family_filter(&AARCH64, &too_many, false).is_err(),
            "u8 jump overflow is an error"
        );
    }

    #[test]
    fn policy_names_are_normalised_and_arch_filtered() {
        let deny = policy_names(&X86_64, &s(&["MKDIR", "open", "not_a_syscall"]), true);
        assert_eq!(deny, s(&["mkdir", "open"]));
        let deny = policy_names(&AARCH64, &s(&["MKDIR", "mkdirat"]), true);
        assert_eq!(deny, s(&["mkdirat"]), "aarch64 has no mkdir: dropped");
        let allow = policy_names(&X86_64, &s(&["read"]), false);
        for must in ["read", "execve", "exit_group", "rt_sigreturn"] {
            assert!(allow.contains(&must.to_string()), "{must}");
        }
    }

    #[test]
    fn syscall_filter_behaves() {
        let nr = |a: &Arch, n: &str| a.syscall(n).unwrap();
        for arch in [&AARCH64, &X86_64] {
            let p = syscall_filter(arch, &s(&["mkdirat", "ptrace"]), true).unwrap();
            assert_eq!(run(&p, nr(arch, "mkdirat"), arch.audit, 0), EPERM_RET);
            assert_eq!(run(&p, nr(arch, "ptrace"), arch.audit, 0), EPERM_RET);
            assert_eq!(run(&p, nr(arch, "read"), arch.audit, 0), SECCOMP_RET_ALLOW);
            assert_eq!(
                run(&p, nr(arch, "mkdirat"), 0xbb, 0),
                SECCOMP_RET_ALLOW,
                "foreign arch: default"
            );

            let p = syscall_filter(arch, &s(&["read"]), false).unwrap();
            assert_eq!(run(&p, nr(arch, "read"), arch.audit, 0), SECCOMP_RET_ALLOW);
            assert_eq!(
                run(&p, nr(arch, "execve"), arch.audit, 0),
                SECCOMP_RET_ALLOW,
                "implicit"
            );
            assert_eq!(run(&p, nr(arch, "mkdirat"), arch.audit, 0), EPERM_RET);
            assert_eq!(
                run(&p, nr(arch, "read"), 0xbb, 0),
                EPERM_RET,
                "foreign arch: default deny"
            );

            // Whole table: every listed syscall hits the action through the
            // long-jump path, and nothing else does.
            let all: Vec<String> = arch.syscalls.iter().map(|(n, _)| n.to_string()).collect();
            let p = syscall_filter(arch, &all, true).unwrap();
            assert!(p.len() > 256);
            for (_, num) in arch.syscalls {
                assert_eq!(run(&p, *num, arch.audit, 0), EPERM_RET);
            }
            assert_eq!(run(&p, 100_000, arch.audit, 0), SECCOMP_RET_ALLOW);
        }
        let p = syscall_filter(&X86_64, &s(&["read"]), true).unwrap();
        assert_eq!(
            run(&p, 0x4000_0000, X86_64.audit, 0),
            SECCOMP_RET_ERRNO | ENOSYS,
            "x32 ABI blocked"
        );
    }

    fn golden_dir() -> std::path::PathBuf {
        std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("testdata/seccomp")
    }

    /// Every recorded program (both arches) is rebuilt from its recorded
    /// inputs and must be identical.
    #[test]
    fn matches_recorded_golden_programs() {
        let mut checked = 0;
        for e in std::fs::read_dir(golden_dir()).expect("seccomp goldens") {
            let path = e.unwrap().path();
            let g: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
            let arch = Arch::by_name(g["arch"].as_str().unwrap()).unwrap();
            let names: Vec<String> = serde_json::from_value(g["names"].clone()).unwrap();
            let deny = g["deny"].as_bool().unwrap();
            let got = match g["kind"].as_str().unwrap() {
                "syscalls" => syscall_filter(arch, &names, deny),
                "families" => family_filter(arch, &resolve_families(&names).unwrap(), deny),
                k => panic!("unknown kind {k}"),
            }
            .unwrap();
            let want: Vec<SockFilter> = g["program"]
                .as_array()
                .unwrap()
                .iter()
                .map(|i| {
                    let v: Vec<u32> = serde_json::from_value(i.clone()).unwrap();
                    SockFilter {
                        code: v[0] as u16,
                        jt: v[1] as u8,
                        jf: v[2] as u8,
                        k: v[3],
                    }
                })
                .collect();
            assert_eq!(
                got,
                want,
                "{} differs from the recorded program",
                path.display()
            );
            checked += 1;
        }
        assert_eq!(checked, 20, "expected 10 cases x 2 arches");
    }

    #[test]
    fn tables_are_sorted_and_the_natives_resolve() {
        for arch in [&AARCH64, &X86_64] {
            assert!(
                arch.syscalls.windows(2).all(|w| w[0].0 < w[1].0),
                "{} table sorted for binary search",
                arch.name
            );
        }
        assert_eq!(AARCH64.syscall("socket"), Some(198));
        assert_eq!(X86_64.syscall("socket"), Some(41));
        assert_eq!(AARCH64.syscall("mkdir"), None);
        assert!(Arch::native().is_some());
        assert!(Arch::by_name("riscv64").is_none());
    }
}
