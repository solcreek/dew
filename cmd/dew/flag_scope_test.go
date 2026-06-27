//go:build darwin

package main

import (
	"strings"
	"testing"
)

// --cgroup and --confine are parsed by the shared parseFlags but only honored
// by specific commands. Commands that don't thread them through must reject
// them rather than silently no-op. resolveAssets/boot never run because the
// guard fires first (no assets are downloaded in the test).
func TestFlagScope_Rejections(t *testing.T) {
	tests := []struct {
		name string
		fn   func([]string) error
		args []string
		want string
	}{
		{"up rejects --cgroup", cmdUp, []string{"--cgroup", "memory=256M"}, "--cgroup is not supported on `dew up`"},
		{"up rejects --confine", cmdUp, []string{"--confine", "/nonexistent.service"}, "--confine is only supported on `dew run`"},
		{"vm start rejects --confine", cmdStart, []string{"--confine", "/nonexistent.service"}, "--confine is only supported on `dew run`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}
