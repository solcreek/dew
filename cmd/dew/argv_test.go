//go:build darwin

package main

import (
	"reflect"
	"testing"
)

// argvOrShellWrap is the load-bearing rule that fixes the
// long-standing sh -c double-wrap bug. Pinning all the cases
// the bug report and the rewrite needed it to handle.
func TestArgvOrShellWrap(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantCommand string
		wantArgs    []string
	}{
		{
			name:        "empty input returns empty pair",
			in:          nil,
			wantCommand: "",
			wantArgs:    nil,
		},
		{
			// `dew run "echo a; echo b"` — single arg with shell
			// metachars. The user implicitly expects shell parsing.
			name:        "single-arg keeps legacy shell wrap",
			in:          []string{"echo a; echo b"},
			wantCommand: "/bin/sh",
			wantArgs:    []string{"-c", "echo a; echo b"},
		},
		{
			// `dew run -- echo hi` — argv form, no shell needed.
			name:        "two-arg uses argv direct",
			in:          []string{"echo", "hi"},
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
		},
		{
			// THE BUG: `dew run -- sh -c 'echo A; echo B'`.
			// Old behavior joined to "sh -c echo A; echo B" then
			// wrapped: `/bin/sh -c "sh -c echo A; echo B"` →
			// outer shell parses, the "echo A" loses its arg, the
			// "; echo B" becomes a separate command. Output is
			// "\nB\n" instead of "A\nB\n".
			//
			// The fix: with 3 args, pass them straight as argv —
			// the agent runs sh -c "echo A; echo B" directly.
			name:        "user-supplied sh -c is not double-wrapped",
			in:          []string{"sh", "-c", "echo A; echo B"},
			wantCommand: "sh",
			wantArgs:    []string{"-c", "echo A; echo B"},
		},
		{
			name:        "user-supplied bash -c is not double-wrapped",
			in:          []string{"bash", "-lc", "echo $PATH"},
			wantCommand: "bash",
			wantArgs:    []string{"-lc", "echo $PATH"},
		},
		{
			// Mixed shell + non-shell — argv all the way through.
			name:        "argv with flags and operands",
			in:          []string{"ls", "-la", "/tmp"},
			wantCommand: "ls",
			wantArgs:    []string{"-la", "/tmp"},
		},
		{
			// Important: spaces in argv elements must be preserved
			// verbatim. The old join-then-split would have split
			// "hello world" into two argv slots.
			name:        "argv elements with spaces are preserved verbatim",
			in:          []string{"echo", "hello world", "and another"},
			wantCommand: "echo",
			wantArgs:    []string{"hello world", "and another"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args := argvOrShellWrap(tc.in)
			if cmd != tc.wantCommand {
				t.Errorf("command = %q, want %q", cmd, tc.wantCommand)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tc.wantArgs)
			}
		})
	}
}

// The shell-wrap path must produce an ExecRequest the guest agent
// can interpret. We don't have a guest in this test, but we can
// assert the (command, args) pair is what the agent's
// exec.CommandContext expects.
func TestArgvOrShellWrap_ProducesValidExecPair(t *testing.T) {
	// For the user-supplied sh -c case, the agent will call
	// exec.CommandContext("sh", "-c", "echo A; echo B"). That is
	// equivalent to running `sh -c 'echo A; echo B'` directly,
	// which prints "A\nB\n" — the correct behavior.
	cmd, args := argvOrShellWrap([]string{"sh", "-c", "echo A; echo B"})
	if cmd != "sh" {
		t.Fatalf("argv direct: command should be the user's program literally; got %q", cmd)
	}
	// The args MUST be exactly what the user wrote — any
	// re-joining or re-splitting breaks shell-quoted strings.
	if len(args) != 2 || args[0] != "-c" || args[1] != "echo A; echo B" {
		t.Fatalf("args reshaped: got %#v", args)
	}
}
