//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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
