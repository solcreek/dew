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
//	dew setup       → download rootfs + wsl --import dew
//	dew vm start    → ensure distro is running (wsl auto-starts on use)
//	dew vm stop     → wsl --terminate dew
//	dew vm status   → report running / stopped / not installed (passive)
//	dew down        → alias for vm stop
//	dew exec <cmd>  → wsl -d dew --exec <cmd> (no implicit shell)
//
// This is fundamentally different from the macOS path (where we
// drive Apple Virtualization + our own kernel + initramfs); on
// Windows the rootfs is the only thing we control, and the wrapper
// keeps the user-facing command surface aligned so the muscle
// memory survives the platform switch.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	case "run":
		err = cmdRun(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		// alias for `dew vm stop`
		err = cmdVM([]string{"stop"})
	case "doctor":
		err = cmdDoctor()
	case "env":
		err = cmdEnv()
	default:
		fmt.Fprintf(os.Stderr, "dew: %q is not implemented in the Windows wrapper yet.\n", os.Args[1])
		fmt.Fprintf(os.Stderr, "Supported on Windows today: setup, up, run, exec, vm start|stop|status|list, down, doctor, env.\n")
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
  dew run [--] <cmd>     Run a one-shot command in the WSL2 distro
  dew exec <cmd>         Run a command inside the WSL2 distro
  dew vm start           Ensure the WSL2 distro is running
  dew vm stop            Terminate the WSL2 distro (alias: dew down)
  dew vm status          Show whether the WSL2 distro is running
  dew vm list            List registered WSL2 distros and their state
  dew doctor             Diagnose the WSL2 environment
  dew env                Print environment info (paths, distro state)
  dew version            Print version

WSL2 is the VM platform on Windows — Microsoft manages the kernel
and lifecycle; the wrapper just imports a Linux rootfs as a custom
distro and runs commands inside it.

dew up currently supports Node-style projects (package.json with
a dev or start script). The dev server's port (e.g. Vite :5173)
is reachable on the Windows host via WSL2 localhost forwarding.
`)
}

// wslQuery runs wsl.exe with the given args and returns its captured
// stdout. It is a package var so tests can stub wsl.exe without a real
// WSL2 install. WSL_UTF8=1 is set so `wsl --list` yields UTF-8 rather
// than UTF-16. Only query/control commands (status, list, terminate,
// in-distro probes) go through here; the streaming commands (exec, up,
// setup import) wire stdio directly and are covered by the smoke test.
var wslQuery = func(args ...string) ([]byte, error) {
	cmd := exec.Command("wsl", args...)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	return cmd.Output()
}

// wslInstalled checks if WSL2 is available.
func wslInstalled() bool {
	_, err := wslQuery("--status")
	return err == nil
}

// withWSLStderr enriches a wslQuery error with wsl.exe's captured stderr
// (cmd.Output stashes it in ExitError.Stderr), so a failed control
// command surfaces the actual wsl message rather than a bare exit code.
func withWSLStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
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
// earlier. Zero cost on an already-running distro. wslQuery uses
// cmd.Output(): the probe's stdout is captured (not forwarded) and its
// stderr is discarded, so the welcome banner some wsl versions emit
// doesn't leak into dew's own output.
func distroExists() bool {
	_, err := wslQuery("-d", distroName, "--", "true")
	return err == nil
}

// distroListNames returns the distro names printed by `wsl --list
// --quiet <extra>`. Unlike distroExists, listing is passive — it never
// starts a distro. WSL_UTF8=1 forces plain UTF-8 output (modern WSL);
// stripping stray NUL bytes recovers ASCII names even if a legacy
// build still emits UTF-16LE, sidestepping the encoding trap that made
// earlier `wsl -l` parsing unreliable.
//
// A non-nil error means wsl.exe itself failed (e.g. a transient WSL
// service issue). Callers that report status must surface it rather
// than treat a failure as "no distros" — otherwise `dew vm status`
// would misreport a live-but-unlistable WSL as "not installed".
func distroListNames(extra ...string) (map[string]bool, error) {
	out, err := wslQuery(append([]string{"--list", "--quiet"}, extra...)...)
	if err != nil {
		return nil, withWSLStderr(err)
	}
	return parseDistroNames(out), nil
}

// parseDistroNames extracts the set of distro names from `wsl --list
// --quiet` output. Strips stray NUL bytes so a legacy UTF-16LE build
// still yields ASCII names, and skips blank lines.
func parseDistroNames(out []byte) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\x00", ""), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names[n] = true
		}
	}
	return names
}

// distroRegistered reports whether the dew distro is imported, without
// starting it — contrast distroExists, whose `true` probe starts the
// distro as a side effect. Propagates a wsl --list failure.
func distroRegistered() (bool, error) {
	names, err := distroListNames()
	return names[distroName], err
}

// distroRunningNow reports whether the dew distro is currently running,
// without starting it. `wsl --list --running` lists only running
// distros, so this is a name lookup — immune to STATE-column
// localization on non-English Windows. Propagates a wsl --list failure.
func distroRunningNow() (bool, error) {
	names, err := distroListNames("--running")
	return names[distroName], err
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
		return fmt.Errorf("usage: dew vm start|stop|status|list")
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
		// Idempotent: terminating a non-running distro exits 0. On real
		// failure, surface wsl.exe's own stderr so the message is
		// diagnosable instead of a bare "exit status N".
		if _, err := wslQuery("--terminate", distroName); err != nil {
			return fmt.Errorf("wsl --terminate %s: %w", distroName, withWSLStderr(err))
		}
		fmt.Println("dew: stopped")
		return nil
	case "status":
		if !wslInstalled() {
			fmt.Println("dew: WSL2 not installed")
			return nil
		}
		line, err := vmStatusLine()
		if err != nil {
			return fmt.Errorf("wsl --list: %w", err)
		}
		fmt.Println(line)
		return nil
	case "list":
		if !wslInstalled() {
			fmt.Println("dew: WSL2 not installed")
			return nil
		}
		return vmList()
	default:
		return fmt.Errorf("unknown vm subcommand %q (start|stop|status|list)", args[0])
	}
}

// runPassthrough runs an already-wired exec.Cmd and, when the command
// ran but exited non-zero, exits the dew process with that same code
// and stays silent — the command's own stderr already explained the
// failure, so a wrapping "dew: exit status N" line would just be noise
// that also flattens every failure to exit 1. Errors that mean the
// command never ran (wsl.exe missing, distro gone) are returned so the
// caller can surface them. Returns nil only on success.
func runPassthrough(cmd *exec.Cmd) error {
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return err
}

// execWSLArgs builds the wsl.exe argv that runs the given command
// directly inside the dew distro. The `--exec` (not bare `--`) is
// load-bearing: it bypasses the distro's implicit /bin/sh so the
// caller's argv is passed through without shell re-parsing. See
// cmdExec.
func execWSLArgs(args []string) []string {
	return append([]string{"-d", distroName, "--exec"}, args...)
}

// cmdExec runs a command inside the WSL2 distro, passing the caller's
// argv through unchanged (docker-exec semantics: no shell unless the
// caller asks for one via `dew exec sh -c '...'`).
//
// It uses `--exec` (`-e`), not the bare `--` separator. With `--`,
// wsl.exe hands the post-separator tokens to the distro's default
// shell (`/bin/sh -c "<tokens>"`), which re-parses them: an argument
// like `[%s]|` gets its `|` treated as a pipe and args with `$vars`
// or spaces are mangled. `--exec` runs the command directly, so argv
// arrives byte-for-byte. Stdin/stdout/stderr connect straight through
// so interactive commands work and the exit code propagates.
func cmdExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew exec <cmd> [args...]")
	}
	if err := ensureDistro(); err != nil {
		return err
	}
	cmd := exec.Command("wsl", execWSLArgs(args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runPassthrough(cmd)
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
	nmTest := exec.Command("wsl", "-d", distroName, "--exec",
		"sh", "-c", "test -d "+shellQuote(wslPath+"/node_modules"))
	if nmTest.Run() != nil {
		fmt.Println("dew: installing dependencies (npm install)...")
		install := exec.Command("wsl", "-d", distroName, "--exec",
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

	dev := exec.Command("wsl", "-d", distroName, "--exec",
		"sh", "-c", fmt.Sprintf("cd %s && npm run %s", shellQuote(wslPath), script))
	dev.Stdin = os.Stdin
	dev.Stdout = os.Stdout
	dev.Stderr = os.Stderr
	return runPassthrough(dev)
}

// winPathToWSL converts a Windows absolute path into its WSL2
// mount path via the distro's `wslpath` utility. Trims the
// trailing newline wslpath emits.
//
// Runs via `--exec` so wslpath receives the path as a single argv
// element with no intervening shell. Still normalize to forward
// slashes (filepath.ToSlash) first: Windows APIs accept both \ and
// /, wslpath handles both, and forward slashes avoid any backslash
// quirk in the Windows command-line encoding.
func winPathToWSL(winPath string) (string, error) {
	normalized := filepath.ToSlash(winPath)
	out, err := exec.Command("wsl", "-d", distroName, "--exec",
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

// cmdRun runs a one-shot command in the WSL2 distro. On Windows the
// distro is the persistent environment, so run is exec's muscle-memory
// twin: `dew run -- uname -a` and `dew run uname -a` both work. The
// optional `--` separator (parity with `dew run` on the macOS CLI) is
// stripped before dispatch.
func cmdRun(args []string) error {
	args = stripLeadingSeparator(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: dew run [--] <cmd> [args...]")
	}
	return cmdExec(args)
}

// stripLeadingSeparator drops a single leading "--" token so callers
// can write `dew run -- cmd` (matching the macOS CLI) or `dew run cmd`.
func stripLeadingSeparator(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// vmList prints every registered WSL2 distro and its run state, marking
// the dew-managed one. Passive: it never starts a distro.
func vmList() error {
	all, err := distroListNames()
	if err != nil {
		return fmt.Errorf("wsl --list: %w", err)
	}
	if len(all) == 0 {
		fmt.Println("dew: no WSL2 distros registered")
		return nil
	}
	running, err := distroListNames("--running")
	if err != nil {
		return fmt.Errorf("wsl --list --running: %w", err)
	}
	fmt.Print(formatVMList(all, running))
	return nil
}

// formatVMList renders the distro table. Split out from vmList so the
// ordering and state labelling are unit-testable without wsl.exe. The
// dew distro sorts first; the rest follow alphabetically.
func formatVMList(all, running map[string]bool) string {
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == distroName) != (names[j] == distroName) {
			return names[i] == distroName
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	for _, n := range names {
		state := "stopped"
		if running[n] {
			state = "running"
		}
		tag := ""
		if n == distroName {
			tag = "  (dew)"
		}
		fmt.Fprintf(&b, "  %-20s %s%s\n", n, state, tag)
	}
	return b.String()
}

// wslConfigPath returns the per-user .wslconfig path (global WSL2 tuning).
func wslConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wslconfig")
}

// mirroredNetworkingEnabled reports whether .wslconfig requests mirrored
// networking, which makes a WSL2 dev server's port reachable on the
// Windows host as plain localhost. Best-effort scan of the tiny INI.
func mirroredNetworkingEnabled() bool {
	data, err := os.ReadFile(wslConfigPath())
	if err != nil {
		return false
	}
	return hasMirroredNetworking(data)
}

// hasMirroredNetworking scans .wslconfig text for networkingMode=mirrored,
// tolerating any surrounding whitespace (spaces or tabs, e.g. a
// tab-indented `networkingMode\t=\tmirrored`) and case. Split out for
// testing.
func hasMirroredNetworking(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		// strings.Fields splits on any whitespace run, so joining with ""
		// removes every space/tab (leading, trailing, and around the =).
		compact := strings.Join(strings.Fields(line), "")
		if strings.EqualFold(compact, "networkingMode=mirrored") {
			return true
		}
	}
	return false
}

// distroState returns "not installed" / "stopped" / "running" for the
// dew distro without starting it. A non-nil error means `wsl --list`
// failed, so the state is genuinely unknown (not "not installed").
func distroState() (string, error) {
	registered, err := distroRegistered()
	if err != nil {
		return "", err
	}
	if !registered {
		return "not installed", nil
	}
	running, err := distroRunningNow()
	if err != nil {
		return "", err
	}
	if running {
		return "running", nil
	}
	return "stopped", nil
}

// vmStatusLine is the human line printed by `dew vm status`, derived
// passively from distroState (never starts the distro).
func vmStatusLine() (string, error) {
	state, err := distroState()
	if err != nil {
		return "", err
	}
	switch state {
	case "not installed":
		return "dew: not installed (run: dew setup)", nil
	case "running":
		return "dew: running", nil
	default:
		return "dew: stopped", nil
	}
}

// cmdEnv prints where the wrapper keeps things and the distro's current
// state. Handy for debugging and for scripts that need the paths.
func cmdEnv() error {
	dataDir := dewDataDir()
	rootfs := filepath.Join(dataDir, "dew-rootfs.tar.gz")
	rootfsState := "not downloaded"
	if fi, err := os.Stat(rootfs); err == nil {
		rootfsState = fmt.Sprintf("%d MB", fi.Size()/(1024*1024))
	}
	mirrored := "off"
	if mirroredNetworkingEnabled() {
		mirrored = "on"
	}
	// Report a missing WSL2 distinctly from an un-imported distro, and a
	// failed `wsl --list` distinctly from both, so `dew env` never blames
	// the distro for a WSL-level problem.
	state := "WSL2 not installed"
	if wslInstalled() {
		if s, err := distroState(); err != nil {
			state = "unknown (wsl --list failed)"
		} else {
			state = s
		}
	}
	fmt.Printf("distro         %s\n", distroName)
	fmt.Printf("state          %s\n", state)
	fmt.Printf("data dir       %s\n", dataDir)
	fmt.Printf("rootfs         %s (%s)\n", rootfs, rootfsState)
	fmt.Printf("wslconfig      %s\n", wslConfigPath())
	fmt.Printf("mirrored net   %s\n", mirrored)
	return nil
}

// doctorCheck is one diagnostic result line.
type doctorCheck struct {
	level  string // OK | WARN | FAIL
	label  string
	detail string
}

// doctorInputs are the probed facts doctor reasons about, injected so
// the decision logic is unit-testable without a live WSL2.
type doctorInputs struct {
	wslInstalled  bool
	listErr       string // non-empty if `wsl --list` failed despite WSL present
	registered    bool
	running       bool
	nodeVersion   string // "" when node is absent or the distro isn't up
	mirrored      bool
	rootfsMB      int64  // -1 when the rootfs isn't cached
	wslconfigPath string // path named in the mirrored-networking hint
}

// doctorReport turns probed facts into report lines plus an overall
// health verdict — false if any hard prerequisite failed (warnings do
// not fail the verdict). Pure: no I/O, no printing.
func doctorReport(in doctorInputs) (checks []doctorCheck, healthy bool) {
	healthy = true
	add := func(level, label, detail string) {
		checks = append(checks, doctorCheck{level, label, detail})
		if level == "FAIL" {
			healthy = false
		}
	}

	if !in.wslInstalled {
		add("FAIL", "wsl2", "not installed - run: wsl --install --no-distribution")
		return checks, false
	}
	add("OK", "wsl2", "installed")

	// A wsl --list failure means we can't tell registered/running apart;
	// report it instead of misclaiming the distro isn't imported.
	if in.listErr != "" {
		add("FAIL", "distro", "cannot list distros: "+in.listErr)
	} else {
		switch {
		case !in.registered:
			add("FAIL", "distro", fmt.Sprintf("%q not imported - run: dew setup", distroName))
		case in.running:
			add("OK", "distro", fmt.Sprintf("%q running", distroName))
		default:
			add("OK", "distro", fmt.Sprintf("%q registered (stopped)", distroName))
		}

		if in.registered {
			if in.nodeVersion != "" {
				add("OK", "node", in.nodeVersion)
			} else {
				add("WARN", "node", "not found in distro (dew up needs it)")
			}
		}
	}

	if in.mirrored {
		add("OK", "mirrored net", "enabled (dev servers reachable on localhost)")
	} else {
		add("WARN", "mirrored net", "off - add [wsl2] networkingMode=mirrored to "+in.wslconfigPath)
	}

	if in.rootfsMB >= 0 {
		add("OK", "rootfs", fmt.Sprintf("%d MB cached", in.rootfsMB))
	} else {
		add("WARN", "rootfs", "not cached (downloads on next setup)")
	}
	return checks, healthy
}

// cmdDoctor gathers the environment facts, prints an OK/WARN/FAIL line
// per check, and exits non-zero on a hard failure so scripts can gate
// on `dew doctor`. The decision logic lives in doctorReport.
func cmdDoctor() error {
	installed := wslInstalled()
	registered, running, nodeVersion, listErr := false, false, "", ""
	if installed {
		reg, err := distroRegistered()
		if err != nil {
			listErr = err.Error()
		} else {
			registered = reg
			if registered {
				if run, err := distroRunningNow(); err == nil {
					running = run
				}
				// Probing node starts the distro — acceptable for an active
				// diagnostic. Only meaningful once imported.
				if out, err := wslQuery("-d", distroName, "--exec", "node", "--version"); err == nil {
					nodeVersion = strings.TrimSpace(string(out))
				}
			}
		}
	}
	rootfsMB := int64(-1)
	if fi, err := os.Stat(filepath.Join(dewDataDir(), "dew-rootfs.tar.gz")); err == nil {
		rootfsMB = fi.Size() / (1024 * 1024)
	}

	checks, healthy := doctorReport(doctorInputs{
		wslInstalled:  installed,
		listErr:       listErr,
		registered:    registered,
		running:       running,
		nodeVersion:   nodeVersion,
		mirrored:      mirroredNetworkingEnabled(),
		rootfsMB:      rootfsMB,
		wslconfigPath: wslConfigPath(),
	})
	for _, c := range checks {
		fmt.Printf("  %-5s %-14s %s\n", c.level, c.label, c.detail)
	}
	fmt.Println()
	if !healthy {
		fmt.Println("dew doctor: some checks failed (see above)")
		os.Exit(1)
	}
	fmt.Println("dew doctor: all good")
	return nil
}
