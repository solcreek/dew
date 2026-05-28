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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	distroName = "dew"
	version    = "0.1.0-dev"
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
  dew start [flags]      Start a persistent VM
  dew exec <cmd>         Execute in running environment
  dew down               Stop the environment
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
func distroExists() bool {
	out, err := exec.Command("wsl", "-l", "-q").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		// WSL outputs UTF-16, trim null bytes
		name = strings.ReplaceAll(name, "\x00", "")
		if strings.EqualFold(name, distroName) {
			return true
		}
	}
	return false
}

// dewDataDir returns the Windows data directory for Dew.
func dewDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dew")
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
		// TODO: download from GitHub Releases
		return fmt.Errorf("rootfs not found at %s — download from GitHub Releases", rootfsPath)
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
