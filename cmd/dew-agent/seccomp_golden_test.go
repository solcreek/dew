//go:build linux

package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elastic/go-seccomp-bpf/arch"
	"golang.org/x/sys/unix"
)

var updateSeccompGolden = flag.Bool("update-seccomp", false, "rewrite this arch's testdata/seccomp files from the Go filter builders")

// seccompGolden is one emitted program and the inputs that produced it. The
// Rust agent (rust/dew-agent) rebuilds each from the same inputs and must
// match instruction for instruction, so the two agents enforce identical
// filters.
//
// go-seccomp-bpf assembles only for the build arch, so each arch's files are
// generated, and checked, on that arch: `go test ./cmd/dew-agent -run
// SeccompGolden -update-seccomp` on linux/arm64 (aarch64) and linux/amd64
// (x86_64; Rosetta in a dew VM works). CI checks the x86_64 set.
type seccompGolden struct {
	Kind    string      `json:"kind"` // "syscalls" | "families"
	Arch    string      `json:"arch"` // "aarch64" | "x86_64"
	Names   []string    `json:"names"`
	Deny    bool        `json:"deny"`
	Program [][4]uint32 `json:"program"` // [code, jt, jf, k]
}

func goldenProgram(prog []unix.SockFilter) [][4]uint32 {
	out := make([][4]uint32, len(prog))
	for i, f := range prog {
		out[i] = [4]uint32{uint32(f.Code), uint32(f.Jt), uint32(f.Jf), f.K}
	}
	return out
}

func TestSeccompGolden(t *testing.T) {
	info, err := arch.GetInfo("")
	if err != nil {
		t.Fatal(err)
	}
	inputs := []struct {
		name  string
		kind  string
		names []string
		deny  bool
	}{
		{"syscalls_deny", "syscalls", []string{"mkdir", "mkdirat", "ptrace"}, true},
		{"syscalls_allow", "syscalls", []string{"read", "write", "openat"}, false},
		// Upper case, a duplicate, padding and a name in no table: normalised
		// and dropped.
		{"syscalls_deny_messy", "syscalls", []string{"MKDIRAT", "mkdirat", " Ptrace ", "not_a_syscall"}, true},
		// Every name unknown: the group is empty and only the default remains.
		{"syscalls_deny_all_unknown", "syscalls", []string{"not_a_syscall"}, true},
		// Only the implicit exec/exit set survives.
		{"syscalls_allow_implicit_only", "syscalls", []string{"not_a_syscall"}, false},
		{"families_allow", "families", []string{"AF_UNIX"}, false},
		{"families_deny", "families", []string{"AF_INET", "AF_INET6"}, true},
		// AF_LOCAL aliases AF_UNIX and collapses.
		{"families_allow_alias", "families", []string{"AF_UNIX", "AF_LOCAL", "AF_INET"}, false},
	}
	dir := filepath.Join("testdata", "seccomp")
	if *updateSeccompGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range inputs {
		name := in.name + "." + info.Name
		t.Run(name, func(t *testing.T) {
			var prog []unix.SockFilter
			var err error
			if in.kind == "syscalls" {
				prog, err = syscallFilter(in.names, in.deny)
			} else {
				prog, err = addressFamilyFilter(in.names, in.deny)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := seccompGolden{Kind: in.kind, Arch: info.Name, Names: in.names, Deny: in.deny, Program: goldenProgram(prog)}
			path := filepath.Join(dir, name+".json")
			if *updateSeccompGolden {
				b, _ := json.MarshalIndent(got, "", " ")
				if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with -update-seccomp on %s): %v", info.Name, err)
			}
			var want seccompGolden
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s drifted from the Go builders", path)
			}
		})
	}
}
