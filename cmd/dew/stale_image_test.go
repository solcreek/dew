//go:build darwin

package main

import (
	"strings"
	"testing"
)

// `dew assets pull standard` must target the standard profile, not
// silently fall back to minimal. Both the positional and --profile
// forms are supported; --profile wins if both appear.
func TestParseAssetsArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		def      string
		wantProf string
		wantForc bool
	}{
		{"positional", []string{"pull", "standard"}, "minimal", "standard", false},
		{"flag", []string{"pull", "--profile", "node"}, "minimal", "node", false},
		{"positional+force", []string{"pull", "standard", "--force"}, "minimal", "standard", true},
		{"flag wins over positional", []string{"pull", "minimal", "--profile", "standard"}, "minimal", "standard", false},
		{"default when bare", []string{"pull"}, "minimal", "minimal", false},
		{"seeded default", []string{"list"}, "node", "node", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProf, gotForc := parseAssetsArgs(tt.args, tt.def)
			if gotProf != tt.wantProf {
				t.Errorf("profile = %q, want %q", gotProf, tt.wantProf)
			}
			if gotForc != tt.wantForc {
				t.Errorf("force = %v, want %v", gotForc, tt.wantForc)
			}
		})
	}
}

func TestStaleDiskHint(t *testing.T) {
	// No disk → no hint (minimal profile has no persistent disk).
	if h := staleDiskHint("", "dew up --reset-disk"); h != "" {
		t.Errorf("expected empty hint with no disk, got %q", h)
	}
	// Disk profile → hint names the rebuild command and the disk path.
	h := staleDiskHint("/home/u/.local/share/dew/standard.img", "dew up --reset-disk")
	for _, want := range []string{"dew up --reset-disk", "/home/u/.local/share/dew/standard.img", "rm "} {
		if !strings.Contains(h, want) {
			t.Errorf("hint missing %q:\n%s", want, h)
		}
	}
}

func TestParseFlags_ResetDisk(t *testing.T) {
	if _, _, err := parseFlags([]string{"--reset-disk"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flagResetDisk {
		t.Error("--reset-disk was not parsed")
	}
	// Resets when absent on a later call.
	if _, _, err := parseFlags([]string{}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flagResetDisk {
		t.Error("flagResetDisk should reset to false when absent")
	}
}
