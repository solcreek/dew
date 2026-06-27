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

	// The toolbox is folded into the node/standard apk transaction: a base
	// PKGS list augmented for the standard profile and installed via one
	// apk_install_pkgs $PKGS call (so the standard build fetches the index
	// once rather than running a second transaction).
	idx := strings.Index(script, `PKGS="nodejs npm"`)
	if idx < 0 {
		t.Fatal("no PKGS base list found for the node/standard apk transaction")
	}
	// Scope to the apk transaction block (up to its first closing fi) so a
	// match elsewhere can't mask a missing install line.
	block := script[idx:]
	if end := strings.Index(block, "\nfi\n"); end >= 0 {
		block = block[:end]
	}

	if !strings.Contains(block, `[ "$PROFILE" = "standard" ] && PKGS=`) {
		t.Error("hardening toolbox is not gated on the standard profile")
	}
	for _, pkg := range []string{"setpriv", "util-linux-misc", "libcap", "iproute2"} {
		if !strings.Contains(block, pkg) {
			t.Errorf("standard profile does not bake %q into the hardening toolbox", pkg)
		}
	}
	if !strings.Contains(block, "apk_install_pkgs") || !strings.Contains(block, "$PKGS") {
		t.Error("hardening block never installs the augmented $PKGS via apk_install_pkgs")
	}
}
