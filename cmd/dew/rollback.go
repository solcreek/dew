//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// cmdRollback is currently a placeholder. The server-side handler
// returns success but doesn't actually restore the previous version
// — there is no deploy history persistence yet. The CLI surfaces this
// honestly rather than silently calling the stub, and the command
// stays registered so future deploy-history work has a known entry
// point.
//
// Tracked in ROADMAP under "Restore previous version after deploy
// (rollback)".
func cmdRollback(args []string) error {
	// cmdRollback doesn't go through the shared parseFlags helper, so
	// scan args ourselves for --json. Once rollback gains real behavior
	// it'll move onto parseFlags like the other commands.
	wantJSON := flagJSON
	for _, a := range args {
		if a == "--json" {
			wantJSON = true
		}
	}
	msg := "dew rollback is not yet implemented — the deploy receiver doesn't persist version history. " +
		"Tracked in ROADMAP. Workaround: re-deploy the previous build tarball with `dew deploy <target>`."
	if wantJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":         false,
			"error":      "not_implemented",
			"message":    msg,
			"workaround": "re-deploy the previous build tarball with `dew deploy <target>`",
		})
		return fmt.Errorf("rollback not implemented")
	}
	return fmt.Errorf("%s", msg)
}
