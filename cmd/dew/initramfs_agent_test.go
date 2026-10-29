//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBuildScriptPreamble runs a copy of build.sh for profile with DEW_AGENT
// set (unset when agent is "") and a PATH holding only the tools its
// argument checks use. Past the checks the script reaches its first `rm` and dies there
// (command not found under set -e), so the test can't delete or download
// anything even if a check stops exiting.
func runBuildScriptPreamble(t *testing.T, profile, agent string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	initramfs := filepath.Join(dir, "initramfs")
	if err := os.Mkdir(initramfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(initramfs, "build.sh"), []byte(readBuildScript(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "uname", "dirname"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	bash := filepath.Join(bin, "bash")
	cmd := exec.Command(bash, filepath.Join(initramfs, "build.sh"), profile)
	cmd.Env = []string{"PATH=" + bin}
	if agent != "" {
		cmd.Env = append(cmd.Env, "DEW_AGENT="+agent)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The Go agent is gone: DEW_AGENT=go (or anything but rust) must stop the
// build before it starts (before WORK_DIR is wiped) instead of quietly
// baking the Rust agent, and unset or rust must get past the check.
func TestInitramfsBuildScript_ValidatesDewAgent(t *testing.T) {
	for _, agent := range []string{"go", "bogus"} {
		out, err := runBuildScriptPreamble(t, "minimal", agent)
		if err == nil {
			t.Fatalf("DEW_AGENT=%s did not fail:\n%s", agent, out)
		}
		if !strings.Contains(out, "unsupported DEW_AGENT: "+agent) {
			t.Errorf("DEW_AGENT=%s: no clear error:\n%s", agent, out)
		}
		if strings.Contains(out, "=== Building") {
			t.Errorf("DEW_AGENT=%s: the build started before the check:\n%s", agent, out)
		}
	}

	for _, agent := range []string{"", "rust"} {
		out, _ := runBuildScriptPreamble(t, "minimal", agent)
		if strings.Contains(out, "unsupported DEW_AGENT") || !strings.Contains(out, "=== Building") {
			t.Errorf("DEW_AGENT=%q was not accepted:\n%s", agent, out)
		}
	}
}

// The agent must land at the path init-stage2 starts, built for the target
// arch via the musl target the toolchain file pins.
func TestInitramfsBuildScript_BuildsTheAgent(t *testing.T) {
	script := readBuildScript(t)
	start := strings.Index(script, "# --- Step 3: Build dew-agent ---")
	end := strings.Index(script, "# --- Step 4a:")
	if start < 0 || end < start {
		t.Fatal("could not locate the dew-agent build step — test needs updating")
	}
	step := script[start:end]

	for _, want := range []string{
		`RUST_TARGET="${ALPINE_ARCH}-unknown-linux-musl"`,
		`cd "${REPO_DIR}/rust"`,
		`cargo build --locked --release -p dew-agent --target "$RUST_TARGET"`,
		`"${CARGO_TARGET_DIR:-target}/${RUST_TARGET}/release/dew-agent"`,
		`"${WORK_DIR}/usr/local/bin/dew-agent"`,
	} {
		if !strings.Contains(step, want) {
			t.Errorf("rust agent build lost %q", want)
		}
	}
	// The init script starts whatever is at this path; the build may not move it.
	if !strings.Contains(script, "exec env $AGENT_ENV /usr/local/bin/dew-agent") {
		t.Error("init-stage2 no longer starts /usr/local/bin/dew-agent — update the agent step's install path with it")
	}
}
