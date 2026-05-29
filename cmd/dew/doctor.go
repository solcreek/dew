//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func cmdDoctor(_ []string) error {
	fmt.Println("dew doctor — environment check")
	fmt.Println()

	pass := 0
	fail := 0
	warn := 0

	check := func(name string, ok bool, hint string) {
		if ok {
			fmt.Printf("  ✓ %s\n", name)
			pass++
		} else {
			fmt.Printf("  ✗ %s\n", name)
			if hint != "" {
				fmt.Printf("    %s\n", hint)
			}
			fail++
		}
	}
	warning := func(name, hint string) {
		fmt.Printf("  ⚠ %s\n", name)
		if hint != "" {
			fmt.Printf("    %s\n", hint)
		}
		warn++
	}

	// macOS version
	out, _ := exec.Command("sw_vers", "-productVersion").Output()
	macVer := strings.TrimSpace(string(out))
	check("macOS "+macVer, macVer != "" && macVer >= "13", "Apple VZ requires macOS 13+")

	// Architecture
	fmt.Printf("  ℹ Architecture: %s\n", runtime.GOARCH)

	// Self binary path
	self, _ := os.Executable()
	fmt.Printf("  ℹ Binary: %s\n", self)

	// Codesign + virtualization entitlement
	out, csErr := exec.Command("codesign", "-d", "--entitlements", ":-", self).CombinedOutput()
	hasEntitlement := strings.Contains(string(out), "com.apple.security.virtualization")
	if csErr != nil {
		warning("Codesign check", "Could not read entitlements: "+csErr.Error())
	} else {
		check("Virtualization entitlement", hasEntitlement, "Run: codesign --entitlements <plist> --force -s - "+self)
	}

	// Kernel + initramfs assets
	home, _ := os.UserHomeDir()
	assets := []string{
		home + "/.local/share/dew/vmlinuz",
		home + "/.local/share/dew/initramfs.cpio.gz",
	}
	for _, p := range assets {
		_, err := os.Stat(p)
		check("Asset: "+p, err == nil, "Run: dew assets pull")
	}

	// Running VM
	sock := home + "/.local/state/dew/default.sock"
	if _, err := os.Stat(sock); err == nil {
		fmt.Printf("  ℹ VM running (socket: %s)\n", sock)
	}

	// Memory check
	out, _ = exec.Command("sysctl", "-n", "hw.memsize").Output()
	fmt.Printf("  ℹ Memory: %s bytes\n", strings.TrimSpace(string(out)))

	fmt.Println()
	if fail > 0 {
		fmt.Printf("  %d passed, %d failed, %d warnings\n", pass, fail, warn)
		return fmt.Errorf("doctor: %d issues found", fail)
	}
	fmt.Printf("  ✓ All checks passed (%d ok, %d warnings)\n", pass, warn)
	return nil
}
