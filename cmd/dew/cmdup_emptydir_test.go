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

	must := []struct {
		needle string
		why    string
	}{
		{"no_project_detected", "agents need a grep-able error code"},
		{"dew shell", "beginners want a generic VM"},
		{"dew app run", "another concrete next-step command"},
		{"dew up --profile minimal", "explicit-profile escape hatch"},
		{"https://dewvm.dev/start", "docs link for full context"},
	}
	for _, m := range must {
		if !strings.Contains(msg, m.needle) {
			t.Errorf("error missing %q (%s)\nfull error:\n%s", m.needle, m.why, msg)
		}
	}
}
