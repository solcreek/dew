//go:build darwin

package main

import "testing"

// The regex catches the "Local: http://localhost:N" announce that
// every modern bundler prints. These are real captures of the
// startup lines we expect to encounter.
func TestDetectDevServerPort_FrameworkShapes(t *testing.T) {
	cases := map[string]int{
		// Vite (TanStack Start, SvelteKit, Solid, Astro, plain Vite)
		"  VITE v8.0.14  ready in 432 ms\n\n  ➜  Local:   http://localhost:3000/\n  ➜  Network: use --host to expose":     3000,
		"VITE ready\n  Local: http://127.0.0.1:5173/":                                                                       5173,
		// Next.js
		"   ▲ Next.js 15.0.0\n   - Local:        http://localhost:3000\n   - Network:      use --hostname":                  3000,
		// Astro
		"  astro  v4.0.0 ready in 89 ms\n  ┃ Local    http://localhost:4321/\n  ┃ Network  use --host":                       4321,
		// Nuxt
		"➜  Local:    http://localhost:3000/":                                                                                3000,
		// 0.0.0.0 bind
		"  Local:   http://0.0.0.0:8080/":                                                                                    8080,
	}
	for input, want := range cases {
		got := detectDevServerPort(input)
		if got != want {
			t.Errorf("detectDevServerPort(%q) = %d, want %d", input, got, want)
		}
	}
}

// Garbage / pre-announce text MUST NOT false-positive — emitting
// the wrong port would have us set up a forward to nothing while
// the dev server is still booting.
func TestDetectDevServerPort_NoMatch(t *testing.T) {
	for _, input := range []string{
		"",
		"npm install\nadded 1100 packages in 35s",
		"   fetching http://example.com/api/...",
		"  Local files: 12 changed",      // word "Local" but no URL
		"connected to http://otherhost:80", // URL but not "Local"
	} {
		if got := detectDevServerPort(input); got != 0 {
			t.Errorf("detectDevServerPort(%q) should be 0, got %d", input, got)
		}
	}
}

// When dev server restarts on a different port (Vite's "Port X is
// in use, trying Y" behavior), the LATEST announce wins. Pinning
// this so a future "find first match" refactor doesn't silently
// land on a stale port.
func TestDetectDevServerPort_LatestAnnounceWins(t *testing.T) {
	multi := `  Local:   http://localhost:3000/
[hmr] disconnected
  Local:   http://localhost:3001/`
	got := detectDevServerPort(multi)
	if got != 3001 {
		t.Errorf("got %d, want 3001 (latest announce)", got)
	}
}

// ANSI color codes wrap URLs in colored dev-server output. Without
// stripping them first the regex would fail on most real captures.
func TestStripANSI(t *testing.T) {
	in := "\x1b[1m\x1b[32m  Local:\x1b[39m   \x1b[36mhttp://localhost:3000/\x1b[39m\x1b[22m"
	out := stripANSI(in)
	if got := detectDevServerPort(out); got != 3000 {
		t.Errorf("after stripANSI, detected = %d, want 3000\nstripped: %q", got, out)
	}
}

func TestPickFreeHostPort_PassesUnusedThrough(t *testing.T) {
	p, sub, err := pickFreeHostPort(54321, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p != 54321 || sub {
		t.Errorf("expected (54321, false), got (%d, %v)", p, sub)
	}
}
