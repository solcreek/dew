//go:build linux

package main

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/elastic/go-seccomp-bpf/arch"
	protocol "github.com/solcreek/dew/internal/vsock"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// addressFamilyByName maps systemd RestrictAddressFamilies= names to their AF_*
// numbers. The values are arch-independent on Linux. Unknown names fail closed
// (resolveAddressFamily errors) rather than silently widening the filter.
var addressFamilyByName = map[string]int{
	"af_unix":      unix.AF_UNIX, // == AF_LOCAL
	"af_local":     unix.AF_LOCAL,
	"af_inet":      unix.AF_INET,
	"af_inet6":     unix.AF_INET6,
	"af_netlink":   unix.AF_NETLINK,
	"af_packet":    unix.AF_PACKET,
	"af_vsock":     unix.AF_VSOCK,
	"af_unspec":    unix.AF_UNSPEC,
	"af_ipx":       unix.AF_IPX,
	"af_appletalk": unix.AF_APPLETALK,
	"af_x25":       unix.AF_X25,
	"af_ax25":      unix.AF_AX25,
	"af_bluetooth": unix.AF_BLUETOOTH,
	"af_alg":       unix.AF_ALG,
	"af_can":       unix.AF_CAN,
	"af_key":       unix.AF_KEY,
	"af_nfc":       unix.AF_NFC,
	"af_rds":       unix.AF_RDS,
}

// resolveFamilies resolves the AF_* names to numbers, de-duplicating (drop-ins
// and aliases like AF_UNIX+AF_LOCAL can repeat a number) while preserving
// first-seen order, so the generated BPF stays minimal and the u8 jump offsets
// don't overflow for a representable config.
func resolveFamilies(names []string) ([]uint32, error) {
	fams := make([]uint32, 0, len(names))
	seen := make(map[uint32]bool, len(names))
	for _, name := range names {
		fam, err := resolveAddressFamily(name)
		if err != nil {
			return nil, err
		}
		if seen[fam] {
			continue
		}
		seen[fam] = true
		fams = append(fams, fam)
	}
	return fams, nil
}

func resolveAddressFamily(name string) (uint32, error) {
	if v, ok := addressFamilyByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return uint32(v), nil
	}
	return 0, fmt.Errorf("unknown address family %q", name)
}

// needsSeccomp reports whether the spec carries a seccomp directive the shim
// applies: RestrictAddressFamilies= and/or explicit SystemCallFilter= names.
func needsSeccomp(c *protocol.Confinement) bool {
	return len(c.AddressFamilies) > 0 || len(c.SystemCalls) > 0
}

// syscallImplicitAllow is the minimal set always permitted in an allowlist so
// the shim can exec and the target can terminate, mirroring systemd's implicit
// additions. An explicit allowlist is rarely runtime-complete without @-groups
// (5c); this just keeps exec/exit working.
var syscallImplicitAllow = []string{"execve", "execveat", "exit", "exit_group", "rt_sigreturn"}

// buildSyscallPolicy turns the unit's SystemCallFilter= names into a
// go-seccomp-bpf policy. Names not in this arch's table are dropped so a unit
// written for another arch still loads instead of failing closed; the same
// drop, however, also silently discards misspelled/unknown names, which for a
// denylist can weaken the intended policy (a mistyped blocked syscall just isn't
// blocked). Denylist → default-allow with the listed names returning EPERM;
// allowlist → default-EPERM with the listed names (plus the implicit exec/exit
// set) allowed. known is the arch's syscall-name set (arch.Info.SyscallNames).
func buildSyscallPolicy(names []string, deny bool, known map[string]int) seccomp.Policy {
	want := names
	if !deny {
		want = append(append([]string{}, names...), syscallImplicitAllow...)
	}
	seen := map[string]bool{}
	var filtered []string
	for _, n := range want {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		if _, ok := known[n]; !ok {
			continue // not on this arch → unreachable, skip
		}
		seen[n] = true
		filtered = append(filtered, n)
	}
	pol := seccomp.Policy{}
	if deny {
		pol.DefaultAction = seccomp.ActionAllow
		pol.Syscalls = []seccomp.SyscallGroup{{Action: seccomp.ActionErrno, Names: filtered}}
	} else {
		pol.DefaultAction = seccomp.ActionErrno
		pol.Syscalls = []seccomp.SyscallGroup{{Action: seccomp.ActionAllow, Names: filtered}}
	}
	return pol
}

// syscallFilter assembles the SystemCallFilter= policy into a classic-BPF
// program. The policy's arch is the agent's build arch; foreign-arch syscalls
// (e.g. x86_64 under Rosetta) take the default action — allowed for a denylist,
// blocked for an allowlist.
func syscallFilter(names []string, deny bool) ([]unix.SockFilter, error) {
	info, err := arch.GetInfo("")
	if err != nil {
		return nil, fmt.Errorf("seccomp arch: %w", err)
	}
	pol := buildSyscallPolicy(names, deny, info.SyscallNames)
	insts, err := pol.Assemble()
	if err != nil {
		return nil, fmt.Errorf("assemble syscall policy: %w", err)
	}
	raw, err := bpf.Assemble(insts)
	if err != nil {
		return nil, fmt.Errorf("assemble bpf: %w", err)
	}
	prog := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		prog[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	return prog, nil
}

// Classic-BPF opcodes used by the seccomp program (linux/filter.h).
const (
	bpfLdAbsW = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K
)

// seccomp_data field offsets (little-endian: args are u64, the low 32 bits of
// args[0] sit at offset 16).
const (
	scDataNr   = 0
	scDataArch = 4
	scDataArg0 = 16
)

// socketFamilyFilter builds a seccomp classic-BPF program restricting the domain
// argument of socket(2)/socketpair(2). deny=false → only the listed families are
// allowed (others EPERM); deny=true → the listed families are blocked (others
// allowed). Every other syscall is allowed. Calls on a non-native arch (an
// x86_64 binary under Rosetta when the agent is arm64) are allowed through — the
// program is installed for the agent's build arch only. Returns an error if a
// jump offset would exceed the BPF u8 range (only with an implausibly long
// family list).
func socketFamilyFilter(nativeArch, sysSocket, sysSocketpair uint32, families []uint32, deny bool) ([]unix.SockFilter, error) {
	n := len(families)
	// Index layout:
	//   0 LD arch
	//   1 JEQ nativeArch        (mismatch → ALLOW)
	//   2 LD nr
	//   3 JEQ socket            (→ DOMAIN)
	//   4 JEQ socketpair        (→ DOMAIN, else ALLOW)
	//   5 LD args[0] (DOMAIN)
	//   6..6+n-1 JEQ family
	//   6+n   first terminal (fallthrough target)
	//   7+n   second terminal (match target)
	const domainIdx = 5
	fallthroughIdx := 6 + n
	matchIdx := 7 + n

	// allow mode: a family match → ALLOW, fallthrough → DENY.
	// deny mode:  a family match → DENY,  fallthrough → ALLOW.
	allowIdx, denyIdx := matchIdx, fallthroughIdx
	if deny {
		allowIdx, denyIdx = fallthroughIdx, matchIdx
	}

	// off computes a BPF jump distance (instructions to skip from the one after
	// the jump) and verifies it fits in the u8 Jt/Jf fields.
	off := func(from, to int) (uint8, error) {
		d := to - (from + 1)
		if d < 0 || d > 255 {
			return 0, fmt.Errorf("seccomp jump offset %d out of range", d)
		}
		return uint8(d), nil
	}

	prog := make([]unix.SockFilter, 0, 8+n)
	emit := func(f unix.SockFilter) { prog = append(prog, f) }

	emit(unix.SockFilter{Code: bpfLdAbsW, K: scDataArch})
	archJf, err := off(1, allowIdx)
	if err != nil {
		return nil, err
	}
	emit(unix.SockFilter{Code: bpfJeqK, Jt: 0, Jf: archJf, K: nativeArch})

	emit(unix.SockFilter{Code: bpfLdAbsW, K: scDataNr})
	sockJt, err := off(3, domainIdx)
	if err != nil {
		return nil, err
	}
	emit(unix.SockFilter{Code: bpfJeqK, Jt: sockJt, Jf: 0, K: sysSocket})
	spJt, err := off(4, domainIdx)
	if err != nil {
		return nil, err
	}
	spJf, err := off(4, allowIdx)
	if err != nil {
		return nil, err
	}
	emit(unix.SockFilter{Code: bpfJeqK, Jt: spJt, Jf: spJf, K: sysSocketpair})

	emit(unix.SockFilter{Code: bpfLdAbsW, K: scDataArg0})
	for i, fam := range families {
		idx := domainIdx + 1 + i
		target := allowIdx
		if deny {
			target = denyIdx
		}
		jt, err := off(idx, target)
		if err != nil {
			return nil, err
		}
		emit(unix.SockFilter{Code: bpfJeqK, Jt: jt, Jf: 0, K: fam})
	}

	// Terminals, in (fallthrough, match) order so the family fallthrough lands
	// on the correct default.
	deniedK := uint32(unix.SECCOMP_RET_ERRNO) | (uint32(unix.EPERM) & uint32(unix.SECCOMP_RET_DATA))
	allowF := unix.SockFilter{Code: bpfRetK, K: uint32(unix.SECCOMP_RET_ALLOW)}
	denyF := unix.SockFilter{Code: bpfRetK, K: deniedK}
	if deny {
		emit(allowF)
		emit(denyF)
	} else {
		emit(denyF)
		emit(allowF)
	}
	return prog, nil
}

// applySeccomp installs the spec's seccomp filters on the current (locked)
// thread, just before exec. seccomp(2) requires no_new_privs (set by the caller
// for any seccomp spec) or CAP_SYS_ADMIN; filters are inherited across execve.
//
// When both directives are present the kernel stacks the filters and takes the
// most restrictive action, so they compose. Install order matters: the
// permissive address-family filter goes on first, the syscall filter (which may
// be a default-deny allowlist) last — otherwise installing the second filter's
// own seccomp(2) call could be blocked by the first.
func applySeccomp(c *protocol.Confinement) error {
	if !needsSeccomp(c) {
		return nil
	}

	if len(c.AddressFamilies) > 0 {
		fams, err := resolveFamilies(c.AddressFamilies)
		if err != nil {
			return err
		}
		var nativeArch uint32
		switch runtime.GOARCH {
		case "arm64":
			nativeArch = unix.AUDIT_ARCH_AARCH64
		case "amd64":
			nativeArch = unix.AUDIT_ARCH_X86_64
		default:
			return fmt.Errorf("seccomp: unsupported arch %q", runtime.GOARCH)
		}
		prog, err := socketFamilyFilter(nativeArch, uint32(unix.SYS_SOCKET), uint32(unix.SYS_SOCKETPAIR), fams, c.AddressFamiliesDeny)
		if err != nil {
			return err
		}
		if err := installSeccompFilter(prog); err != nil {
			return err
		}
	}

	if len(c.SystemCalls) > 0 {
		prog, err := syscallFilter(c.SystemCalls, c.SystemCallsDeny)
		if err != nil {
			return err
		}
		if err := installSeccompFilter(prog); err != nil {
			return err
		}
	}
	return nil
}

// installSeccompFilter loads one classic-BPF program via seccomp(2) on the
// calling thread (no TSYNC: the shim execs on this same locked thread).
func installSeccompFilter(prog []unix.SockFilter) error {
	if len(prog) == 0 {
		// A caller asked to install a filter but produced no instructions; fail
		// closed rather than dereference prog[0].
		return fmt.Errorf("install seccomp filter: empty program")
	}
	fprog := &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), 0, uintptr(unsafe.Pointer(fprog))); errno != 0 {
		return fmt.Errorf("install seccomp filter: %w", errno)
	}
	runtime.KeepAlive(prog)
	runtime.KeepAlive(fprog)
	return nil
}
