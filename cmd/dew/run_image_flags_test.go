//go:build darwin

package main

import "testing"

// parseFlags must collect repeatable -e/--env pairs into flagEnv and
// reject values without an '='. flagEnv is process-global, so each
// subtest re-runs parseFlags (which resets it) to stay independent.
func TestParseFlags_Env(t *testing.T) {
	t.Run("repeatable --env and -e accumulate", func(t *testing.T) {
		if _, _, err := parseFlags([]string{
			"--image", "redis:7", "--env", "A=1", "-e", "B=2",
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(flagEnv) != 2 || flagEnv[0] != "A=1" || flagEnv[1] != "B=2" {
			t.Fatalf("flagEnv = %v, want [A=1 B=2]", flagEnv)
		}
	})

	t.Run("value without = is rejected", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--env", "NOPE"}); err == nil {
			t.Fatal("--env NOPE should error (missing '=')")
		}
	})

	t.Run("reset between calls", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--image", "redis:7", "-e", "A=1"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, _, err := parseFlags([]string{"--image", "redis:7"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(flagEnv) != 0 {
			t.Fatalf("flagEnv should reset to empty, got %v", flagEnv)
		}
	})
}
