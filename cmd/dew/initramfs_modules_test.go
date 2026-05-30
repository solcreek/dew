//go:build darwin

package main

import (
	"os"
	"path/filepath"
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
