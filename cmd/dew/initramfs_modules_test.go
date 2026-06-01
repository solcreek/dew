//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Regression guard for the standard-profile CNI bridge plugin: nerdctl fails
// with `could not add "nerdctl0": operation not supported` if any of these
// kernel modules aren't loaded before containerd starts.
func TestInitramfsBuildScript_LoadsCNIBridgeModules(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	containerdIdx := strings.Index(script, "if [ -x /usr/local/bin/containerd ]")
	if containerdIdx == -1 {
		t.Fatal("could not locate containerd startup block in build.sh — test needs updating")
	}
	endIdx := strings.Index(script[containerdIdx:], "INIT2_EOF")
	if endIdx == -1 {
		t.Fatal("could not locate end of init-stage2 heredoc")
	}
	containerdBlock := script[containerdIdx : containerdIdx+endIdx]

	required := []string{
		"modprobe bridge",        // creates the nerdctl0 bridge
		"modprobe br_netfilter",  // sysctl bridge-nf-call-iptables=1 (set by CNI)
		"modprobe veth",          // container-side network pair
		"modprobe iptable_nat",   // masquerade chain
		"modprobe nf_nat",        // NAT engine
		"modprobe xt_MASQUERADE", // -j MASQUERADE rule
	}
	for _, want := range required {
		if !strings.Contains(containerdBlock, want) {
			t.Errorf("init-stage2 containerd block missing %q — CNI bridge networking will fail in standard profile", want)
		}
	}
}

// /lib/modules on the persistent disk must be refreshed from the initramfs on
// every boot, not just first boot. Otherwise a kernel-APK bump in the
// initramfs leaves stale modules behind, every modprobe silently fails, and
// the user sees mystery "operation not supported" errors from CNI/containerd.
func TestInitramfsBuildScript_RefreshesKernelModulesEveryBoot(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	// Sentinel comment marks the always-on refresh block. Find it, then verify
	// it lives OUTSIDE the first-boot-only branch (i.e., not gated by
	// `! -f /mnt/root/.dew-initialized`).
	marker := "DEW_REFRESH_KMODULES"
	markerIdx := strings.Index(script, marker)
	if markerIdx == -1 {
		t.Fatalf("init script missing %q marker for the always-on module refresh block", marker)
	}
	firstBootIdx := strings.Index(script, "! -f /mnt/root/.dew-initialized")
	endFirstBoot := strings.Index(script[firstBootIdx:], "    fi\n")
	if firstBootIdx == -1 || endFirstBoot == -1 {
		t.Fatal("could not locate first-boot block boundaries — test needs updating")
	}
	firstBootEnd := firstBootIdx + endFirstBoot
	if markerIdx > firstBootIdx && markerIdx < firstBootEnd {
		t.Errorf("module refresh is inside the first-boot block — must run on every boot to avoid kernel/module drift")
	}
}

// Alpine 3.21's linux-virt aarch64 kernel ships in EFI zboot format
// (PE32+ wrapper + gzip payload). Apple VZ on Apple Silicon rejects it
// with VZErrorDomain Code=1 — it requires a raw ARM64 Image. This
// regressed dew silently for every Apple Silicon brew user on v0.7.7–9
// because Code-Hex/vz forwards the file straight to VZ without stripping
// the wrapper. build.sh now detects and extracts; this test catches a
// future revert.
func TestInitramfsBuildScript_StripsAArch64ZBootWrapper(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	for _, want := range []string{
		`= "MZ"`,           // detection
		`"zimg"`,           // zboot marker check
		"EFI zboot",        // human-readable signal
		`b'ARM\x64'`,       // post-extract magic check (ensures we caught a broken extract)
		"gzip.decompress",  // payload decompression
	} {
		if !strings.Contains(script, want) {
			t.Errorf("build.sh missing %q — zboot strip path may be broken; Apple Silicon boot will fail with VZErrorDomain Code=1", want)
		}
	}

	// The strip must happen BEFORE the "Kernel: <size>" reporting line so
	// the reported size reflects the extracted Image, not the wrapper.
	stripIdx := strings.Index(script, "Detected EFI zboot wrapper")
	reportIdx := strings.LastIndex(script, `echo "Kernel:`)
	if stripIdx == -1 || reportIdx == -1 {
		t.Fatal("could not locate zboot-strip block or kernel-size report")
	}
	if stripIdx > reportIdx {
		t.Errorf("zboot strip happens AFTER the size report — reported sizes will be wrong")
	}
}

// The module allowlist in build.sh decides which .ko files survive the prune
// step. Any modprobe that init/init-stage2 runs MUST be in the allowlist or
// it will silently fail at boot (modprobe lines are `|| true` because some
// modules are profile-conditional). This catches "added modprobe but forgot
// to update allowlist" drift.
func TestInitramfsBuildScript_ModprobeMatchesAllowlist(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	// Extract module names from `modprobe <name>` lines.
	modprobeRE := regexp.MustCompile(`(?m)^\s*modprobe\s+(\S+)`)
	probed := map[string]struct{}{}
	for _, m := range modprobeRE.FindAllStringSubmatch(script, -1) {
		probed[m[1]] = struct{}{}
	}
	if len(probed) == 0 {
		t.Fatal("found zero modprobe lines — test scanning is broken")
	}

	// Extract the allowlist: KMODS_BASE + KMODS_STANDARD assignments.
	// Each value is on a continuation-line block; we join them and split
	// on whitespace.
	allowed := map[string]struct{}{}
	for _, varName := range []string{"KMODS_BASE", "KMODS_STANDARD"} {
		re := regexp.MustCompile(varName + `="((?:[^"]|\n)*)"`)
		m := re.FindStringSubmatch(script)
		if m == nil {
			t.Fatalf("could not find %s allowlist in build.sh", varName)
		}
		for _, tok := range strings.Fields(strings.ReplaceAll(m[1], "\\", "")) {
			allowed[tok] = struct{}{}
		}
	}

	for name := range probed {
		if _, ok := allowed[name]; !ok {
			t.Errorf("modprobe %s appears in init scripts but is missing from KMODS_BASE/KMODS_STANDARD allowlist — boot will silently skip it after prune", name)
		}
	}
}
