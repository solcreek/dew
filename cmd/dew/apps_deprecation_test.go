//go:build darwin

package main

import (
	"strings"
	"testing"
)

// dew's repo independence policy says dew CHANGELOG/help/notices
// never name downstream consumer products (Marina, Grove). The
// apps deprecation notice has to point users somewhere without
// breaking that rule. This test pins the invariant — any future
// edit that drops the dew ROADMAP pointer or sneaks a product name
// in fails fast.
func TestAppsDeprecationNotice_DoesNotNameDownstreamProducts(t *testing.T) {
	out := captureStderrString(t, printAppsDeprecationNotice)

	// Must NOT name any downstream consumer product.
	for _, banned := range []string{"Marina", "Grove", "marina", "grove"} {
		if strings.Contains(out, banned) {
			t.Errorf("notice names downstream product %q — violates repo-independence policy.\nnotice:\n%s", banned, out)
		}
	}

	// Must explain what's happening + point at the dew repo so
	// users have somewhere to follow the migration.
	must := []string{
		"will move",                  // sets expectation
		"separate tool",              // generic, not a name
		"keep working",               // reassures: no immediate break
		"github.com/solcreek/dew",    // pointer to dew repo (not grove)
	}
	for _, needle := range must {
		if !strings.Contains(out, needle) {
			t.Errorf("notice missing %q\nnotice:\n%s", needle, out)
		}
	}
}

// Under --json, the notice must be suppressed so machine-readable
// output stays parseable.
func TestAppsDeprecationNotice_QuietUnderJSON(t *testing.T) {
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })

	out := captureStderrString(t, printAppsDeprecationNotice)
	if out != "" {
		t.Errorf("--json should suppress the notice; got:\n%s", out)
	}
}
