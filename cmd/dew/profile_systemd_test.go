//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// The systemd profile is designed but not built (see docs/systemd-profile.md).
// Until the rootfs exists, --profile systemd must fail fast with an
// "unavailable" code and a helpful pointer — not fall through to a confusing
// asset-download error.
func TestParseFlags_SystemdProfileGuarded(t *testing.T) {
	_, _, err := parseFlags([]string{"--profile", "systemd", "--", "true"})
	if err == nil {
		t.Fatal("--profile systemd should error until the profile is built")
	}
	var de *dewerr.Error
	if !errors.As(err, &de) || de.Code != dewerr.CodeUnavailable {
		t.Fatalf("want CodeUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "docs/systemd-profile.md") {
		t.Errorf("error should point to the design doc, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--confine") {
		t.Errorf("error should mention the --confine alternative, got: %v", err)
	}
}

// A real profile must still parse.
func TestParseFlags_StandardProfileOK(t *testing.T) {
	for _, p := range []string{"minimal", "node", "python", "standard"} {
		if _, _, err := parseFlags([]string{"--profile", p, "--", "true"}); err != nil {
			t.Fatalf("--profile %s should parse, got: %v", p, err)
		}
	}
}

// A misspelled profile must fail with a clear usage error listing the valid
// names, not fall through to a confusing asset-download failure.
func TestParseFlags_UnknownProfileRejected(t *testing.T) {
	_, _, err := parseFlags([]string{"--profile", "noed", "--", "true"})
	if err == nil {
		t.Fatal("unknown profile should error")
	}
	var de *dewerr.Error
	if !errors.As(err, &de) || de.Code != dewerr.CodeUsage {
		t.Fatalf("want CodeUsage, got %v", err)
	}
	if !strings.Contains(err.Error(), "minimal, node, python, standard") {
		t.Errorf("error should list valid profiles, got: %v", err)
	}
}
