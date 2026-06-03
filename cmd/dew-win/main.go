//go:build windows

// dew-win is the Windows wrapper for Dew. It delegates to a custom
// WSL2 distro that contains the Linux dew-agent + containerd.
//
// Flow:
//   1. Check if WSL2 is available
//   2. Import Dew distro if not already imported
//   3. Forward commands to: wsl -d dew -- dew-native <args>
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	distroName = "dew"
	version    = "0.1.0-dev"
	// rootfsURL templates against the latest dew release. The rootfs
	// format is stable across patch versions (it's the standard
	// initramfs unpacked); pinning to a specific release would just
	// stall the wrapper behind dew release tags it has no other
	// reason to know about.
	rootfsURLTemplate = "https://github.com/solcreek/dew/releases/latest/download/dew-rootfs-%s.tar.gz"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("dew %s (windows/wsl2)\n", version)
	case "help", "--help", "-h":
		printUsage()
	case "setup":
		if err := cmdSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
			os.Exit(1)
		}
	default:
		if err := ensureDistro(); err != nil {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
			os.Exit(1)
		}
		if err := forward(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dew — ultra-lightweight dev environment (Windows via WSL2)

Usage:
  dew up [dir]           Auto-detect project and start dev environment
  dew vm start [flags]   Start a persistent VM
  dew exec <cmd>         Execute in running environment
  dew vm stop            Stop the VM (alias: dew down)
  dew setup              Install/update WSL2 distro
  dew version            Print version

All commands run inside a WSL2 Linux environment.
`)
}

// wslInstalled checks if WSL2 is available.
func wslInstalled() bool {
	cmd := exec.Command("wsl", "--status")
	return cmd.Run() == nil
}

// distroExists checks if the dew distro is already imported.
//
// wsl.exe emits its --list output as UTF-16LE with a BOM, not the
// UTF-8 most Go code assumes. An earlier implementation tried to
// recover by stripping individual NUL bytes after `string(out)`
// conversion, which leaves the BOM bytes (0xFF 0xFE) attached to
// the first line — `strings.EqualFold("\xff\xfeAlpine", "dew")`
// is fine but if "dew" happens to be the first line the BOM
// prevents matching it. Decode UTF-16 explicitly instead.
func distroExists() bool {
	out, err := exec.Command("wsl", "-l", "-q").Output()
	if err != nil {
		return false
	}
	text := decodeWSLOutput(out)
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(line)
		if strings.EqualFold(name, distroName) {
			return true
		}
	}
	return false
}

// decodeWSLOutput converts the raw bytes wsl.exe emits into a Go
// string. Modern wsl.exe (Windows 11 era) emits UTF-16LE with a
// BOM; older versions and some WSL builds emit plain ASCII/UTF-8.
// Detect by BOM and decode accordingly so the distro-name match
// works across both.
func decodeWSLOutput(raw []byte) string {
	// UTF-16LE BOM (\xff\xfe) at start ⇒ decode as UTF-16LE.
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		body := raw[2:]
		u16 := make([]uint16, 0, len(body)/2)
		for i := 0; i+1 < len(body); i += 2 {
			u16 = append(u16, binary.LittleEndian.Uint16(body[i:i+2]))
		}
		return string(utf16.Decode(u16))
	}
	// UTF-16BE BOM (\xfe\xff) — vanishingly rare on Windows but
	// trivial to handle here.
	if len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff {
		body := raw[2:]
		u16 := make([]uint16, 0, len(body)/2)
		for i := 0; i+1 < len(body); i += 2 {
			u16 = append(u16, binary.BigEndian.Uint16(body[i:i+2]))
		}
		return string(utf16.Decode(u16))
	}
	// No BOM — treat as UTF-8 / ASCII (older WSL builds, or non-
	// wsl callers in tests).
	return string(raw)
}

// dewDataDir returns the Windows data directory for Dew.
func dewDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dew")
}

// rootfsAssetArch maps Go's runtime.GOARCH (amd64, arm64) to the
// arch tag used in published release asset names (x86_64, aarch64).
// We use the Linux convention because the rootfs is a Linux distro.
func rootfsAssetArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return ""
	}
}

// downloadRootfs fetches the per-arch rootfs from the latest dew
// release and writes it to dest. Uses GitHub's /latest/download/
// redirect so the wrapper doesn't have to track release tags.
//
// Large file (~120 MB); a 5-minute timeout covers slow connections
// while still failing visibly on a permanent hang.
func downloadRootfs(dest string) error {
	arch := rootfsAssetArch(runtime.GOARCH)
	if arch == "" {
		return fmt.Errorf("unsupported GOARCH %q (need amd64 or arm64)", runtime.GOARCH)
	}
	url := fmt.Sprintf(rootfsURLTemplate, arch)
	fmt.Printf("  fetching %s\n", url)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	// Stream to a .part file then atomic-rename so a partial download
	// doesn't leave a corrupt rootfs that distroExists falsely trusts.
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dest, err)
	}
	return nil
}

// cmdSetup installs WSL2 and imports the Dew distro.
func cmdSetup() error {
	if !wslInstalled() {
		fmt.Println("dew: WSL2 not found. Installing...")
		fmt.Println()
		fmt.Println("  This requires administrator privileges and a restart.")
		fmt.Println("  Run this command in an elevated PowerShell:")
		fmt.Println()
		fmt.Println("    wsl --install --no-distribution")
		fmt.Println()
		fmt.Println("  After restart, run 'dew setup' again.")
		return fmt.Errorf("WSL2 not installed")
	}

	if distroExists() {
		fmt.Println("dew: distro already installed")
		return nil
	}

	dataDir := dewDataDir()
	os.MkdirAll(dataDir, 0755)

	rootfsPath := filepath.Join(dataDir, "dew-rootfs.tar.gz")
	if _, err := os.Stat(rootfsPath); err != nil {
		fmt.Println("dew: downloading rootfs...")
		if err := downloadRootfs(rootfsPath); err != nil {
			return fmt.Errorf("download rootfs: %w", err)
		}
	}

	distroDir := filepath.Join(dataDir, "distro")
	os.MkdirAll(distroDir, 0755)

	fmt.Println("dew: importing WSL2 distro...")
	cmd := exec.Command("wsl", "--import", distroName, distroDir, rootfsPath, "--version", "2")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wsl import failed: %w", err)
	}

	fmt.Println("dew: setup complete")
	return nil
}

// ensureDistro checks that WSL2 and the distro are ready.
func ensureDistro() error {
	if !wslInstalled() {
		return fmt.Errorf("WSL2 not installed. Run: dew setup")
	}
	if !distroExists() {
		return fmt.Errorf("Dew distro not found. Run: dew setup")
	}
	return nil
}

// forward passes the command to the WSL2 distro.
func forward(args []string) error {
	wslArgs := []string{"-d", distroName, "--"}
	wslArgs = append(wslArgs, "dew-native")
	wslArgs = append(wslArgs, args...)

	cmd := exec.Command("wsl", wslArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
