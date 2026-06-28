//go:build linux

package main

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	protocol "github.com/solcreek/dew/internal/vsock"
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
// applies. Today that is only RestrictAddressFamilies= (named/grouped
// SystemCallFilter= is a later phase).
func needsSeccomp(c *protocol.Confinement) bool {
	return len(c.AddressFamilies) > 0
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

// applySeccomp installs the RestrictAddressFamilies= filter on the current
// (locked) thread, just before exec. seccomp(2) requires no_new_privs (set by
// the caller for any seccomp spec) or CAP_SYS_ADMIN. The filter is inherited
// across execve and applies to the target.
func applySeccomp(c *protocol.Confinement) error {
	if !needsSeccomp(c) {
		return nil
	}
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
	fprog := &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), 0, uintptr(unsafe.Pointer(fprog))); errno != 0 {
		return fmt.Errorf("install seccomp filter: %w", errno)
	}
	runtime.KeepAlive(prog)
	runtime.KeepAlive(fprog)
	return nil
}
