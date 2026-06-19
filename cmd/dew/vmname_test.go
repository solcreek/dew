//go:build darwin

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// validateVMName is the security boundary: the name becomes a path
// component (<name>.sock, <name>/ state dir) and a front door may feed
// it an untrusted SSH username, so traversal and odd characters must be
// rejected — not sanitized.
func TestValidateVMName(t *testing.T) {
	good := []string{"alice", "bob-1", "ci_runner", "A1", strings.Repeat("x", 64)}
	for _, n := range good {
		if err := validateVMName(n); err != nil {
			t.Errorf("validateVMName(%q) = %v, want nil", n, err)
		}
	}

	bad := []string{
		"",                      // empty
		"default",               // reserved alias for the unnamed VM
		"../etc",                // traversal
		"a/b",                   // path separator
		"alice.sock",            // dot
		"alice bob",             // space
		"naïve",                 // non-ASCII
		strings.Repeat("x", 65), // too long
	}
	for _, n := range bad {
		err := validateVMName(n)
		if err == nil {
			t.Errorf("validateVMName(%q) = nil, want error", n)
			continue
		}
		if got := dewerr.CodeOf(err); got != dewerr.CodeUsage {
			t.Errorf("validateVMName(%q) code = %v, want CodeUsage", n, got)
		}
	}
}

func TestPopNameFlag(t *testing.T) {
	// Extracts the pair, strips it from args, sets the global.
	rest, err := popNameFlag([]string{"add", "--name", "alice", "5432:5432"})
	if err != nil {
		t.Fatal(err)
	}
	if flagVMName != "alice" {
		t.Errorf("flagVMName = %q, want alice", flagVMName)
	}
	if strings.Join(rest, " ") != "add 5432:5432" {
		t.Errorf("rest = %v, want [add 5432:5432]", rest)
	}

	// Absent --name resets the global to the default VM, so a name can't
	// leak from a previous command in a reused process.
	rest, err = popNameFlag([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	if flagVMName != "" {
		t.Errorf("flagVMName = %q, want empty", flagVMName)
	}
	if strings.Join(rest, " ") != "status" {
		t.Errorf("rest = %v, want [status]", rest)
	}

	// A dangling --name is a usage error, not a silent no-op.
	if _, err := popNameFlag([]string{"--name"}); err == nil {
		t.Error("trailing --name should error")
	} else if dewerr.CodeOf(err) != dewerr.CodeUsage {
		t.Errorf("code = %v, want CodeUsage", dewerr.CodeOf(err))
	}

	// An invalid name is rejected here too (front-door safety).
	if _, err := popNameFlag([]string{"--name", "../x"}); err == nil {
		t.Error("invalid --name should error")
	}
}

func TestParseFlags_Name(t *testing.T) {
	if _, _, err := parseFlags([]string{"--name", "alice", "--profile", "minimal"}); err != nil {
		t.Fatal(err)
	}
	if flagVMName != "alice" {
		t.Errorf("flagVMName = %q, want alice", flagVMName)
	}
	// parseFlags resets command-scoped globals, so a later invocation
	// without --name falls back to the default VM.
	if _, _, err := parseFlags([]string{"--profile", "minimal"}); err != nil {
		t.Fatal(err)
	}
	if flagVMName != "" {
		t.Errorf("flagVMName = %q after nameless parse, want empty", flagVMName)
	}
}

// `dew vm status --name alice` must probe alice.sock, not the default.
func TestVMStatus_NamedSocketRouting(t *testing.T) {
	_ = withTempSocketPath(t) // sets HOME → temp state dir
	flagJSON = true
	defer func() { flagJSON = false; flagVMName = "" }()

	out := captureStdout(t, func() {
		_ = cmdStatus([]string{"--name", "alice"})
	})

	var env struct {
		Data struct {
			Running bool   `json:"running"`
			Socket  string `json:"socket"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}
	if !strings.HasSuffix(env.Data.Socket, filepath.Join("dew", "alice.sock")) {
		t.Errorf("socket = %q, want it to end in dew/alice.sock", env.Data.Socket)
	}
	if env.Data.Running {
		t.Error("named VM reported running with no listener")
	}
}

// Isolation: a live listener on the DEFAULT socket must not make a named
// VM look running — they are distinct sockets.
func TestVMStatus_NamedIsolatedFromDefault(t *testing.T) {
	defSock := withTempSocketPath(t)
	if err := os.MkdirAll(filepath.Dir(defSock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", defSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
	defer func() { flagVMName = "" }()

	// Default VM sees the listener → running.
	defOut := captureStdout(t, func() { _ = cmdStatus(nil) })
	if strings.Contains(defOut, "not running") {
		t.Errorf("default VM should be running: %q", defOut)
	}
	// Named VM has its own (absent) socket → not running.
	namedOut := captureStdout(t, func() { _ = cmdStatus([]string{"--name", "alice"}) })
	if !strings.Contains(namedOut, "not running") {
		t.Errorf("named VM should be independent of default's listener: %q", namedOut)
	}
}
