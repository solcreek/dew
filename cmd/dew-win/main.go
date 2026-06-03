//go:build windows

// dew-win is the Windows wrapper for Dew. WSL2 is the VM platform
// on Windows — Microsoft maintains the kernel and lifecycle, we
// just import a Linux rootfs as a custom distro ("dew") and run
// commands inside it via wsl.exe.
//
// Because WSL2 manages the VM, the wrapper translates dew commands
// directly into WSL operations rather than forwarding them to a
// dedicated in-distro CLI:
//
//   dew setup       → download rootfs + wsl --import dew
//   dew vm start    → ensure distro is running (wsl auto-starts on use)
//   dew vm stop     → wsl --terminate dew
//   dew vm status   → check if distro is registered
//   dew down        → alias for vm stop
//   dew exec <cmd>  → wsl -d dew -- <cmd>
//
// This is fundamentally different from the macOS path (where we
// drive Apple Virtualization + our own kernel + initramfs); on
// Windows the rootfs is the only thing we control, and the wrapper
// keeps the user-facing command surface aligned so the muscle
// memory survives the platform switch.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Printf("dew %s (windows/wsl2)\n", version)
	case "help", "--help", "-h":
		printUsage()
	case "setup":
		err = cmdSetup()
	case "vm":
		err = cmdVM(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		// alias for `dew vm stop`
		err = cmdVM([]string{"stop"})
	default:
		fmt.Fprintf(os.Stderr, "dew: %q is not implemented in the Windows wrapper yet.\n", os.Args[1])
		fmt.Fprintf(os.Stderr, "Supported on Windows today: setup, vm start|stop|status, exec, down.\n")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dew: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dew — ultra-lightweight dev environment (Windows via WSL2)

Usage:
  dew setup              Install/update WSL2 distro
  dew up [dir]           Detect a Node project + run its dev server in WSL2
  dew vm start           Ensure the WSL2 distro is running
  dew vm stop            Terminate the WSL2 distro (alias: dew down)
  dew vm status          Show whether the WSL2 distro is running
  dew exec <cmd>         Run a command inside the WSL2 distro
  dew version            Print version

WSL2 is the VM platform on Windows — Microsoft manages the kernel
and lifecycle; the wrapper just imports a Linux rootfs as a custom
distro and runs commands inside it.

dew up currently supports Node-style projects (package.json with
a dev or start script). The dev server's port (e.g. Vite :5173)
is reachable on the Windows host via WSL2 localhost forwarding.
`)
}

// wslInstalled checks if WSL2 is available.
func wslInstalled() bool {
	cmd := exec.Command("wsl", "--status")
	return cmd.Run() == nil
}

// distroExists checks if the dew distro is already imported.
//
// Probes the distro directly with `wsl -d dew -- true` instead of
// parsing `wsl -l -q` output. v0.7.25 tried to handle wsl.exe's
// UTF-16LE encoding but field reports from Windows-on-ARM showed
// the parse still missed the distro under some WSL versions /
// locales (cmd succeeded interactively but Go's exec.Command
// captured bytes in a different encoding mode and the BOM-based
// detector misclassified them). Probing sidesteps every encoding
// edge case: wsl.exe exits 0 iff the named distro is registered
// and can start, non-zero otherwise.
//
// Side effect: starts the distro if it was stopped. That's fine —
// every caller of ensureDistro is about to run a command inside
// it anyway, so we just pay the start cost a few hundred ms
// earlier. Zero cost on an already-running distro. Stdout/stderr
// silenced so the welcome banner some wsl versions emit doesn't
// leak into grove's own output.
func distroExists() bool {
	cmd := exec.Command("wsl", "-d", distroName, "--", "true")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
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

// cmdVM dispatches `dew vm <subcommand>` to the WSL2 lifecycle.
//
// WSL2 manages distro lifecycle on demand: any command against a
// distro auto-starts it, and the WSL daemon terminates idle distros
// after a few seconds of inactivity (or on `wsl --terminate`).
// So there's no separate "boot the VM" step we have to drive — the
// distro presence check (which probes by running `true` inside it)
// is the start.
func cmdVM(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew vm start|stop|status")
	}
	switch args[0] {
	case "start":
		if err := ensureDistro(); err != nil {
			return err
		}
		fmt.Println("dew: WSL2 distro running")
		return nil
	case "stop":
		if !wslInstalled() {
			return fmt.Errorf("WSL2 not installed")
		}
		// Idempotent: terminating a non-running distro exits 0.
		c := exec.Command("wsl", "--terminate", distroName)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("wsl --terminate %s: %w", distroName, err)
		}
		fmt.Println("dew: stopped")
		return nil
	case "status":
		if !wslInstalled() {
			fmt.Println("dew: WSL2 not installed")
			return nil
		}
		if distroExists() {
			fmt.Println("dew: running")
		} else {
			fmt.Println("dew: not installed (run: dew setup)")
		}
		return nil
	default:
		return fmt.Errorf("unknown vm subcommand %q (start|stop|status)", args[0])
	}
}

// cmdExec runs a command inside the WSL2 distro, passing the
// caller's argv through unchanged. The "--" separator in `wsl -d
// <name> -- <cmd...>" tells wsl.exe everything after is the
// command line for the distro, no further flag parsing on its side.
// Stdin/stdout/stderr connect straight through so interactive
// commands work and exit code propagates.
func cmdExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew exec <cmd> [args...]")
	}
	if err := ensureDistro(); err != nil {
		return err
	}
	wslArgs := append([]string{"-d", distroName, "--"}, args...)
	cmd := exec.Command("wsl", wslArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdUp detects a Node-style project in dir (or cwd) and runs its
// dev server inside the WSL2 distro. The project is reached via
// the auto-mounted /mnt/<drive>/... path; WSL2's mirrored
// networking (Windows 11 + WSL 2.0+) makes the dev server's port
// reachable on the Windows host as plain localhost:<port>.
//
// Today's scope is intentionally narrow vs macOS dew up — Node
// projects only, no framework-specific port detection, no
// streaming health probe, no auto-rebuild handling. The dev
// server's own output goes straight to the user's terminal so
// they see the actual URL banner Vite/Next/Astro/etc. prints.
// Heavier project-aware behavior can land iteratively as Windows
// users hit specific gaps.
func cmdUp(args []string) error {
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		dir = args[0]
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", dir, err)
	}

	// Node-only fast check. If we ever support Python/Go/etc.
	// projects on Windows, branch here per project type.
	pkgPath := filepath.Join(absDir, "package.json")
	if _, err := os.Stat(pkgPath); err != nil {
		return fmt.Errorf("no package.json in %s — dew up on Windows currently supports Node-style projects only", absDir)
	}

	if err := ensureDistro(); err != nil {
		return err
	}

	// Translate the Windows path to the WSL2 mount path
	// (e.g. C:\Users\foo\proj → /mnt/c/Users/foo/proj). Defer to
	// wslpath inside the distro so we don't have to mirror its
	// drive-letter / case / UNC rules.
	wslPath, err := winPathToWSL(absDir)
	if err != nil {
		return err
	}
	fmt.Printf("dew: project at %s\n", absDir)
	fmt.Printf("dew: mounted in WSL2 at %s\n", wslPath)

	// Install dependencies if node_modules is missing. Skip when
	// it exists — preserves the user's last install (the install
	// itself is idempotent but slow on cold cache).
	nmTest := exec.Command("wsl", "-d", distroName, "--",
		"test", "-d", wslPath+"/node_modules")
	if nmTest.Run() != nil {
		fmt.Println("dew: installing dependencies (npm install)...")
		install := exec.Command("wsl", "-d", distroName, "--",
			"sh", "-c", fmt.Sprintf("cd %s && npm install", shellQuote(wslPath)))
		install.Stdin = os.Stdin
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr
		if err := install.Run(); err != nil {
			return fmt.Errorf("npm install: %w", err)
		}
	}

	// Pick the script. "dev" wins because every modern frontend
	// scaffolding (Vite, Next, Astro, SvelteKit, Nuxt) uses it;
	// "start" is the fallback for older / minimal templates.
	script := detectDevScript(pkgPath)
	if script == "" {
		return fmt.Errorf("package.json has no 'dev' or 'start' script — add one or run npm directly via dew exec")
	}
	fmt.Printf("dew: starting (npm run %s)\n", script)
	fmt.Println("dew: dev server output follows. Ctrl+C to stop.")
	fmt.Println()

	dev := exec.Command("wsl", "-d", distroName, "--",
		"sh", "-c", fmt.Sprintf("cd %s && npm run %s", shellQuote(wslPath), script))
	dev.Stdin = os.Stdin
	dev.Stdout = os.Stdout
	dev.Stderr = os.Stderr
	return dev.Run()
}

// winPathToWSL converts a Windows absolute path into its WSL2
// mount path via the distro's `wslpath` utility. Trims the
// trailing newline wslpath emits.
//
// wsl.exe with arguments after `--` joins them into a single
// command line and dispatches via /bin/sh -c inside the distro
// — even though Go's exec.Command passes them as separate argv
// to wsl.exe itself, there's a hidden shell layer on the Linux
// side. That shell strips unquoted backslashes, so "C:\Foo\Bar"
// arrives at wslpath as "C:FooBar" and wslpath errors out.
//
// Normalizing to forward slashes (filepath.ToSlash) sidesteps it:
// Windows APIs accept both \ and /, Linux paths use /, and the
// shell passes / through untouched. wslpath handles both spellings
// of the input.
func winPathToWSL(winPath string) (string, error) {
	normalized := filepath.ToSlash(winPath)
	out, err := exec.Command("wsl", "-d", distroName, "--",
		"wslpath", "-a", normalized).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath %s: %w", winPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// detectDevScript reads package.json and returns "dev" if scripts
// defines one, else "start", else "". Tolerant of malformed JSON
// — returns "" and lets the caller surface the actionable error.
func detectDevScript(pkgPath string) string {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	if _, ok := pkg.Scripts["dev"]; ok {
		return "dev"
	}
	if _, ok := pkg.Scripts["start"]; ok {
		return "start"
	}
	return ""
}

// shellQuote produces a POSIX-sh-safe single-quoted literal — used
// when interpolating WSL paths into an inline `sh -c` string. The
// WSL path is alphanumeric + / + . in practice but a user's project
// dir could theoretically contain a single-quote in a parent dir
// name (rare), so escape per POSIX: '...'\''...' .
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
