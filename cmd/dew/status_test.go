//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempSocketPath redirects daemon.SocketPath("") to a temp dir
// by overriding HOME — SocketDir resolves to $HOME/.local/state/dew.
// This is the only seam status_test needs; we exercise the file +
// connection probe directly without spinning up a real daemon.
func withTempSocketPath(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return filepath.Join(tmp, ".local", "state", "dew", "default.sock")
}

// Uses captureStdout from events_test.go (same package).

func TestStatus_NotRunningWhenNoSocket(t *testing.T) {
	_ = withTempSocketPath(t)
	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "not running") {
		t.Errorf("expected 'not running', got: %q", out)
	}
	if strings.Contains(out, "stale socket") {
		t.Errorf("clean state misreported as stale: %q", out)
	}
}

// A leftover socket file with no listener is a real crash scenario.
// Status must surface it distinctly so the user understands the next
// `dew start` will clean it up.
func TestStatus_StaleSocketDetected(t *testing.T) {
	sock := withTempSocketPath(t)
	// Create a plain file at the socket path (not a real listener).
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "stale socket") {
		t.Errorf("expected 'stale socket' marker, got: %q", out)
	}
}

func TestStatus_RunningWhenListenerAccepts(t *testing.T) {
	sock := withTempSocketPath(t)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Drain accepts so the dial completes cleanly; we don't read
	// from the conn — status only needs the handshake.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "running") || strings.Contains(out, "not running") {
		t.Errorf("expected 'running' (no 'not'), got: %q", out)
	}
}

// JSON envelope is the contract grove + other agents depend on.
// Pin every field so a future refactor can't silently rename them.
func TestStatus_JSONEnvelopeShape(t *testing.T) {
	_ = withTempSocketPath(t)
	flagJSON = true
	defer func() { flagJSON = false }()

	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	out = strings.TrimSpace(out)
	var env struct {
		Ok            bool   `json:"ok"`
		SchemaVersion string `json:"schema_version"`
		Data          struct {
			Running       bool   `json:"running"`
			Socket        string `json:"socket"`
			SocketPresent bool   `json:"socket_present"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if !env.Ok {
		t.Errorf("ok=false even on clean not-running state")
	}
	if env.SchemaVersion != "1.0" {
		t.Errorf("schema_version = %q, want 1.0", env.SchemaVersion)
	}
	if env.Data.Running {
		t.Errorf("running=true with no socket present")
	}
	if env.Data.Socket == "" {
		t.Errorf("socket path empty")
	}
}

// Exit code is part of the contract — `dew status` is a query, not
// an action; it MUST return nil so `set -e` scripts can chain it.
func TestStatus_NeverReturnsError(t *testing.T) {
	_ = withTempSocketPath(t)
	flagJSON = false
	var stdout bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := cmdStatus(nil)
	w.Close()
	os.Stdout = old
	_, _ = io.Copy(&stdout, r)
	if err != nil {
		t.Errorf("status returned error on not-running state: %v", err)
	}
}
