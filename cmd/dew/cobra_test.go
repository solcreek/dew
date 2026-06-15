//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// cobraCommands (what main() routes to cobra) and the commands actually
// registered on the root must agree exactly. A command on the root but
// missing from cobraCommands silently 404s ("unknown command"); the
// reverse means dispatch points at a non-existent command. This guards
// the drift class that let `dew --version` regress.
func TestCobraCommands_MatchRoot(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range newRootCmd().Commands() {
		registered[c.Name()] = true
		for _, a := range c.Aliases {
			registered[a] = true
		}
	}
	for name := range cobraCommands {
		if !registered[name] {
			t.Errorf("cobraCommands has %q but it isn't registered on the cobra root (dispatch would 404 it)", name)
		}
	}
	for name := range registered {
		if !cobraCommands[name] {
			t.Errorf("root registers %q but it's missing from cobraCommands (main() would never route to it)", name)
		}
	}
}

// passthroughCommands must be a subset of cobraCommands — a passthrough
// command not routed to cobra can't have its pre-scan limited correctly.
func TestPassthroughCommands_SubsetOfCobra(t *testing.T) {
	for name := range passthroughCommands {
		if !cobraCommands[name] {
			t.Errorf("passthroughCommands has %q but it's not in cobraCommands", name)
		}
	}
}

// For passthrough commands, main()'s global flag pre-scan must stop at
// the subcommand so a guest command's own --json isn't read as dew's.
func TestGlobalFlagScanArgs(t *testing.T) {
	// dew exec curl --json url  → leading globals only (none here).
	all := []string{"exec", "curl", "--json", "url"}
	dispatch := []string{"exec", "curl", "--json", "url"}
	if got := globalFlagScanArgs(all, dispatch, true); len(got) != 0 {
		t.Errorf("passthrough scan = %v, want [] (guest --json not scanned)", got)
	}
	// dew --json exec curl → leading --json IS scanned.
	all = []string{"--json", "exec", "curl"}
	dispatch = []string{"exec", "curl"}
	got := globalFlagScanArgs(all, dispatch, true)
	if strings.Join(got, " ") != "--json" {
		t.Errorf("leading scan = %v, want [--json]", got)
	}
	// Non-passthrough (e.g. share): scan everything (position-independent).
	all = []string{"share", "--json", "3000"}
	dispatch = []string{"share", "--json", "3000"}
	if got := globalFlagScanArgs(all, dispatch, false); len(got) != 3 {
		t.Errorf("non-passthrough scan = %v, want all 3 args", got)
	}
}

// The whole point of migrating exec to cobra with SetInterspersed(false)
// is that a guest command's own flags pass through untouched. This locks
// that contract at the parse layer (no VM needed): we swap RunE to
// capture what reached the command body.
func TestExecCmd_Passthrough(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantArgs []string
		wantJSON bool
		wantErr  bool
	}{
		{"guest flags pass through", []string{"ss", "-tlnp"}, []string{"ss", "-tlnp"}, false, false},
		{"dew --json before cmd", []string{"--json", "ss", "-tlnp"}, []string{"ss", "-tlnp"}, true, false},
		{"dew --timeout before cmd", []string{"--timeout", "5s", "ss", "-tlnp"}, []string{"ss", "-tlnp"}, false, false},
		{"guest --json not eaten", []string{"curl", "--json", "https://x"}, []string{"curl", "--json", "https://x"}, false, false},
		{"-- escapes a dash-leading cmd", []string{"--", "-tlnp"}, []string{"-tlnp"}, false, false},
		{"no args is a usage error", []string{}, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			var gotJSON bool
			c := newExecCmd()
			c.RunE = func(cmd *cobra.Command, args []string) error {
				gotArgs = args
				gotJSON, _ = cmd.Flags().GetBool("json")
				return nil
			}
			c.SetArgs(tc.in)
			err := c.Execute()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a usage error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(gotArgs, " ") != strings.Join(tc.wantArgs, " ") {
				t.Errorf("args = %v, want %v", gotArgs, tc.wantArgs)
			}
			if gotJSON != tc.wantJSON {
				t.Errorf("--json = %v, want %v", gotJSON, tc.wantJSON)
			}
		})
	}
}
