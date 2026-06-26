//go:build darwin

package main

import (
	"path/filepath"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

// Real-work profiles (node/python/standard) need 4 vCPUs + 2 GB so
// npm-install reify and bundler transforms don't stall on a single
// core. minimal stays at the global default so `dew run`-style
// ephemeral commands stay light. Pinning the numbers here because
// dropping them silently was the cause of a ~4× install slowdown.
func TestApplyProfileDefaults_RealWorkProfiles(t *testing.T) {
	cases := []struct {
		profile  string
		wantCPUs uint
		wantMem  uint64
		wantImg  string
		wantGB   uint
	}{
		{"node", 4, 2048, "node.img", 4},
		{"python", 4, 2048, "python.img", 4},
		{"standard", 4, 2048, "standard.img", 10},
	}
	for _, c := range cases {
		t.Run(c.profile, func(t *testing.T) {
			cfg := vm.Config{CPUs: 1, MemoryMB: 512}
			applyProfileDefaults(&cfg, c.profile, "/data", "")
			if cfg.CPUs != c.wantCPUs {
				t.Errorf("CPUs = %d, want %d", cfg.CPUs, c.wantCPUs)
			}
			if cfg.MemoryMB != c.wantMem {
				t.Errorf("MemoryMB = %d, want %d", cfg.MemoryMB, c.wantMem)
			}
			if cfg.DiskPath != filepath.Join("/data", c.wantImg) {
				t.Errorf("DiskPath = %q, want %q", cfg.DiskPath, filepath.Join("/data", c.wantImg))
			}
			if cfg.DiskGB != c.wantGB {
				t.Errorf("DiskGB = %d, want %d", cfg.DiskGB, c.wantGB)
			}
		})
	}
}

// A named VM gets its own per-name disk ("<profile>-<name>.img") so
// concurrent named VMs are fully isolated — disk, socket, and state dir
// all keyed by name. The default unnamed VM keeps "<profile>.img".
func TestApplyProfileDefaults_NamedDisk(t *testing.T) {
	cfg := vm.Config{CPUs: 1, MemoryMB: 512}
	applyProfileDefaults(&cfg, "node", "/data", "redis")
	if want := filepath.Join("/data", "node-redis.img"); cfg.DiskPath != want {
		t.Errorf("named DiskPath = %q, want %q", cfg.DiskPath, want)
	}
}

// minimal must NOT auto-bump — `dew run`-style ephemeral commands
// would otherwise grab 4 host cores + 2 GB for nothing. Same applies
// to unknown profile names: leave the caller's config alone.
func TestApplyProfileDefaults_MinimalStaysLight(t *testing.T) {
	for _, p := range []string{"minimal", "", "unknown"} {
		t.Run(p, func(t *testing.T) {
			cfg := vm.Config{CPUs: 1, MemoryMB: 512}
			applyProfileDefaults(&cfg, p, "/data", "")
			if cfg.CPUs != 1 || cfg.MemoryMB != 512 {
				t.Errorf("profile %q changed defaults: CPUs=%d MemoryMB=%d",
					p, cfg.CPUs, cfg.MemoryMB)
			}
			if cfg.DiskPath != "" {
				t.Errorf("profile %q set DiskPath=%q, want empty", p, cfg.DiskPath)
			}
		})
	}
}

// Explicit --cpus / --memory overrides come in BEFORE defaults run
// (parseFlags sets cfg.CPUs to the user value). The default switch
// must not stomp on those — only fills when the value still matches
// the parseFlags-zero of 1 / 512.
func TestApplyProfileDefaults_RespectsExplicitOverrides(t *testing.T) {
	cfg := vm.Config{CPUs: 2, MemoryMB: 4096, DiskPath: "/custom/disk.img", DiskGB: uint(8)}
	applyProfileDefaults(&cfg, "node", "/data", "redis")
	if cfg.CPUs != 2 {
		t.Errorf("CPUs overridden: got %d, want 2", cfg.CPUs)
	}
	if cfg.MemoryMB != 4096 {
		t.Errorf("MemoryMB overridden: got %d, want 4096", cfg.MemoryMB)
	}
	if cfg.DiskPath != "/custom/disk.img" {
		t.Errorf("DiskPath overridden: got %q", cfg.DiskPath)
	}
	if cfg.DiskGB != 8 {
		t.Errorf("DiskGB overridden: got %d, want 8", cfg.DiskGB)
	}
}
