//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// The systemd profile owns the cgroup hierarchy (PID 1 = systemd) and runs
// real units, so dew's --cgroup and --confine can't apply. systemdProfileFlagConflict
// must reject those combinations with a CodeUsage error, and leave every other
// combination (including the same flags on non-systemd profiles) alone. This is
// the gate that activates when the --profile systemd availability guard is
// removed in R1 Phase 4.
func TestSystemdProfileFlagConflict(t *testing.T) {
	cases := []struct {
		name      string
		profile   string
		confine   string
		cgroupSet bool
		wantErr   bool
		wantSub   string
	}{
		{"systemd alone is fine", "systemd", "", false, false, ""},
		{"systemd + cgroup rejected", "systemd", "", true, true, "--cgroup cannot be combined"},
		{"systemd + confine rejected", "systemd", "x.service", false, true, "--confine cannot be combined"},
		{"cgroup precedence over confine", "systemd", "x.service", true, true, "--cgroup cannot be combined"},
		{"non-systemd + cgroup is fine", "standard", "", true, false, ""},
		{"non-systemd + confine is fine", "node", "x.service", false, false, ""},
		{"empty profile is fine", "", "x.service", true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := systemdProfileFlagConflict(c.profile, c.confine, c.cgroupSet)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if code := dewerr.CodeOf(err); code != dewerr.CodeUsage {
					t.Errorf("error code = %v, want CodeUsage", code)
				}
				if !strings.Contains(err.Error(), c.wantSub) {
					t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
				}
				return
			}
			if err != nil {
				t.Errorf("expected nil, got %v", err)
			}
		})
	}
}
