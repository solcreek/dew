//go:build darwin

package main

import (
	"os"
	"strings"
	"testing"
)

// `dew up` in a dir with no detectable project (no package.json, no
// requirements.txt, etc.) used to surface a flat
// "no supported project detected in <dir>" error with no suggested
// next step. Beginners bounce off this; agents can't programmatically
// recover. The new error must:
//   1) Tag with code `no_project_detected` so agents can grep it
//   2) Suggest at least three concrete next-step commands
//   3) Point at the docs URL
func TestCmdUp_EmptyDir_SurfacesHelpfulOptions(t *testing.T) {
	tmp, err := os.MkdirTemp("", "dew-up-empty-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	err = cmdUp([]string{tmp})
	if err == nil {
		t.Fatalf("expected error for empty dir, got nil")
	}
	msg := err.Error()

	// Every suggested command must work today — never point at a planned
	// command that doesn't yet exist (broken-suggestion → trust tax).
	// `dew shell` lands later; the suggestion list links to it then.
	must := []struct {
		needle string
		why    string
	}{
		{"no_project_detected", "agents need a grep-able error code"},
		{"dew up --profile minimal", "explicit-profile escape hatch"},
		{"dew start --profile minimal", "decouple boot from project context"},
		{"dew app run", "another concrete next-step command"},
		{"https://dewvm.dev/start", "docs link for full context"},
	}
	// Negative assertion: don't suggest planned-but-unbuilt commands.
	for _, banned := range []string{"dew shell"} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("error message mentions planned-but-unbuilt command %q — fix the suggestion list", banned)
		}
	}
	for _, m := range must {
		if !strings.Contains(msg, m.needle) {
			t.Errorf("error missing %q (%s)\nfull error:\n%s", m.needle, m.why, msg)
		}
	}
}
