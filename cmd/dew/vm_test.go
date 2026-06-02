//go:build darwin

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// cmdVM with no args MUST surface a Usage error rather than
// silently doing nothing. Grove and any other caller that
// programmatically invokes the dispatcher needs a deterministic
// shape for the empty-args case.
func TestCmdVM_RequiresSubcommand(t *testing.T) {
	err := cmdVM(nil)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	if got := dewerr.CodeOf(err); got != dewerr.CodeUsage {
		t.Errorf("code = %v, want CodeUsage", got)
	}
}

func TestCmdVM_UnknownSubcommandIsUsageError(t *testing.T) {
	err := cmdVM([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := dewerr.CodeOf(err); got != dewerr.CodeUsage {
		t.Errorf("code = %v, want CodeUsage", got)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown subcommand: %v", err)
	}
}

// status is the cheapest subcommand to exercise — it doesn't touch
// the VM, just probes the socket. Calling it via cmdVM proves the
// dispatcher actually delegates to cmdStatus rather than no-oping.
func TestCmdVM_StatusDelegates(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	flagJSON = false
	// Capture stdout to confirm something rendered.
	out := captureStdout(t, func() {
		_ = cmdVM([]string{"status"})
	})
	if out == "" {
		t.Errorf("dew vm status produced no output")
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("expected status output, got: %q", out)
	}
}

// Deprecation hints go to stderr, never to stdout — so they can't
// pollute --json envelopes that callers parse. Test by redirecting
// stderr and confirming the message lands there only.
func TestDeprecationHint_StderrOnly(t *testing.T) {
	flagJSON = false

	origStderr := os.Stderr
	rErr, wErr, _ := os.Pipe()
	os.Stderr = wErr

	origStdout := os.Stdout
	rOut, wOut, _ := os.Pipe()
	os.Stdout = wOut

	deprecationHint("start", "vm start")

	wErr.Close()
	wOut.Close()
	os.Stderr = origStderr
	os.Stdout = origStdout

	errBuf := make([]byte, 1024)
	n, _ := rErr.Read(errBuf)
	stderr := string(errBuf[:n])

	outBuf := make([]byte, 1024)
	m, _ := rOut.Read(outBuf)
	stdout := string(outBuf[:m])

	if !strings.Contains(stderr, "deprecated") {
		t.Errorf("hint missing from stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "dew start") || !strings.Contains(stderr, "dew vm start") {
		t.Errorf("hint should name both old and new paths: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("hint leaked to stdout: %q", stdout)
	}
}

// In --json mode, the deprecation hint MUST be silent — agents
// pipe stdout and don't want stderr surprises mid-envelope.
func TestDeprecationHint_SilentInJSONMode(t *testing.T) {
	flagJSON = true
	defer func() { flagJSON = false }()

	origStderr := os.Stderr
	rErr, wErr, _ := os.Pipe()
	os.Stderr = wErr

	deprecationHint("start", "vm start")

	wErr.Close()
	os.Stderr = origStderr

	buf := make([]byte, 1024)
	n, _ := rErr.Read(buf)
	if n != 0 {
		t.Errorf("deprecation hint leaked in JSON mode: %q", string(buf[:n]))
	}
}
