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

// parseFlags must turn repeatable -p/--publish HOST:CONTAINER into
// cfg.Forwards (HostPort:GuestPort), matching --forward's transport.
func TestParseFlags_Publish(t *testing.T) {
	t.Run("repeatable --publish and -p map to Forwards", func(t *testing.T) {
		cfg, _, err := parseFlags([]string{
			"--image", "nginx", "--publish", "8080:80", "-p", "5432:5432",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Forwards) != 2 {
			t.Fatalf("Forwards = %v, want 2 entries", cfg.Forwards)
		}
		if cfg.Forwards[0].HostPort != 8080 || cfg.Forwards[0].GuestPort != 80 {
			t.Fatalf("Forwards[0] = %+v, want 8080->80", cfg.Forwards[0])
		}
		if cfg.Forwards[1].HostPort != 5432 || cfg.Forwards[1].GuestPort != 5432 {
			t.Fatalf("Forwards[1] = %+v, want 5432->5432", cfg.Forwards[1])
		}
	})

	t.Run("malformed value is rejected", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"-p", "notaport"}); err == nil {
			t.Fatal("-p notaport should error")
		}
	})
}

func TestParseVolume(t *testing.T) {
	cases := []struct {
		in       string
		wantSrc  string
		wantDest string
		wantErr  bool
	}{
		{"pgdata:/var/lib/postgresql/data", "/var/lib/dew/volumes/pgdata", "/var/lib/postgresql/data", false},
		{"/srv/cache:/cache", "/srv/cache", "/cache", false},
		{"name-only", "", "", true},          // no colon
		{"data:relative/path", "", "", true}, // dest not absolute
		{"../escape:/x", "", "", true},       // unsafe name
		{":/x", "", "", true},                // empty name
		{"x:", "", "", true},                 // empty dest
	}
	for _, c := range cases {
		src, dest, err := parseVolume(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseVolume(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if src != c.wantSrc || dest != c.wantDest {
			t.Errorf("parseVolume(%q) = (%q,%q), want (%q,%q)", c.in, src, dest, c.wantSrc, c.wantDest)
		}
	}
}

// -v/--volume requires --image, caps at one, and validates at parse time.
func TestParseFlags_Volume(t *testing.T) {
	t.Run("single volume parses", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"--image", "postgres", "-v", "pgdata:/data"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(flagVolumes) != 1 || flagVolumes[0] != "pgdata:/data" {
			t.Fatalf("flagVolumes = %v, want [pgdata:/data]", flagVolumes)
		}
	})

	t.Run("invalid volume rejected at parse time", func(t *testing.T) {
		if _, _, err := parseFlags([]string{"-v", "bad:relative"}); err == nil {
			t.Fatal("-v bad:relative should error")
		}
	})
}
