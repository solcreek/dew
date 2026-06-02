//go:build darwin

package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// withCapturedStdout swaps os.Stdout for a pipe, runs fn, and
// returns the captured bytes. Used to verify the NDJSON event
// stream without depending on the full share command running.
func withCapturedStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()

	fn()
	w.Close()
	os.Stdout = old
	return string(<-done)
}

// The event contract is the load-bearing surface for grove share's
// future delegation to dew share. These tests pin the shape so a
// future edit can't silently drop a field or rename an event
// without surfacing the break.

func TestShareEvent_NoOutputWhenEventsFlagOff(t *testing.T) {
	flagEvents = false
	out := withCapturedStdout(t, func() {
		emitShareEvent("starting", map[string]any{"port": "3000"})
	})
	if out != "" {
		t.Errorf("--events off should emit nothing; got: %q", out)
	}
}

func TestShareEvent_EmitsNDJSONWhenEventsFlagOn(t *testing.T) {
	flagEvents = true
	defer func() { flagEvents = false }()

	out := withCapturedStdout(t, func() {
		emitShareEvent("starting", map[string]any{"port": "3000"})
	})
	// One line, one JSON object.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line, got %d: %q", len(lines), out)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("not valid JSON: %v\nline: %s", err, lines[0])
	}

	// Every event MUST have event + ts. These two are the
	// stability contract for downstream parsers.
	if ev["event"] != "starting" {
		t.Errorf("event = %v, want starting", ev["event"])
	}
	ts, ok := ev["ts"].(string)
	if !ok || ts == "" {
		t.Errorf("ts missing or wrong type: %v", ev["ts"])
	}
	// Verify ts parses as RFC3339Nano (round-trip).
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts %q not RFC3339Nano: %v", ts, err)
	}

	// Custom fields are merged in.
	if ev["port"] != "3000" {
		t.Errorf("port field missing: %v", ev["port"])
	}
}

// Multiple events emit as one JSON object per line — that's the
// NDJSON convention consumers (grove share) will rely on. Pin it.
func TestShareEvent_MultipleEventsEachOnOwnLine(t *testing.T) {
	flagEvents = true
	defer func() { flagEvents = false }()

	out := withCapturedStdout(t, func() {
		emitShareEvent("starting", map[string]any{"port": "3000"})
		emitShareEvent("tunnel-url", map[string]any{
			"url":  "https://x.trycloudflare.com",
			"port": "3000",
		})
		emitShareEvent("established", map[string]any{
			"url":  "https://x.trycloudflare.com",
			"port": "3000",
		})
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), out)
	}
	wantEvents := []string{"starting", "tunnel-url", "established"}
	for i, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d not JSON: %v\n  %s", i, err, line)
			continue
		}
		if ev["event"] != wantEvents[i] {
			t.Errorf("line %d event = %v, want %s", i, ev["event"], wantEvents[i])
		}
	}
}

// Pin all event names so adding a new one or renaming an existing
// one shows up in code review. The grove side (and any other
// consumer) hard-codes these strings; a typo here is silent failure.
func TestShareEvent_KnownEventVocabulary(t *testing.T) {
	flagEvents = true
	defer func() { flagEvents = false }()

	known := map[string]bool{
		"starting":      true,
		"tunnel-url":    true,
		"established":   true,
		"probe-timeout": true,
		"closed":        true,
	}
	for name := range known {
		out := withCapturedStdout(t, func() {
			emitShareEvent(name, nil)
		})
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil {
			t.Errorf("event %q didn't emit valid JSON: %v", name, err)
			continue
		}
		if ev["event"] != name {
			t.Errorf("event name not echoed: %q != %v", name, ev["event"])
		}
	}
}

// closed event carries an optional `reason` field. Pin it so
// callers know they can branch on it (e.g., "no_tunnel_url" vs
// "tunnel-exited").
func TestShareEvent_ClosedReasonField(t *testing.T) {
	flagEvents = true
	defer func() { flagEvents = false }()

	out := withCapturedStdout(t, func() {
		emitShareEvent("closed", map[string]any{"reason": "no_tunnel_url"})
	})
	var ev map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &ev)
	if ev["reason"] != "no_tunnel_url" {
		t.Errorf("closed.reason missing: %v", ev["reason"])
	}
}
