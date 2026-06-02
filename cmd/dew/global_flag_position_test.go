//go:build darwin

package main

import (
	"reflect"
	"testing"
)

// `dew --json apps` used to fail with "unknown command --json" because
// the dispatcher took os.Args[1] literally without knowing about
// global flags that legitimately precede the subcommand. POSIX/GNU
// CLIs let global flags appear in either position; agents writing
// `dew --json apps | jq` is a natural pattern and must work.
//
// stripLeadingGlobalFlags is the load-bearing helper. These cases
// pin the contract.
func TestStripLeadingGlobalFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "no flags — passthrough",
			in:   []string{"apps"},
			want: []string{"apps"},
		},
		{
			// THE BUG: dispatcher saw --json as the subcommand.
			name: "single leading --json — stripped",
			in:   []string{"--json", "apps"},
			want: []string{"apps"},
		},
		{
			name: "multiple leading globals — all stripped",
			in:   []string{"--json", "--events", "apps"},
			want: []string{"apps"},
		},
		{
			name: "--dry-run before subcommand — stripped",
			in:   []string{"--dry-run", "up"},
			want: []string{"up"},
		},
		{
			name: "global flag after subcommand — left alone (parseFlags handles)",
			in:   []string{"apps", "--json"},
			want: []string{"apps", "--json"},
		},
		{
			// Don't swallow flags that take a value — those are
			// subcommand-config, not global state.
			name: "value-taking flag stops the strip (--with is not global)",
			in:   []string{"--with", "postgres", "up"},
			want: []string{"--with", "postgres", "up"},
		},
		{
			name: "only global flags, no subcommand — empty result",
			in:   []string{"--json"},
			want: []string{},
		},
		{
			name: "empty input — empty output",
			in:   []string{},
			want: []string{},
		},
		{
			name: "subcommand args after first non-flag survive",
			in:   []string{"--json", "exec", "--", "echo", "hi"},
			want: []string{"exec", "--", "echo", "hi"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripLeadingGlobalFlags(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The four global no-value flags are the ones an agent will commonly
// position before the subcommand for one-line invocations:
//   dew --json apps | jq
//   dew --events up | grep ready
// All four must be recognized.
func TestStripLeadingGlobalFlags_AllSupportedGlobals(t *testing.T) {
	for _, flag := range []string{"--json", "--events", "--stream", "--dry-run"} {
		got := stripLeadingGlobalFlags([]string{flag, "apps"})
		if len(got) != 1 || got[0] != "apps" {
			t.Errorf("%s not recognized as leading global; got %#v", flag, got)
		}
	}
}
