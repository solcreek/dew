//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn and returns whatever it wrote to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	<-done
	return buf.String()
}

func TestEmitEvent_SuppressedByDefault(t *testing.T) {
	flagJSON = false
	flagEvents = false
	got := captureStdout(t, func() {
		emitEvent("preparing", map[string]any{"backend": "vm"})
	})
	if got != "" {
		t.Errorf("expected no output, got %q", got)
	}
}

func TestEmitEvent_JSONMode(t *testing.T) {
	defer func() { flagJSON = false }()
	flagJSON = true
	flagEvents = false
	got := captureStdout(t, func() {
		emitEvent("started", map[string]any{
			"backend": "docker",
			"port":    3000,
		})
	})

	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &ev); err != nil {
		t.Fatalf("not valid JSON: %v\noutput: %q", err, got)
	}
	if ev["type"] != "started" {
		t.Errorf("type = %v, want 'started'", ev["type"])
	}
	if ev["backend"] != "docker" {
		t.Errorf("backend = %v, want 'docker'", ev["backend"])
	}
	if _, ok := ev["ts"]; !ok {
		t.Error("expected ts field")
	}
}

func TestEmitEvent_NDJSONShape(t *testing.T) {
	defer func() { flagEvents = false }()
	flagEvents = true
	flagJSON = false
	got := captureStdout(t, func() {
		emitEvent("a", map[string]any{"x": 1})
		emitEvent("b", map[string]any{"y": 2})
		emitEvent("c", nil)
	})

	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines, got %d:\n%s", len(lines), got)
	}
	for i, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d not valid JSON: %v", i+1, err)
		}
	}
}

func TestEmitEvent_FallbackPayloadShape(t *testing.T) {
	defer func() { flagJSON = false }()
	flagJSON = true
	got := captureStdout(t, func() {
		emitEvent("fallback", map[string]any{
			"from":   "vm",
			"to":     "docker",
			"reason": "VZErrorDomain Code=1",
		})
	})

	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["from"] != "vm" {
		t.Errorf("from = %v", ev["from"])
	}
	if ev["to"] != "docker" {
		t.Errorf("to = %v", ev["to"])
	}
	if !strings.Contains(ev["reason"].(string), "VZError") {
		t.Errorf("reason = %v", ev["reason"])
	}
}

func TestSuppressProgress(t *testing.T) {
	flagJSON, flagEvents = false, false
	if suppressProgress() {
		t.Error("should not suppress with neither flag")
	}
	flagJSON = true
	if !suppressProgress() {
		t.Error("should suppress with --json")
	}
	flagJSON = false
	flagEvents = true
	if !suppressProgress() {
		t.Error("should suppress with --events")
	}
	flagEvents = false
}

func TestFmtErr(t *testing.T) {
	if fmtErr(nil) != "" {
		t.Error("nil error should be empty string")
	}
	if got := fmtErr(io.EOF); got == "" {
		t.Error("non-nil error should not be empty")
	}
}
