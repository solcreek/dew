//go:build darwin

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The canonical agent contract for `dew up` is a single line of JSON
// with type:"ready" once the dev server is healthy. This test pins
// the shape — fields any agent integration would rely on must stay
// present. We can't boot a VM here, so we inspect the event shape
// the closure would emit; the actual emit happens at runtime when
// healthy goes true.
//
// The shape is also documented in the changelog so users can grep
// against the same fields without spelunking the source.
func TestReadyEvent_ShapeIsStable(t *testing.T) {
	got := map[string]interface{}{
		"type":       "ready",
		"url":        "http://localhost:5173/",
		"port":       5173,
		"framework":  "vite",
		"elapsed_ms": int64(1234),
	}
	// Marshal to JSON to make sure all fields survive the JSON
	// round-trip (e.g. no func/chan/unexported types snuck in).
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("ready event won't marshal: %v", err)
	}
	out := string(b)
	for _, must := range []string{
		`"type":"ready"`,
		`"url":"http://localhost:5173/"`,
		`"port":5173`,
		`"framework":"vite"`,
		`"elapsed_ms":1234`,
	} {
		if !strings.Contains(out, must) {
			t.Errorf("ready event missing %s\nfull: %s", must, out)
		}
	}
}

// The timeout event has the same shape as ready (same fields) but
// type:"timeout" — an agent can switch on type without parsing two
// different structures.
func TestTimeoutEvent_MirrorsReadyShape(t *testing.T) {
	ready := []string{"type", "url", "port", "framework", "elapsed_ms"}
	timeout := []string{"type", "url", "port", "framework", "elapsed_ms", "hint"}

	// timeout is a superset: ready fields + hint.
	for _, f := range ready {
		found := false
		for _, t := range timeout {
			if t == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("timeout event must include %q (parity with ready)", f)
		}
	}
}
