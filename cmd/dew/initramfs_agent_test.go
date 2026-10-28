//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBuildScriptPreamble runs a copy of build.sh with DEW_AGENT set (unset
// when agent is "") and a PATH holding only the tools its argument checks
// use. Past the checks the script reaches its first `rm` and dies there
// (command not found under set -e), so the test can't delete or download
// anything even if a check stops exiting.
func runBuildScriptPreamble(t *testing.T, agent string) (string, error) {
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
	cmd := exec.Command(bash, filepath.Join(initramfs, "build.sh"), "minimal")
	cmd.Env = []string{"PATH=" + bin}
	if agent != "" {
		cmd.Env = append(cmd.Env, "DEW_AGENT="+agent)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// DEW_AGENT selects the guest agent; an unknown value must stop the build
// before it starts (before WORK_DIR is wiped), and go, rust and the default
// must get past the check.
func TestInitramfsBuildScript_ValidatesDewAgent(t *testing.T) {
	out, err := runBuildScriptPreamble(t, "bogus")
	if err == nil {
		t.Fatalf("DEW_AGENT=bogus did not fail:\n%s", out)
	}
	if !strings.Contains(out, "unsupported DEW_AGENT: bogus") {
		t.Errorf("DEW_AGENT=bogus: no clear error:\n%s", out)
	}
	if strings.Contains(out, "=== Building") {
		t.Errorf("DEW_AGENT=bogus: the build started before the check:\n%s", out)
	}

	for _, agent := range []string{"", "go", "rust"} {
		out, _ := runBuildScriptPreamble(t, agent)
		if strings.Contains(out, "unsupported DEW_AGENT") || !strings.Contains(out, "=== Building") {
			t.Errorf("DEW_AGENT=%q was not accepted:\n%s", agent, out)
		}
	}
}

// Both agents must land at the path init-stage2 starts, built for the target
// arch: Go via GOARCH, Rust via the musl target the toolchain file pins.
func TestInitramfsBuildScript_BuildsTheSelectedAgent(t *testing.T) {
	script := readBuildScript(t)
	if !strings.Contains(script, `DEW_AGENT="${DEW_AGENT:-go}"`) {
		t.Error("DEW_AGENT does not default to go")
	}
	start := strings.Index(script, "# --- Step 3: Build dew-agent ---")
	end := strings.Index(script, "# --- Step 4a:")
	if start < 0 || end < start {
		t.Fatal("could not locate the dew-agent build step — test needs updating")
	}
	step := script[start:end]

	goIdx := strings.Index(step, "    go)")
	if goIdx < 0 {
		t.Fatal("no go) arm in the agent step")
	}
	goArm := step[goIdx:]
	rustIdx := strings.Index(goArm, "    rust)")
	if rustIdx < 0 {
		t.Fatal("no rust) arm after the go) arm in the agent step")
	}
	goArm, rustArm := goArm[:rustIdx], goArm[rustIdx:]
	for _, want := range []string{`GOARCH="$GO_ARCH"`, "CGO_ENABLED=0", `-o "${WORK_DIR}/usr/local/bin/dew-agent"`, `"${REPO_DIR}/cmd/dew-agent/"`} {
		if !strings.Contains(goArm, want) {
			t.Errorf("go agent build lost %q", want)
		}
	}
	for _, want := range []string{
		`RUST_TARGET="${ALPINE_ARCH}-unknown-linux-musl"`,
		`cd "${REPO_DIR}/rust"`,
		`cargo build --locked --release -p dew-agent --target "$RUST_TARGET"`,
		`"${CARGO_TARGET_DIR:-target}/${RUST_TARGET}/release/dew-agent"`,
		`"${WORK_DIR}/usr/local/bin/dew-agent"`,
	} {
		if !strings.Contains(rustArm, want) {
			t.Errorf("rust agent build lost %q", want)
		}
	}
	// The init script starts whatever is at this path; neither arm may move it.
	if !strings.Contains(script, "exec env $AGENT_ENV /usr/local/bin/dew-agent") {
		t.Error("init-stage2 no longer starts /usr/local/bin/dew-agent — update the agent step's install path with it")
	}
}
