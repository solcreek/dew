//go:build darwin

package main

import (
	"strings"
	"testing"
)

// The standard profile bakes a hardening toolbox so a hardened systemd
// unit's uid/caps/rlimit effects are reproducible by hand (the BusyBox
// setpriv in minimal cannot express --reuid/--bounding-set). These guards
// keep the package set in build.sh from silently regressing; the names
// themselves are validated by the Linux CI build (set -euo pipefail makes a
// bad package abort the build).
func TestInitramfsBuildScript_StandardHardeningToolbox(t *testing.T) {
	script := readBuildScript(t)

	idx := strings.Index(script, `if [ "$PROFILE" = "standard" ]; then`)
	if idx < 0 {
		t.Fatal("no standard-only block found for the hardening toolbox")
	}
	// Look at the standard-only block, not the whole script, so a match in
	// some unrelated section can't mask a missing install line.
	block := script[idx:]
	if end := strings.Index(block[len(`if [ "$PROFILE" = "standard" ]; then`):], "\nfi\n"); end >= 0 {
		block = block[:len(`if [ "$PROFILE" = "standard" ]; then`)+end]
	}

	for _, pkg := range []string{"setpriv", "util-linux-misc", "libcap", "iproute2"} {
		if !strings.Contains(block, pkg) {
			t.Errorf("standard profile does not bake %q into the hardening toolbox", pkg)
		}
	}
	if !strings.Contains(block, "apk_install_pkgs") {
		t.Error("standard hardening block never calls apk_install_pkgs")
	}
}
