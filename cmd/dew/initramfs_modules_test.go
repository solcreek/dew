//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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

// /usr/local/bin on the persistent disk (crun, dew-oci-run, dew-agent,
// dew-httpd) must be refreshed from the initramfs on EVERY boot, not just first
// boot. Otherwise an initramfs upgrade that adds or replaces one of those
// binaries never reaches an already-initialized disk, and `dew run --image` /
// `dew up --with` fail with a bare "exit -1". Mirrors the kernel-module guard.
func TestInitramfsBuildScript_RefreshesLocalBinEveryBoot(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	marker := "DEW_REFRESH_LOCALBIN"
	markerIdx := strings.Index(script, marker)
	if markerIdx == -1 {
		t.Fatalf("init script missing %q marker for the always-on /usr/local/bin refresh block", marker)
	}
	firstBootIdx := strings.Index(script, "! -f /mnt/root/.dew-initialized")
	endFirstBoot := strings.Index(script[firstBootIdx:], "    fi\n")
	if firstBootIdx == -1 || endFirstBoot == -1 {
		t.Fatal("could not locate first-boot block boundaries — test needs updating")
	}
	firstBootEnd := firstBootIdx + endFirstBoot
	if markerIdx > firstBootIdx && markerIdx < firstBootEnd {
		t.Errorf("/usr/local/bin refresh is inside the first-boot block — must run on every boot or initramfs binary upgrades never reach an existing disk")
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

	// Extract the allowlist: KMODS_BASE (the single module set all profiles
	// share now that containers run via crun --net=host, no CNI bridge).
	// The value spans continuation lines; join and split on whitespace.
	allowed := map[string]struct{}{}
	re := regexp.MustCompile(`KMODS_BASE="((?:[^"]|\n)*)"`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatal("could not find KMODS_BASE allowlist in build.sh")
	}
	for _, tok := range strings.Fields(strings.ReplaceAll(m[1], "\\", "")) {
		allowed[tok] = struct{}{}
	}

	for name := range probed {
		if _, ok := allowed[name]; !ok {
			t.Errorf("modprobe %s appears in init scripts but is missing from KMODS_BASE allowlist — boot will silently skip it after prune", name)
		}
	}
}
