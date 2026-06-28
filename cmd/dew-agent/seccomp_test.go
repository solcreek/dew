//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveAddressFamily(t *testing.T) {
	if v, err := resolveAddressFamily("AF_INET"); err != nil || v != uint32(unix.AF_INET) {
		t.Errorf("AF_INET = %d,%v; want %d,nil", v, err, unix.AF_INET)
	}
	// Case-insensitive, whitespace-tolerant.
	if v, err := resolveAddressFamily("  af_unix "); err != nil || v != uint32(unix.AF_UNIX) {
		t.Errorf("af_unix = %d,%v; want %d,nil", v, err, unix.AF_UNIX)
	}
	if _, err := resolveAddressFamily("AF_BOGUS"); err == nil {
		t.Error("unknown family should fail closed")
	}
}

func TestResolveFamilies(t *testing.T) {
	// AF_LOCAL aliases AF_UNIX and a literal repeat collapse to one, first-seen
	// order preserved.
	got, err := resolveFamilies([]string{"AF_INET", "AF_UNIX", "AF_LOCAL", "AF_INET"})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{uint32(unix.AF_INET), uint32(unix.AF_UNIX)}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("resolveFamilies = %v, want %v (deduped, first-seen order)", got, want)
	}
	if _, err := resolveFamilies([]string{"AF_INET", "AF_BOGUS"}); err == nil {
		t.Error("an unknown family should fail closed")
	}
}

// runFilter is a minimal classic-BPF interpreter for the opcodes
// socketFamilyFilter emits (LD abs word, JEQ k, RET k). It returns the matched
// SECCOMP_RET_* value for a synthetic seccomp_data.
func runFilter(t *testing.T, prog []unix.SockFilter, nr, arch, domain uint32) uint32 {
	t.Helper()
	var data [64]byte
	put := func(off int, v uint32) {
		data[off] = byte(v)
		data[off+1] = byte(v >> 8)
		data[off+2] = byte(v >> 16)
		data[off+3] = byte(v >> 24)
	}
	put(scDataNr, nr)
	put(scDataArch, arch)
	put(scDataArg0, domain)
	load := func(off uint32) uint32 {
		return uint32(data[off]) | uint32(data[off+1])<<8 | uint32(data[off+2])<<16 | uint32(data[off+3])<<24
	}

	var a uint32
	for pc := 0; pc < len(prog); {
		ins := prog[pc]
		switch ins.Code {
		case bpfLdAbsW:
			a = load(ins.K)
			pc++
		case bpfJeqK:
			if a == ins.K {
				pc += 1 + int(ins.Jt)
			} else {
				pc += 1 + int(ins.Jf)
			}
		case bpfRetK:
			return ins.K
		default:
			t.Fatalf("unexpected opcode 0x%x at pc %d", ins.Code, pc)
		}
	}
	t.Fatal("filter ran off the end without RET")
	return 0
}

func TestSocketFamilyFilter(t *testing.T) {
	const (
		nativeArch    = uint32(0xAA) // synthetic; decoupled from the real audit arch
		foreignArch   = uint32(0xBB)
		sysSocket     = uint32(198)
		sysSocketpair = uint32(199)
		sysRead       = uint32(63)
		afUnix        = uint32(unix.AF_UNIX)
		afInet        = uint32(unix.AF_INET)
	)
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	eperm := uint32(unix.SECCOMP_RET_ERRNO) | (uint32(unix.EPERM) & uint32(unix.SECCOMP_RET_DATA))

	// Allowlist {AF_UNIX}: only AF_UNIX sockets pass.
	prog, err := socketFamilyFilter(nativeArch, sysSocket, sysSocketpair, []uint32{afUnix}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, afUnix); got != allow {
		t.Errorf("allowlist socket(AF_UNIX) = 0x%x, want ALLOW", got)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, afInet); got != eperm {
		t.Errorf("allowlist socket(AF_INET) = 0x%x, want EPERM", got)
	}
	if got := runFilter(t, prog, sysSocketpair, nativeArch, afInet); got != eperm {
		t.Errorf("allowlist socketpair(AF_INET) = 0x%x, want EPERM", got)
	}
	// Non-socket syscalls are unaffected.
	if got := runFilter(t, prog, sysRead, nativeArch, afInet); got != allow {
		t.Errorf("allowlist read() = 0x%x, want ALLOW", got)
	}
	// A foreign arch (e.g. x86_64 under Rosetta) is allowed through.
	if got := runFilter(t, prog, sysSocket, foreignArch, afInet); got != allow {
		t.Errorf("allowlist foreign-arch socket(AF_INET) = 0x%x, want ALLOW", got)
	}

	// Denylist {AF_INET}: AF_INET blocked, everything else allowed.
	prog, err = socketFamilyFilter(nativeArch, sysSocket, sysSocketpair, []uint32{afInet}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, afInet); got != eperm {
		t.Errorf("denylist socket(AF_INET) = 0x%x, want EPERM", got)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, afUnix); got != allow {
		t.Errorf("denylist socket(AF_UNIX) = 0x%x, want ALLOW", got)
	}

	// Multiple allowed families both pass; a third is denied.
	prog, _ = socketFamilyFilter(nativeArch, sysSocket, sysSocketpair, []uint32{afUnix, afInet}, false)
	if got := runFilter(t, prog, sysSocket, nativeArch, afUnix); got != allow {
		t.Errorf("multi allowlist AF_UNIX = 0x%x, want ALLOW", got)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, afInet); got != allow {
		t.Errorf("multi allowlist AF_INET = 0x%x, want ALLOW", got)
	}
	if got := runFilter(t, prog, sysSocket, nativeArch, uint32(unix.AF_NETLINK)); got != eperm {
		t.Errorf("multi allowlist AF_NETLINK = 0x%x, want EPERM", got)
	}
}
