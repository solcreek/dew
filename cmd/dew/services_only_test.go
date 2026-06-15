//go:build darwin

package main

import (
	"strings"
	"testing"
)

func TestParseFlags_ServicesOnly(t *testing.T) {
	if _, _, err := parseFlags([]string{"--services-only"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flagServicesOnly {
		t.Error("--services-only was not parsed")
	}

	if _, _, err := parseFlags([]string{"--no-dev"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flagServicesOnly {
		t.Error("--no-dev alias was not parsed")
	}

	// parseFlags resets command-scoped globals: a later call without the
	// flag must clear it.
	if _, _, err := parseFlags([]string{}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flagServicesOnly {
		t.Error("flagServicesOnly should reset to false when absent")
	}
}

// services-only without --with is a user error and must be caught before
// any VM boot, with an actionable message.
func TestCmdUp_ServicesOnlyRequiresWith(t *testing.T) {
	defer func() { flagServicesOnly = false; flagWith = "" }()

	err := cmdUp([]string{"--services-only"})
	if err == nil {
		t.Fatal("expected error when --services-only is used without --with")
	}
	if !strings.Contains(err.Error(), "requires --with") {
		t.Errorf("error = %q, want it to mention --with requirement", err.Error())
	}
}
