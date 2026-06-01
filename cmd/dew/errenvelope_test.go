//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// emitErrorJSON is the single point of truth for the --json error
// envelope shape. The tests below pin every documented field — if
// any disappears or renames silently, downstream agents could break.
func TestEmitErrorJSON_TypedError(t *testing.T) {
	out := captureStdout(t, func() {
		err := dewerr.New(dewerr.CodeAuth, "token expired")
		emitErrorJSON(err, dewerr.CodeOf(err))
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}

	if got["ok"] != false {
		t.Errorf("ok=%v, want false", got["ok"])
	}
	if got["schema_version"] != "1.0" {
		t.Errorf("schema_version=%v, want %q", got["schema_version"], "1.0")
	}
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field not an object: %v", got["error"])
	}
	for _, field := range []string{"code", "exit_code", "message", "retryable"} {
		if _, ok := errObj[field]; !ok {
			t.Errorf("error.%s missing", field)
		}
	}
	if errObj["code"] != "auth" {
		t.Errorf("error.code=%v, want auth", errObj["code"])
	}
	if int(errObj["exit_code"].(float64)) != 100 {
		t.Errorf("error.exit_code=%v, want 100", errObj["exit_code"])
	}
	if errObj["retryable"] != false {
		t.Errorf("error.retryable=%v, want false (auth is non-retryable by default)", errObj["retryable"])
	}
}

func TestEmitErrorJSON_UntypedError(t *testing.T) {
	// Untyped error falls back to CodeGeneric / slug "error".
	out := captureStdout(t, func() {
		err := errors.New("something broke")
		emitErrorJSON(err, dewerr.CodeOf(err))
	})
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	errObj := got["error"].(map[string]any)
	if errObj["code"] != "error" {
		t.Errorf("code=%v, want error", errObj["code"])
	}
	if int(errObj["exit_code"].(float64)) != 1 {
		t.Errorf("exit_code=%v, want 1", errObj["exit_code"])
	}
}

func TestEmitErrorJSON_NetworkIsRetryableByDefault(t *testing.T) {
	out := captureStdout(t, func() {
		err := dewerr.New(dewerr.CodeNetwork, "connection refused")
		emitErrorJSON(err, dewerr.CodeOf(err))
	})
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	errObj := got["error"].(map[string]any)
	if errObj["retryable"] != true {
		t.Errorf("retryable=%v, want true (network errors retry by default)", errObj["retryable"])
	}
}

func TestEmitErrorJSON_HintCarriesThrough(t *testing.T) {
	out := captureStdout(t, func() {
		e := dewerr.New(dewerr.CodeNotFound, "app not running")
		e.Hint = map[string]string{"app": "ghost", "suggestion": "dew apps"}
		emitErrorJSON(e, dewerr.CodeOf(e))
	})
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	errObj := got["error"].(map[string]any)
	hint, ok := errObj["hint"].(map[string]any)
	if !ok {
		t.Fatalf("hint missing or wrong type: %v", errObj["hint"])
	}
	if hint["app"] != "ghost" {
		t.Errorf("hint.app=%v, want ghost", hint["app"])
	}
}

// captureStdout helper is defined in events_test.go (returns string).
