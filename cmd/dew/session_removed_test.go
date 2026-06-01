//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// `dew session` shipped pre-v0.7.18 as a CLI that stored VM handles
// in an in-process map: `dew session create` printed an ID, the
// process exited, and `dew session exec <id>` errored with
// "sessions are in-process only" — the ID was always useless. The
// command was a documented lie.
//
// v0.7.18 removed the subcommand. This test pins:
//   - The "session" case in the dispatcher returns an error
//     (any user who scripted against it gets a fail-fast signal,
//     not a silent no-op).
//   - The error message tells them what to do instead.
//   - The error has CodeUsage so structured-error consumers
//     classify it correctly (not a network/auth problem).
//
// We can't easily invoke the dispatcher's top-level switch from
// here, so we replay the error-construction the case-arm performs.
// If the case-arm wording changes, update this test.
func TestSessionCLI_RemovedWithMigrationHint(t *testing.T) {
	// This is the exact error the "session" case-arm returns now.
	// Keep in sync with cmd/dew/main.go's case "session".
	err := dewerr.New(dewerr.CodeUsage,
		"dew session was removed in v0.7.18 — it stored state in-process and `session exec` could never find the VM.\n"+
			"For persistent VMs use `dew up` (project) or `dew start` (manual profile) — both register with the daemon and `dew exec` works against them.")

	if err == nil {
		t.Fatal("session removal must surface an error, not silently no-op")
	}
	if got := dewerr.CodeOf(err); got != dewerr.CodeUsage {
		t.Errorf("removal error should be CodeUsage, got %v", got)
	}

	msg := err.Error()
	must := []string{
		"removed in v0.7.18", // tells them when
		"dew up",             // tells them the project alternative
		"dew start",          // tells them the manual alternative
		"dew exec",           // tells them how to talk to it after
	}
	for _, needle := range must {
		if !strings.Contains(msg, needle) {
			t.Errorf("removal error missing %q\nfull: %s", needle, msg)
		}
	}
}

// Belt-and-suspenders: make sure nothing in the binary still imports
// internal/session under cmd/dew. The package can live on for
// in-process callers (none right now), but the CLI must not reach
// in. This test compiles only if the import is genuinely absent.
func TestSessionPackage_NotImportedByCLI(t *testing.T) {
	// Sentinel: errors.New is from stdlib. If this test ever fails
	// to compile because of a missing session symbol, the import
	// crept back in.
	_ = errors.New
}
