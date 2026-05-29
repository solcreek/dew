//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// CheckStatus is one of "pass", "warn", "fail".
type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

type DoctorCheck struct {
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Value   string      `json:"value,omitempty"`
	Message string      `json:"message,omitempty"`
	Code    string      `json:"code,omitempty"`
	Hint    string      `json:"hint,omitempty"`
}

type DoctorSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type DoctorReport struct {
	OK      bool          `json:"ok"`
	Checks  []DoctorCheck `json:"checks"`
	Summary DoctorSummary `json:"summary"`
}

func cmdDoctor(args []string) error {
	// Parse --json from args or global flag.
	jsonMode := flagJSON
	for _, a := range args {
		if a == "--json" {
			jsonMode = true
		}
	}

	report := runDoctorChecks()

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printDoctorReportHuman(report)
	}

	if !report.OK {
		os.Exit(1)
	}
	return nil
}

func runDoctorChecks() DoctorReport {
	var checks []DoctorCheck

	// macOS version
	out, _ := exec.Command("sw_vers", "-productVersion").Output()
	macVer := strings.TrimSpace(string(out))
	if macVer != "" && macVer >= "13" {
		checks = append(checks, DoctorCheck{
			Name: "macOS version", Status: CheckPass, Value: macVer,
		})
	} else {
		checks = append(checks, DoctorCheck{
			Name:    "macOS version",
			Status:  CheckFail,
			Value:   macVer,
			Code:    "macos_too_old",
			Message: "Apple VZ requires macOS 13+",
		})
	}

	// Architecture (informational; included as pass with value)
	checks = append(checks, DoctorCheck{
		Name: "Architecture", Status: CheckPass, Value: runtime.GOARCH,
	})

	// Self binary path
	self, _ := os.Executable()
	checks = append(checks, DoctorCheck{
		Name: "Binary", Status: CheckPass, Value: self,
	})

	// Codesign + virtualization entitlement
	ents, csErr := exec.Command("codesign", "-d", "--entitlements", ":-", self).CombinedOutput()
	hasEntitlement := strings.Contains(string(ents), "com.apple.security.virtualization")
	if csErr != nil {
		checks = append(checks, DoctorCheck{
			Name:    "Codesign check",
			Status:  CheckWarn,
			Code:    "codesign_unreadable",
			Message: "Could not read entitlements: " + csErr.Error(),
		})
	} else if hasEntitlement {
		checks = append(checks, DoctorCheck{
			Name: "Virtualization entitlement present", Status: CheckPass,
		})
	} else {
		checks = append(checks, DoctorCheck{
			Name:    "Virtualization entitlement present",
			Status:  CheckFail,
			Code:    "no_entitlement",
			Message: "Binary is missing com.apple.security.virtualization",
			Hint:    "Run: codesign --entitlements <plist> --force -s - " + self,
		})
	}

	// Ad-hoc + restricted entitlement
	cdInfo, _ := exec.Command("codesign", "-dv", self).CombinedOutput()
	isAdhoc := strings.Contains(string(cdInfo), "adhoc") ||
		strings.Contains(string(cdInfo), "Signature=adhoc")
	if hasEntitlement && isAdhoc {
		checks = append(checks, DoctorCheck{
			Name:    "Ad-hoc signature with restricted entitlement",
			Status:  CheckWarn,
			Code:    "ad_hoc_entitlement",
			Message: "VZ entitlement requires Developer ID, not ad-hoc. VM start will fail with VZErrorDomain Code=1.",
		})
	}

	// Kernel + initramfs assets
	home, _ := os.UserHomeDir()
	assets := []string{
		home + "/.local/share/dew/vmlinuz",
		home + "/.local/share/dew/initramfs.cpio.gz",
	}
	for _, p := range assets {
		name := "Asset: " + p
		if _, err := os.Stat(p); err == nil {
			checks = append(checks, DoctorCheck{Name: name, Status: CheckPass})
		} else {
			checks = append(checks, DoctorCheck{
				Name:    name,
				Status:  CheckFail,
				Code:    "missing_asset",
				Message: "Asset not downloaded",
				Hint:    "Run: dew assets pull",
			})
		}
	}

	// Running VM (informational)
	sock := home + "/.local/state/dew/default.sock"
	if _, err := os.Stat(sock); err == nil {
		checks = append(checks, DoctorCheck{
			Name: "VM running", Status: CheckPass, Value: sock,
		})
	}

	// Memory (informational)
	memOut, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
	checks = append(checks, DoctorCheck{
		Name: "Memory", Status: CheckPass, Value: strings.TrimSpace(string(memOut)) + " bytes",
	})

	// Decide whether to run real boot test:
	// Skip if there's already a hard failure or if ad-hoc warning is present
	// (Boot test would just fail again with the same root cause).
	priorFail := false
	priorAdhoc := false
	for _, c := range checks {
		if c.Status == CheckFail {
			priorFail = true
		}
		if c.Code == "ad_hoc_entitlement" {
			priorAdhoc = true
		}
	}

	if !priorFail && hasEntitlement && !priorAdhoc {
		bootCmd := exec.Command(self, "run", "--profile", "minimal", "--", "echo", "doctor-ok")
		bootOut, bootErr := bootCmd.CombinedOutput()
		bootOk := bootErr == nil && strings.Contains(string(bootOut), "doctor-ok")
		if bootOk {
			checks = append(checks, DoctorCheck{Name: "VM boot test", Status: CheckPass})
		} else {
			checks = append(checks, DoctorCheck{
				Name:    "VM boot test",
				Status:  CheckFail,
				Code:    "boot_failed",
				Message: strings.TrimSpace(string(bootOut)),
			})
		}
	}

	summary := DoctorSummary{}
	for _, c := range checks {
		switch c.Status {
		case CheckPass:
			summary.Pass++
		case CheckWarn:
			summary.Warn++
		case CheckFail:
			summary.Fail++
		}
	}

	return DoctorReport{
		OK:      summary.Fail == 0,
		Checks:  checks,
		Summary: summary,
	}
}

func printDoctorReportHuman(r DoctorReport) {
	fmt.Println("dew doctor — environment check")
	fmt.Println()

	for _, c := range r.Checks {
		switch c.Status {
		case CheckPass:
			if c.Value != "" {
				fmt.Printf("  ✓ %s: %s\n", c.Name, c.Value)
			} else {
				fmt.Printf("  ✓ %s\n", c.Name)
			}
		case CheckWarn:
			fmt.Printf("  ⚠ %s\n", c.Name)
			if c.Message != "" {
				fmt.Printf("    %s\n", c.Message)
			}
		case CheckFail:
			fmt.Printf("  ✗ %s\n", c.Name)
			if c.Message != "" {
				printIndented(c.Message)
			}
			if c.Hint != "" {
				fmt.Printf("    %s\n", c.Hint)
			}
		}
	}

	fmt.Println()
	if r.OK {
		fmt.Printf("  ✓ All checks passed (%d ok, %d warnings)\n",
			r.Summary.Pass, r.Summary.Warn)
	} else {
		fmt.Printf("  %d passed, %d failed, %d warnings\n",
			r.Summary.Pass, r.Summary.Fail, r.Summary.Warn)
	}
}

func printIndented(s string) {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fmt.Printf("    %s\n", line)
	}
}
