//go:build darwin

package main

import (
	"fmt"
	"os"
)

// appsDeprecationNotice is printed once per apps-related invocation.
//
// dew is settling on its identity as a sandboxed Linux compute
// primitive. The pre-packaged apps catalog is a different product
// shape (curated app installer for non-developers) and is moving
// to a standalone tool. Existing functionality keeps working through
// the deprecation window — no behavior break for current users.
//
// The notice deliberately does not name the target tool, per the
// dew-repo independence policy: dew's repo / changelog never names
// downstream consumer products. The "see ... for migration details"
// hint points at the dew repo itself, where the ROADMAP will track
// timing.
//
// Suppressed when --json is set so machine-readable output stays
// parseable.
func printAppsDeprecationNotice() {
	if flagJSON {
		return
	}
	fmt.Fprintln(os.Stderr, "dew: the pre-packaged apps catalog will move to a separate tool in a future release.")
	fmt.Fprintln(os.Stderr, "     Existing apps keep working until then; see github.com/solcreek/dew ROADMAP for details.")
}
