//go:build darwin

package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vmstate"
	"github.com/solcreek/dew/pkg/dewerr"
)

// withTempSocketPath redirects daemon.SocketPath("") to a temp dir
// by overriding HOME — SocketDir resolves to $HOME/.local/state/dew.
// This is the only seam status_test needs; we exercise the file +
// connection probe directly without spinning up a real daemon.
//
// Uses /tmp directly rather than t.TempDir() because macOS sockaddr_un
// has a 104-byte sun_path limit, and t.TempDir() on macOS returns
// long paths like /var/folders/.../T/TestName123/001/ — appending
// .local/state/dew/default.sock pushes the total well past 104 bytes
// and bind() fails with "invalid argument". /tmp/dew-test-XXX is
// short enough to stay under the limit on every macOS version.
// t.Cleanup handles removal since we're not using t.TempDir's hooks.
func withTempSocketPath(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "dew-status-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
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
	var statusErr error
	out := captureStdout(t, func() {
		statusErr = cmdStatus(nil)
	})
	if !strings.Contains(out, "running") || strings.Contains(out, "not running") {
		t.Errorf("expected 'running' (no 'not'), got: %q", out)
	}
	if statusErr != nil {
		t.Errorf("a running VM must exit 0 (nil), got: %v", statusErr)
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

// While a dew process is booting a VM (state file with a live PID,
// no daemon socket yet), status must say so instead of "not running" —
// this was invisible before and agents polling status couldn't tell
// "nothing happening" from "boot in progress".
func TestStatus_ReportsBooting(t *testing.T) {
	sock := withTempSocketPath(t)
	stateDir := filepath.Dir(sock)
	if err := vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseBooting, Mode: "run",
		Profile: "minimal", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "booting") {
		t.Errorf("expected 'booting', got: %q", out)
	}

	flagJSON = true
	defer func() { flagJSON = false }()
	out = captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	var env struct {
		Data struct {
			Running bool   `json:"running"`
			Phase   string `json:"phase"`
			PID     int    `json:"pid"`
			Mode    string `json:"mode"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if env.Data.Phase != "booting" || env.Data.Mode != "run" || env.Data.PID != os.Getpid() {
		t.Errorf("data = %+v, want phase=booting mode=run pid=%d", env.Data, os.Getpid())
	}
	if env.Data.Running {
		t.Error("running=true while still booting (no socket)")
	}
}

// An ephemeral `dew run` never opens a daemon socket; once its guest
// is reachable the state file says running and status must report the
// VM without claiming daemon exec is available.
func TestStatus_ReportsEphemeralRun(t *testing.T) {
	sock := withTempSocketPath(t)
	stateDir := filepath.Dir(sock)
	if err := vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "run",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "running") || !strings.Contains(out, "ephemeral") {
		t.Errorf("expected ephemeral running report, got: %q", out)
	}
}

// A state file from a crashed process (dead PID) is a leftover, not a
// state: status must ignore it, report not running, and remove it.
func TestStatus_StaleStateFileIgnoredAndCleaned(t *testing.T) {
	sock := withTempSocketPath(t)
	stateDir := filepath.Dir(sock)

	// A PID that is certainly dead: spawn-and-reap.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid

	if err := vmstate.Write(stateDir, vmstate.State{
		PID: deadPID, Phase: vmstate.PhaseBooting, Mode: "run",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	flagJSON = false
	out := captureStdout(t, func() {
		_ = cmdStatus(nil)
	})
	if !strings.Contains(out, "not running") || strings.Contains(out, "booting") {
		t.Errorf("stale state misreported: %q", out)
	}
	if _, ok := vmstate.Read(stateDir); ok {
		t.Error("stale state file not cleaned up")
	}
}

// Exit code is part of the contract: `dew vm status` is a reuse-vs-boot gate,
// so on a not-running state it returns a statusExit carrying CodeConflict
// (→ non-zero exit, matching `dew exec` / `dew vm forward`'s "no running VM")
// while STILL printing its report, letting `if dew vm status; then reuse; else
// boot; fi` branch. main() honors the code without an error banner over the
// report.
func TestStatus_ExitsConflictWhenNotRunning(t *testing.T) {
	_ = withTempSocketPath(t)
	flagJSON = false
	var err error
	out := captureStdout(t, func() { err = cmdStatus(nil) })
	if !strings.Contains(out, "not running") {
		t.Errorf("report must still print to stdout, got: %q", out)
	}
	var se statusExit
	if !errors.As(err, &se) {
		t.Fatalf("status returned %v (%T), want statusExit on not-running", err, err)
	}
	if se.Code != dewerr.CodeConflict {
		t.Errorf("statusExit.Code = %d, want CodeConflict (%d)", se.Code, dewerr.CodeConflict)
	}
}

// statusVMPresent is the exit-code decision: a VM that is running, booting, or
// an ephemeral run counts as present (exit 0); nothing — or only a stale
// socket — is absent (exit CodeNotFound). Booting must count so a gate doesn't
// boot a second VM over one mid-boot.
func TestStatusVMPresent(t *testing.T) {
	cases := []struct {
		name    string
		alive   bool
		hasSt   bool
		phase   vmstate.Phase
		present bool
	}{
		{"daemon running", true, false, "", true},
		{"booting", false, true, vmstate.PhaseBooting, true},
		{"ephemeral running", false, true, vmstate.PhaseRunning, true},
		{"alive overrides missing state", true, false, "", true},
		{"nothing / stale socket only", false, false, "", false},
		{"no state short-circuits phase", false, false, vmstate.PhaseBooting, false},
	}
	for _, c := range cases {
		if got := statusVMPresent(c.alive, c.hasSt, c.phase); got != c.present {
			t.Errorf("%s: statusVMPresent(%v,%v,%q) = %v, want %v",
				c.name, c.alive, c.hasSt, c.phase, got, c.present)
		}
	}
}
