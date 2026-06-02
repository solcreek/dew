//go:build darwin

package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/solcreek/dew/internal/vm"
)

// detectDevServerPort scans a chunk of dev-server stdout/stderr
// for a "Local: http://...:PORT" line and returns the port. Most
// modern frameworks (Vite, Next.js, Nuxt, SvelteKit, Astro, Remix)
// announce in a recognizable shape on startup:
//
//	Vite:        Local:   http://localhost:3000/
//	Next.js:     - Local: http://localhost:3000
//	Astro:       Local    http://localhost:4321/
//	Nuxt:        ➜  Local:    http://localhost:3000/
//
// The regex is permissive on surrounding whitespace + decorative
// glyphs (➜, ✓, ⚡), strict on "Local" + URL shape so a stray
// "fetched http://localhost:9999/api" log line doesn't fool us.
//
// Returns 0 when no announce line is present yet (caller retries
// on the next poll tick).
//
// The exact pattern we capture:
//
//	[anything]  Local[:]?  [whitespace]  http[s]://(localhost|127.0.0.1|0.0.0.0):(\d+)
var devLocalRE = regexp.MustCompile(`(?i)\blocal\s*[:]?\s*https?://(?:localhost|127\.0\.0\.1|0\.0\.0\.0):(\d+)`)

func detectDevServerPort(text string) int {
	matches := devLocalRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return 0
	}
	// If multiple announces appear (dev server restarted on a
	// different port), prefer the LAST one — that's the current
	// truth. Vite restarts on port conflict and re-announces.
	last := matches[len(matches)-1]
	port, err := strconv.Atoi(last[1])
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// readDetectedDevPort tails /tmp/dew-dev.log inside the VM via a
// short-timeout vsock exec, strips ANSI codes, and runs the dev-
// server-announce regex on the result. Returns 0 when no "Local:"
// line has appeared yet (so the caller keeps polling).
//
// 5-second timeout — we want this to be CHEAP (it fires every 500ms
// in cmdUp's launch loop until detection succeeds). The log is
// typically < 10 KB at announce time so the tail is fast.
func readDetectedDevPort(v vm.VM, token string, vsockPort uint32) int {
	conn, err := v.VsockConnect(vsockPort)
	if err != nil {
		return 0
	}
	defer conn.Close()
	res, err := execVsockConnTimeout(conn, token, "tail -200 /tmp/dew-dev.log 2>/dev/null", 5*time.Second)
	if err != nil || res == nil {
		return 0
	}
	return detectDevServerPort(stripANSI(res.Stdout))
}

// stripANSI removes ANSI color escape sequences from text before
// regex parsing. Modern dev servers colorize their output; the
// codes around URLs would otherwise break the URL match.
func stripANSI(s string) string {
	// Cheap inline pass — three-character runes that start ANSI
	// sequences are rare enough that the overhead of compiling a
	// regex isn't worth it for this single-pass cleanup.
	var b strings.Builder
	b.Grow(len(s))
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEsc {
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
				inEsc = false
			}
			continue
		}
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i++ // skip [
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
