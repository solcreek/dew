//go:build darwin

package main

import (
	"strings"
	"testing"
)

// The fresh-eyes agent report flagged "no per-subcommand help" as
// the single biggest discoverability barrier — four agents
// independently bounced off `dew up --help` returning "unknown flag"
// and could not find how to override the dev server port. Every
// user-facing subcommand should now have a help block.
func TestSubcommandHelp_CoversUserFacingCommands(t *testing.T) {
	mustHave := []string{
		"up",      // P0 from the report
		"run",     // P0
		"exec",    // mentioned alongside up/run
		"start",
		"down",
		"build",
		"deploy",
		"share",
		// "app" / "apps" removed in v0.7.20 with the apps surface itself
	}
	for _, name := range mustHave {
		t.Run(name, func(t *testing.T) {
			text, ok := subcommandHelp[name]
			if !ok {
				t.Fatalf("dew %s --help has no registered help text", name)
			}
			if len(text) < 100 {
				t.Errorf("help for %q is suspiciously short (%d chars) — was it stubbed?", name, len(text))
			}
			if !strings.Contains(text, "Usage:") {
				t.Errorf("help for %q missing Usage: section", name)
			}
		})
	}
}

// Each help block names every flag the corresponding command
// supports. We don't enforce this for ALL flags, just the ones the
// report specifically named as undiscoverable. Regression guard.
func TestSubcommandHelp_FlagsTheReportFlagged(t *testing.T) {
	cases := map[string][]string{
		"up":  {"--dry-run", "--with", "--profile", "--json"},
		"run": {"--network", "--share", "--json", "--profile"},
	}
	for cmd, flags := range cases {
		text := subcommandHelp[cmd]
		for _, f := range flags {
			if !strings.Contains(text, f) {
				t.Errorf("dew %s --help missing flag %q from doc", cmd, f)
			}
		}
	}
}

// The fresh-eyes report flagged two doc gaps the agent had to learn
// the hard way: dew run's ephemeral semantics (state doesn't persist
// between invocations) and the /app mount path that dew up uses. The
// per-subcommand help now states both explicitly. This test pins the
// docs so a future refactor doesn't silently drop them.
func TestSubcommandHelp_EphemeralAndMountPathsDocumented(t *testing.T) {
	runHelp := subcommandHelp["run"]
	for _, must := range []string{
		"ephemeral",       // explicit word — agents grep this
		"DO NOT persist",  // concrete consequence
		"dew vm start",    // pointer to the persistent alternative
		"dew exec",        // and how to use it
	} {
		if !strings.Contains(runHelp, must) {
			t.Errorf("dew run --help missing ephemeral-state hint %q\nhelp:\n%s", must, runHelp)
		}
	}

	upHelp := subcommandHelp["up"]
	for _, must := range []string{
		"/app",         // the mount path itself
		"virtiofs",     // the mechanism (so technically-curious users know)
	} {
		if !strings.Contains(upHelp, must) {
			t.Errorf("dew up --help missing /app mount-path doc %q\nhelp:\n%s", must, upHelp)
		}
	}
}

// printSubcommandHelp returns true when it found a registered block,
// false otherwise. The dispatcher relies on this signal to decide
// whether to short-circuit or fall through to the regular command
// handler.
func TestPrintSubcommandHelp_ReturnFlag(t *testing.T) {
	if !printSubcommandHelp("up") {
		t.Error("known subcommand should print and return true")
	}
	if printSubcommandHelp("not-a-real-command") {
		t.Error("unknown subcommand should return false")
	}
}
