//go:build darwin

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// captureStdoutString runs fn with os.Stdout redirected to a pipe and
// returns whatever fn wrote there.
func captureStdoutString(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var out []byte
	go func() {
		out, _ = io.ReadAll(r)
		close(done)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	<-done
	return string(out)
}

// `dew up --dry-run` used to be a silent no-op: the flag was parsed,
// the rest of the code ignored it, and the VM booted anyway. Help
// text claimed "Validate without executing." This test pins the new
// behavior: when --dry-run is set, detection runs, the plan is
// emitted, and the function returns without booting.
//
// We can't prove "VM didn't boot" directly here without hooking the
// darwin backend, but cmdUp's boot path is well after the dry-run
// short-circuit; a regression would either error out (couldn't load
// kernel) or hang waiting for assets. The function returning nil
// in <1 s against a fresh tempdir is the proxy.
func TestCmdUp_DryRun_DoesNotBoot(t *testing.T) {
	dir := t.TempDir()
	// Minimal vite project — enough for detect.Detect to return a
	// framework, so we proceed past the empty-dir guard.
	writePkg(t, dir, `{
  "name": "dryrun-test",
  "dependencies": { "vite": "5.0.0" },
  "scripts": { "dev": "vite" }
}`)

	flagDryRun = true
	flagJSON = true
	t.Cleanup(func() {
		flagDryRun = false
		flagJSON = false
	})

	var err error
	out := captureStdoutString(t, func() {
		err = cmdUp([]string{dir})
	})
	if err != nil {
		t.Fatalf("dry-run should succeed cleanly, got %v", err)
	}

	// Find the dry-run line in the output (detect event may emit first).
	var plan map[string]any
	for _, line := range splitLines(out) {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if t, _ := m["type"].(string); t == "dry-run" {
			plan = m
			break
		}
	}
	if plan == nil {
		t.Fatalf("no dry-run event in output:\n%s", out)
	}

	must := []string{"project_dir", "framework", "profile", "would_boot"}
	for _, key := range must {
		if _, ok := plan[key]; !ok {
			t.Errorf("dry-run plan missing %q\nplan: %+v", key, plan)
		}
	}
	if got := plan["would_boot"]; got != false {
		t.Errorf("would_boot must be false, got %v", got)
	}
	if got := plan["framework"]; got != "vite" {
		t.Errorf("framework should be vite, got %v", got)
	}

	// The plan must point at an absolute path so agents don't have
	// to second-guess what "." meant.
	pd, _ := plan["project_dir"].(string)
	if !filepath.IsAbs(pd) {
		t.Errorf("project_dir should be absolute, got %q", pd)
	}
}

// Empty-dir + --dry-run should STILL surface the no-project error.
// Dry-run doesn't bypass detection — it only short-circuits boot
// after detection succeeds.
func TestCmdUp_DryRun_EmptyDirStillErrors(t *testing.T) {
	dir := t.TempDir()
	flagDryRun = true
	t.Cleanup(func() { flagDryRun = false })

	err := cmdUp([]string{dir})
	if err == nil {
		t.Fatal("dry-run on an empty dir should still error")
	}
}

// `dew up <dir> --dry-run` (flag AFTER positional) used to silently
// drop the flag because parseFlags stopped at the first non-flag.
// This pins the fix: flags appearing after the dir still take effect.
func TestParseFlags_DryRunAfterPositional(t *testing.T) {
	flagDryRun = false
	t.Cleanup(func() { flagDryRun = false })

	_, remaining, err := parseFlags([]string{"/tmp/some-dir", "--dry-run"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !flagDryRun {
		t.Fatal("--dry-run after positional was silently dropped")
	}
	if len(remaining) == 0 || remaining[0] != "/tmp/some-dir" {
		t.Fatalf("positional should still be returned, got %v", remaining)
	}
}

// Other known flags should also survive being placed after the dir.
func TestParseFlags_JSONAfterPositional(t *testing.T) {
	flagJSON = false
	t.Cleanup(func() { flagJSON = false })

	_, _, err := parseFlags([]string{"/tmp/some-dir", "--json"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !flagJSON {
		t.Fatal("--json after positional was silently dropped")
	}
}

// Unknown flag AFTER positional should still error (regression that
// the post-positional scan isn't a silent permit-all).
func TestParseFlags_UnknownFlagAfterPositionalErrors(t *testing.T) {
	_, _, err := parseFlags([]string{"/tmp/some-dir", "--nope"})
	if err == nil {
		t.Fatal("unknown flag after positional should error")
	}
}

func writePkg(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
