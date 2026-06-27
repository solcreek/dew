//go:build darwin

package main

import "testing"

// parseFlags continues scanning past the first positional so flags appearing
// AFTER it still register. The recursive scan must NOT reset command-scoped
// globals, or flags parsed BEFORE the positional get wiped. Regression for the
// `dew up --reset-disk ./dir --dry-run` class (and --confine, --init, …).
func TestParseFlags_PostPositionalDoesNotWipePriorGlobals(t *testing.T) {
	_, remaining, err := parseFlags([]string{"--reset-disk", "--init", "./dir", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if !flagResetDisk {
		t.Error("--reset-disk (before the positional) was wiped by the post-positional recursive parse")
	}
	if !flagInit {
		t.Error("--init (before the positional) was wiped by the post-positional recursive parse")
	}
	if !flagDryRun {
		t.Error("--dry-run (after the positional) was not parsed")
	}
	if len(remaining) == 0 || remaining[0] != "./dir" {
		t.Errorf("positional not returned: remaining=%v", remaining)
	}
}

// flagProfile is parsed only by parseFlags (not the early global pass), so the
// reset must clear it for test isolation — a prior --profile must not leak into
// a later call that omits it.
func TestParseFlags_ProfileResetForIsolation(t *testing.T) {
	if _, _, err := parseFlags([]string{"--profile", "standard", "--", "true"}); err != nil {
		t.Fatal(err)
	}
	if flagProfile != "standard" {
		t.Fatalf("flagProfile = %q, want standard", flagProfile)
	}
	// A later call without --profile must not see the leaked value.
	if _, _, err := parseFlags([]string{"--", "true"}); err != nil {
		t.Fatal(err)
	}
	if flagProfile != "" {
		t.Errorf("flagProfile leaked: got %q, want \"\"", flagProfile)
	}
}
