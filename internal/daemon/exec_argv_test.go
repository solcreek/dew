package daemon

import (
	"reflect"
	"testing"
)

// Argv mode must pass through to the guest agent as direct argv —
// no /bin/sh wrap. This is the daemon-side half of the fix for the
// `dew exec sh -c '...'` double-wrap bug.
func TestBuildVsockExec_ArgvMode(t *testing.T) {
	req := ExecRequest{Argv: []string{"sh", "-c", "echo A; echo B"}}
	got := buildVsockExec(req, "tok-123")

	if got.Command != "sh" {
		t.Errorf("Command = %q, want %q", got.Command, "sh")
	}
	want := []string{"-c", "echo A; echo B"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("Args = %#v, want %#v", got.Args, want)
	}
	if got.Token != "tok-123" {
		t.Errorf("token not threaded: got %q", got.Token)
	}
}

// Shell mode (legacy: Command set, Argv empty) must still wrap in
// /bin/sh -c. Older clients that don't know about Argv keep working.
func TestBuildVsockExec_ShellModeLegacy(t *testing.T) {
	req := ExecRequest{Command: "echo a; echo b"}
	got := buildVsockExec(req, "tok")

	if got.Command != "/bin/sh" {
		t.Errorf("Command = %q, want /bin/sh", got.Command)
	}
	want := []string{"-c", "echo a; echo b"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("Args = %#v, want %#v", got.Args, want)
	}
}

// If a client sets BOTH fields, Argv wins. This matches the
// ExecRequest doc — protects against ambiguity if a client author
// fills out the whole struct.
func TestBuildVsockExec_ArgvBeatsCommandWhenBothSet(t *testing.T) {
	req := ExecRequest{
		Command: "should-be-ignored",
		Argv:    []string{"true"},
	}
	got := buildVsockExec(req, "")

	if got.Command != "true" {
		t.Errorf("Argv should win when both set; got Command=%q", got.Command)
	}
	if len(got.Args) != 0 {
		t.Errorf("argv with just [true] should yield empty Args; got %#v", got.Args)
	}
}

// Stream and TimeoutMs are independent of the mode and must thread
// through both branches.
func TestBuildVsockExec_StreamingAndTimeoutThreaded(t *testing.T) {
	argv := buildVsockExec(ExecRequest{Argv: []string{"sleep", "1"}, Stream: true, TimeoutMs: 5000}, "")
	if !argv.Stream {
		t.Error("argv mode lost Stream flag")
	}
	if argv.TimeoutMs != 5000 {
		t.Errorf("argv mode lost TimeoutMs: %d", argv.TimeoutMs)
	}

	shell := buildVsockExec(ExecRequest{Command: "sleep 1", Stream: true, TimeoutMs: 5000}, "")
	if !shell.Stream {
		t.Error("shell mode lost Stream flag")
	}
	if shell.TimeoutMs != 5000 {
		t.Errorf("shell mode lost TimeoutMs: %d", shell.TimeoutMs)
	}
}
