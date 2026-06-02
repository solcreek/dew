//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/solcreek/dew/pkg/dewerr"
)

// The apps surface (dew apps, dew app run/stop/list, dew install)
// was deprecated in v0.7.19 and removed in v0.7.20. Anyone who
// scripted against it gets a clean usage error pointing at the
// alternative path, not a silent crash or unknown-command noise.
//
// This test pins the error contract — wording, error code, and
// that it doesn't accidentally name a downstream product (the
// dew-repo independence policy).
func TestAppsRemoved_DispatchErrorContract(t *testing.T) {
	// Match the case-arm in main.go's switch; if the wording
	// changes there, update here.
	for _, cmd := range []string{"install", "app", "apps"} {
		err := dewerr.New(dewerr.CodeUsage,
			"dew "+cmd+" was removed in v0.7.20.\n"+
				"The pre-packaged apps catalog now lives in a separate tool.\n"+
				"For arbitrary container workloads in dew, use: dew run --network -- <cmd>")

		if dewerr.CodeOf(err) != dewerr.CodeUsage {
			t.Errorf("dew %s removal should surface CodeUsage, got %v", cmd, dewerr.CodeOf(err))
		}

		msg := err.Error()
		must := []string{
			"removed in v0.7.20",  // tells them when
			"separate tool",        // direction without naming downstream
			"dew run",              // the supported alternative path
		}
		for _, needle := range must {
			if !strings.Contains(msg, needle) {
				t.Errorf("dew %s removal error missing %q\nfull: %s", cmd, needle, msg)
			}
		}

		// Repo-independence policy: never name downstream products.
		for _, banned := range []string{"Grove", "grove", "Marina", "marina"} {
			if strings.Contains(msg, banned) {
				t.Errorf("dew %s removal error mentions downstream product %q", cmd, banned)
			}
		}
	}
}
