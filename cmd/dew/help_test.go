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
		"app",     // dew app run, dew app stop, dew app list
		"apps",    // dew apps (catalog)
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
		"app": {"--port", "--dry-run", "--json"},
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
