//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// emitEvent writes one NDJSON line to stdout when --events or --json is set.
// Both modes share the same event schema. Human progress (spinner etc.)
// continues to be written to stderr so the two channels never mix.
func emitEvent(eventType string, extra map[string]any) {
	if !flagEvents && !flagJSON {
		return
	}
	payload := map[string]any{
		"type": eventType,
		"ts":   time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range extra {
		payload[k] = v
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// suppressProgress reports whether progress UI (spinners etc.) should be
// silenced because the caller wants machine-readable output.
func suppressProgress() bool {
	return flagJSON || flagEvents
}

// fmtErr provides a short error string suitable for events.
func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}
