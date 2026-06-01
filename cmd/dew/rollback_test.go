//go:build darwin

package main

import (
	"strings"
	"testing"
)

// Until the deploy receiver gains version history, cmdRollback must
// fail with an honest, actionable message rather than silently calling
// the receiver's stub handler. Regression guard: anyone trying to
// resurrect the old "pretend it worked" path will break this test.
func TestRollback_NotImplementedExplicitly(t *testing.T) {
	err := cmdRollback([]string{"some-target", "some-app"})
	if err == nil {
		t.Fatal("cmdRollback returned nil; expected an error pointing at the ROADMAP")
	}
	msg := err.Error()
	for _, want := range []string{"not yet implemented", "ROADMAP", "dew deploy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\ngot: %s", want, msg)
		}
	}
}
