//go:build darwin

package main

import "testing"

// parseFlags must recognize --rosetta as a no-value flag that sets
// EnableRosetta. The "after a positional" case is the regression-prone
// one parseFlags has tripped on before: the flag must still be accepted
// (not error out). Note that, like every cfg-affecting flag, its effect
// is not applied once it trails a positional — that's a pre-existing
// parseFlags limitation, not specific to --rosetta.
func TestParseFlags_Rosetta(t *testing.T) {
	t.Run("flag sets EnableRosetta", func(t *testing.T) {
		cfg, _, err := parseFlags([]string{"--rosetta"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.EnableRosetta {
			t.Fatal("--rosetta did not set EnableRosetta")
		}
	})

	t.Run("absent leaves EnableRosetta false", func(t *testing.T) {
		cfg, _, err := parseFlags([]string{"--network"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.EnableRosetta {
			t.Fatal("EnableRosetta should default to false")
		}
	})

	t.Run("flag after positional is still accepted", func(t *testing.T) {
		_, remaining, err := parseFlags([]string{"/tmp/some-dir", "--rosetta"})
		if err != nil {
			t.Fatalf("--rosetta after positional should not error: %v", err)
		}
		if len(remaining) == 0 || remaining[0] != "/tmp/some-dir" {
			t.Fatalf("positional should still be returned, got %v", remaining)
		}
	})
}
