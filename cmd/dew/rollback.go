//go:build darwin

package main

import (
	"github.com/solcreek/dew/pkg/dewerr"
)

// cmdRollback is currently a placeholder. The server-side handler
// returns a fake success without actually restoring any prior version
// — there is no deploy history persistence yet. The CLI surfaces
// this honestly rather than calling the stub, and the command stays
// registered so future deploy-history work has a known entry point.
//
// Tracked in ROADMAP under "Restore previous version after deploy
// (rollback)".
func cmdRollback(args []string) error {
	// Flag-scan for --json so the global flagJSON is set before main's
	// error handler runs. (cmdRollback doesn't go through parseFlags.)
	for _, a := range args {
		if a == "--json" {
			flagJSON = true
		}
	}
	err := dewerr.New(dewerr.CodeGeneric,
		"dew rollback is not yet implemented — the deploy receiver doesn't persist version history. "+
			"Tracked in ROADMAP. Workaround: re-deploy the previous build tarball with `dew deploy <target>`.")
	err.Hint = map[string]string{
		"reason":     "not_implemented",
		"workaround": "re-deploy the previous build tarball",
		"tracking":   "https://github.com/solcreek/dew/blob/main/ROADMAP.md",
	}
	return err
}
