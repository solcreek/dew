//go:build darwin

package main

import (
	"testing"
)

// parseShare accepts three input shapes — verified here as a table.
// The 2026-06 regression: users following the help text typed
// `--share /path:ro` (host-first with mode), and the parser
// interpreted "ro" as the hostpath under the (then) tag:hostpath
// schema → confusing "stat ro: no such file or directory". Now both
// the host-first and tag-first forms work.
func TestParseShare(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantTag  string
		wantHost string
		wantRO   bool
		wantErr  bool
	}{
		// Host-first (matches help text examples)
		{"hostonly", "/Users/x/proj", "proj", "/Users/x/proj", true, false},
		{"host_with_ro", "/Users/x/proj:ro", "proj", "/Users/x/proj", true, false},
		{"host_with_rw", "/Users/x/proj:rw", "proj", "/Users/x/proj", false, false},
		{"relative_host", "./data:rw", "data", "./data", false, false},

		// Tag-first (legacy / explicit)
		{"tag_host", "app:/Users/x/proj", "app", "/Users/x/proj", true, false},
		{"tag_host_rw", "app:/Users/x/proj:rw", "app", "/Users/x/proj", false, false},
		{"tag_host_ro", "app:/Users/x/proj:ro", "app", "/Users/x/proj", true, false},

		// 2026-06 regression repro — was rejecting before
		{"bundle_ro", "bundle:ro", "bundle", "bundle", true, false},
		{"bundle_rw", "bundle:rw", "bundle", "bundle", false, false},

		// Errors
		{"empty", "", "", "", true, true},
		{"too_many", "a:b:c:rw", "", "", true, true},
		{"just_colon", ":", "", "", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseShare(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %+v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Tag != tc.wantTag {
				t.Errorf("Tag = %q, want %q", got.Tag, tc.wantTag)
			}
			if got.HostPath != tc.wantHost {
				t.Errorf("HostPath = %q, want %q", got.HostPath, tc.wantHost)
			}
			if got.ReadOnly != tc.wantRO {
				t.Errorf("ReadOnly = %v, want %v", got.ReadOnly, tc.wantRO)
			}
		})
	}
}
