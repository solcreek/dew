//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/solcreek/dew/internal/detect"
	"github.com/solcreek/dew/internal/dewfile"
	"github.com/solcreek/dew/internal/vm/darwin"
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
	jsonMode := flagJSON
	verbose := false
	profile := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonMode = true
		case "--verbose", "-v":
			verbose = true
		case "--profile":
			// Require a real value: `dew doctor --profile --verbose` or a
			// trailing `--profile` must fail loudly, not silently fall
			// back to dew.toml/auto-detection — that would leave the user
			// believing doctor checked the profile they named when it
			// didn't.
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return fmt.Errorf("doctor: --profile requires a value (minimal|node|python|standard)")
			}
			profile = args[i+1]
			i++
		}
	}

	// Validate against the built profiles — mirrors the main CLI's
	// --profile check so `dew doctor --profile typo` fails with a clear
	// error instead of later reporting a confusing missing-asset for a
	// nonexistent initramfs-typo variant.
	if profile != "" {
		switch profile {
		case "minimal", "node", "python", "standard":
		default:
			return fmt.Errorf("doctor: unknown profile %q; valid: minimal, node, python, standard", profile)
		}
	}

	report := runDoctorChecks(verbose, profile)

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

// doctorProfiles returns the initramfs profiles doctor should verify:
// always "minimal" (the in-process boot test boots it), plus the
// profile THIS project actually runs — from an explicit --profile
// override, else the project's dew.toml, else auto-detection of the
// cwd. Without this, doctor only ever checked the minimal initramfs
// and reported all-green while the node initramfs a Node/Rails project
// actually boots (`dew up`, services-only) was missing or unverifiable
// — the exact false-green that hid the 2026-06 services-only brick.
func doctorProfiles(override string) []string {
	profiles := []string{"minimal"}
	add := func(p string) {
		if p == "" {
			return
		}
		for _, e := range profiles {
			if e == p {
				return
			}
		}
		profiles = append(profiles, p)
	}
	if override != "" {
		add(override)
		return profiles
	}
	cwd, _ := os.Getwd()
	if f, err := dewfile.Load(cwd); err == nil && f != nil && f.Project.Profile != "" {
		add(f.Project.Profile)
		return profiles
	}
	if p, err := detect.Detect(cwd); err == nil && p != nil {
		add(p.Profile)
	}
	return profiles
}

func runDoctorChecks(verbose bool, profileOverride string) DoctorReport {
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

	// Signature integrity. Entitlement presence is necessary but not
	// sufficient — npm/tar extraction can leave the entitlement
	// readable while invalidating the signed CodeDirectory hashes. VZ
	// then refuses to boot with the same opaque Code=1 the user sees
	// when the entitlement is genuinely missing. `codesign --verify
	// --strict` catches both classes of corruption.
	if vOut, vErr := exec.Command("codesign", "--verify", "--strict", self).CombinedOutput(); vErr != nil {
		checks = append(checks, DoctorCheck{
			Name:    "Codesign signature integrity",
			Status:  CheckFail,
			Code:    "codesign_invalid",
			Message: strings.TrimSpace(string(vOut)),
			Hint:    "Re-install dew (npm install -g dew) — signature was likely damaged during extraction",
		})
	} else {
		checks = append(checks, DoctorCheck{
			Name: "Codesign signature integrity", Status: CheckPass,
		})
	}

	// Kernel + initramfs assets. Path is content-addressed when the
	// binary was built by the release pipeline (ExpectedAssetSHA
	// populated); falls back to the legacy un-suffixed name for
	// dev/local builds. Doctor verifies the kernel plus EVERY profile
	// this project actually uses (minimal + the detected/declared
	// profile), so a missing or unverifiable node initramfs no longer
	// hides behind a minimal-only all-green.
	dataDir := dewDataDir()
	kernelPath := assetCachePath(dataDir, kernelAssetName())

	statAsset := func(name, path, hint string) {
		if _, err := os.Stat(path); err == nil {
			checks = append(checks, DoctorCheck{Name: name, Status: CheckPass})
		} else {
			checks = append(checks, DoctorCheck{
				Name:    name,
				Status:  CheckFail,
				Code:    "missing_asset",
				Message: "Asset not downloaded",
				Hint:    hint,
			})
		}
	}

	statAsset("Asset: "+kernelPath, kernelPath, "Run: dew assets pull")
	for _, profile := range doctorProfiles(profileOverride) {
		initrdPath := assetCachePath(dataDir, initrdAssetName(profile))
		statAsset(
			fmt.Sprintf("Asset (%s): %s", profile, initrdPath),
			initrdPath,
			"Run: dew assets pull "+profile,
		)
	}

	// Kernel format sanity check. The 2026-06-04 M4 Max report was a
	// stale 9MB EFI-stub-only kernel left over from an older install
	// — Apple VZ rejected it with the opaque Code=1, the user spent
	// days debugging. Detect the broken case at doctor time by
	// reading the kernel's ARM64 boot header magic at offset 0x38.
	// Skipped if the kernel file is missing (already covered above).
	if runtime.GOARCH == "arm64" {
		if _, err := os.Stat(kernelPath); err == nil {
			h := darwin.ReadBinaryHeader(kernelPath)
			hint := darwin.KernelFormatHint(h)
			if strings.Contains(hint, "raw ARM64 Linux Image") {
				checks = append(checks, DoctorCheck{
					Name: "Kernel format", Status: CheckPass, Value: "raw ARM64 Linux Image",
				})
			} else {
				checks = append(checks, DoctorCheck{
					Name:    "Kernel format",
					Status:  CheckFail,
					Code:    "kernel_stale",
					Message: hint,
					Hint:    "Run: dew assets pull --force",
				})
			}
		}
	}

	// Running VM (informational)
	home, _ := os.UserHomeDir()
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
		// --verbose: surface the full VM config dump (kernel format,
		// memory, devices, host model) and any error chain by enabling
		// the debug-print path in the spawned binary.
		if verbose {
			bootCmd.Env = append(os.Environ(), "DEW_DEBUG=1")
		}
		bootOut, bootErr := bootCmd.CombinedOutput()
		bootOk := bootErr == nil && strings.Contains(string(bootOut), "doctor-ok")
		if bootOk {
			c := DoctorCheck{Name: "VM boot test", Status: CheckPass}
			if verbose {
				c.Message = strings.TrimSpace(string(bootOut))
			}
			checks = append(checks, c)
		} else {
			checks = append(checks, DoctorCheck{
				Name:    "VM boot test",
				Status:  CheckFail,
				Code:    "boot_failed",
				Message: strings.TrimSpace(string(bootOut)),
				Hint:    "Re-run with `dew doctor --verbose` for the full VM config dump, then attach the output to a bug report",
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
			// --verbose may attach a captured boot dump to a passing
			// check; surface it indented so the user sees it without
			// having to re-run with DEW_DEBUG=1 themselves.
			if c.Message != "" {
				printIndented(c.Message)
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
