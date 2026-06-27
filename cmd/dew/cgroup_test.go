//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

// parseCgroup turns a `--cgroup` spec into normalized cgroup-v2 limits.
// The byte sizes are 1024-based; cpu is converted to a cpu.max quota
// numerator for a 100000us period (200% == 2 cores == 200000).
func TestParseCgroup(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    vm.CgroupLimits
		wantErr bool
	}{
		{"memory MiB", "memory=256M", vm.CgroupLimits{MemoryBytes: 256 * 1024 * 1024}, false},
		{"mem alias + GiB", "mem=1G", vm.CgroupLimits{MemoryBytes: 1024 * 1024 * 1024}, false},
		{"TiB suffix (matches confine)", "memory=1T", vm.CgroupLimits{MemoryBytes: 1 << 40}, false},
		{"bare bytes", "memory=1048576", vm.CgroupLimits{MemoryBytes: 1048576}, false},
		{"overflow rejected", "memory=99999999999999G", vm.CgroupLimits{}, true},
		{"pids", "pids=256", vm.CgroupLimits{PidsMax: 256}, false},
		{"tasks alias", "tasks=64", vm.CgroupLimits{PidsMax: 64}, false},
		{"cpu percent", "cpu=200%", vm.CgroupLimits{CPUQuota: 200000}, false},
		{"cpu half core percent", "cpu=50%", vm.CgroupLimits{CPUQuota: 50000}, false},
		{"cpu core count", "cpu=2", vm.CgroupLimits{CPUQuota: 200000}, false},
		{"combined", "memory=256M,pids=256,cpu=200%", vm.CgroupLimits{MemoryBytes: 256 * 1024 * 1024, PidsMax: 256, CPUQuota: 200000}, false},
		{"whitespace tolerant", " memory=256M , pids=64 ", vm.CgroupLimits{MemoryBytes: 256 * 1024 * 1024, PidsMax: 64}, false},
		{"empty", "", vm.CgroupLimits{}, true},
		{"unknown key", "cpus=2", vm.CgroupLimits{}, true},
		{"no equals", "memory", vm.CgroupLimits{}, true},
		{"bad memory", "memory=abc", vm.CgroupLimits{}, true},
		{"zero pids", "pids=0", vm.CgroupLimits{}, true},
		{"negative cpu", "cpu=-1", vm.CgroupLimits{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCgroup(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCgroup(%q) = %+v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCgroup(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseCgroup(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

// appendGuestParams must translate the parsed limits into the dew.* kernel
// cmdline tokens the guest init reads. Unset fields emit nothing so the
// common no-cgroup boot path is unchanged.
func TestAppendGuestParams_Cgroup(t *testing.T) {
	cfg := vm.Config{
		CmdLine: "console=hvc0",
		Cgroup:  vm.CgroupLimits{MemoryBytes: 268435456, PidsMax: 256, CPUQuota: 200000},
	}
	appendGuestParams(&cfg)
	for _, want := range []string{"dew.mem_limit=268435456", "dew.pids_max=256", "dew.cpu_quota=200000"} {
		if !strings.Contains(cfg.CmdLine, want) {
			t.Errorf("CmdLine %q missing %q", cfg.CmdLine, want)
		}
	}

	none := vm.Config{CmdLine: "console=hvc0"}
	appendGuestParams(&none)
	if strings.Contains(none.CmdLine, "dew.mem_limit") || strings.Contains(none.CmdLine, "dew.pids_max") || strings.Contains(none.CmdLine, "dew.cpu_quota") {
		t.Errorf("no-cgroup CmdLine should not carry dew.* cgroup tokens, got %q", none.CmdLine)
	}
}
