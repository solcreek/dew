//go:build darwin

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/detect"
	"github.com/solcreek/dew/internal/nmcache"
	"github.com/solcreek/dew/internal/ocistage"
	"github.com/solcreek/dew/internal/progress"
	"github.com/solcreek/dew/internal/selfupdate"
	"github.com/solcreek/dew/internal/serialexec"
	"github.com/solcreek/dew/internal/services"
	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
	"github.com/solcreek/dew/internal/vmstate"
	vsockProto "github.com/solcreek/dew/internal/vsock"
	"github.com/solcreek/dew/pkg/dewerr"
)

// schemaVersion is the version of the --json envelope dew emits. Bumps
// when the shape changes incompatibly; additive changes don't bump.
// See pkg/dewerr/README.md for the policy.
const schemaVersion = "1.0"

var version = "dev"

var flagJSON bool
var flagStream bool
var flagEvents bool
var flagWith string
var flagImage string
var flagPlatform string

// flagEnv collects repeatable -e/--env KEY=VAL pairs for `dew run --image`.
// They are appended to the image's own env in the OCI spec.
var flagEnv []string

// flagVolumes collects -v/--volume values for `dew run --image`. Each is a
// "name:/path" or "/guest:/path" bind mount. Only one is supported for now
// (the guest dew-oci-run launcher takes a single --data dir).
var flagVolumes []string
var flagDryRun bool
var flagProfile string
var flagServicesOnly bool
var flagResetDisk bool

// flagVMName selects which VM a command targets. Empty is the default
// (unnamed) VM, preserving the historical single-VM layout
// (default.sock + vm-state.json). A non-empty name maps to its own
// socket (<name>.sock) and state dir (<name>/), so several named VMs
// can run concurrently. Set by parseFlags (start/run/up) and by
// popNameFlag (status/stop/forward/exec).
var flagVMName string

// flagTimeout is the overall wall-clock budget for `dew run`
// (boot + agent wait + exec). Zero means no overall bound; each
// stage keeps its own default deadline.
var flagTimeout time.Duration

// startReadyEvent is the payload of the `vm start` ready event. The
// shape is an agent-facing contract (pinned by TestStartReadyEvent) —
// fields are additive-only.
func startReadyEvent(socket string, pid int, profile string, elapsedMs int64) map[string]any {
	return map[string]any{
		"socket":     socket,
		"pid":        pid,
		"profile":    profile,
		"elapsed_ms": elapsedMs,
	}
}

// cfgProfileName resolves the effective profile for state reporting:
// flagProfile when set, otherwise the resolveAssets default.
func cfgProfileName() string {
	if flagProfile != "" {
		return flagProfile
	}
	return "minimal"
}

// runBudget tracks the optional --timeout deadline across cmdRun's
// stages. Each stage asks for a window: its own default, shrunk to
// whatever is left of the overall budget.
type runBudget struct{ deadline time.Time }

func newRunBudget(total time.Duration) runBudget {
	if total <= 0 {
		return runBudget{}
	}
	return runBudget{deadline: time.Now().Add(total)}
}

// window returns def, capped at the time remaining until the overall
// deadline. Without a deadline it returns def unchanged.
func (b runBudget) window(def time.Duration) time.Duration {
	if b.deadline.IsZero() {
		return def
	}
	if r := time.Until(b.deadline); r < def {
		return r
	}
	return def
}

func (b runBudget) expired() bool {
	return !b.deadline.IsZero() && !time.Now().Before(b.deadline)
}

// guestTimeout is the exec timeout to send to the guest agent: the
// remaining budget when one is set (the guest kills the command at
// our deadline, instead of us abandoning a still-running command),
// zero (agent default) otherwise.
func (b runBudget) guestTimeout() time.Duration {
	if b.deadline.IsZero() {
		return 0
	}
	r := time.Until(b.deadline)
	if r < time.Second {
		r = time.Second
	}
	return r
}

func dewDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dew")
}

// applyProfileDefaults fills in CPU / memory / disk values that the
// caller left at the global default (CPUs=1, MemoryMB=512, DiskPath="").
// minimal stays light — ad-hoc `dew run` shouldn't claim 4 host cores.
// Real-work profiles get 4 vCPUs + 2 GB because a single vCPU bottlenecks
// npm-install reify and Vite/Next transform pipelines, and modern
// bundlers (esbuild worker threads, tsx) blow past 1 GB during boot.
// Explicit --cpus / --memory overrides win because the caller set them
// to non-default values before this runs.
func applyProfileDefaults(cfg *vm.Config, profile, dataDir string) {
	switch profile {
	case "node":
		if cfg.CPUs == 1 {
			cfg.CPUs = 4
		}
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 2048
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "node.img")
			cfg.DiskGB = 4
		}
	case "python":
		if cfg.CPUs == 1 {
			cfg.CPUs = 4
		}
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 2048
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "python.img")
			cfg.DiskGB = 4
		}
	case "standard":
		if cfg.CPUs == 1 {
			cfg.CPUs = 4
		}
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 2048
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "standard.img")
			cfg.DiskGB = 10
		}
	}
}

func resolveAssets(cfg *vm.Config) error {
	dataDir := dewDataDir()
	profile := flagProfile
	if profile == "" {
		// "minimal" is the right default for ad-hoc one-shot use
		// (`dew run`, ephemeral exec). Callers that need containerd
		// or a runtime baked in (`dew app run`, `dew up` on a Node
		// project) set --profile explicitly. The earlier default
		// was "standard", which dragged every casual command through
		// the 135 MB initramfs download and 30-60s first-boot init.
		profile = "minimal"
	}

	applyProfileDefaults(cfg, profile, dataDir)

	if cfg.Kernel == "" {
		cfg.Kernel = assetCachePath(dataDir, kernelAssetName())
	}
	if cfg.Initrd == "" {
		// Pin to the profile-specific initramfs. If it's missing, the
		// auto-download block below pulls the matching one from GH Release.
		//
		// Earlier behavior fell back to `initramfs.cpio.gz` (the unprefixed
		// file, typically the minimal profile bundled by older versions or
		// left over from a different profile run). Using a minimal initramfs
		// for `--profile standard` kernel-panics at boot — `mkfs.ext4: not
		// found`, no containerd, no e2fsprogs. The fallback masked a real
		// missing-asset condition; force download instead so the user gets
		// a working setup or a clear download error.
		cfg.Initrd = assetCachePath(dataDir, initrdAssetName(profile))
	}
	// Auto-download assets on first use
	needDownload := false
	if _, err := os.Stat(cfg.Kernel); err != nil {
		needDownload = true
	}
	if _, err := os.Stat(cfg.Initrd); err != nil {
		needDownload = true
	}
	if needDownload {
		if err := downloadAssets(dataDir, profile, cfg.Kernel, cfg.Initrd, false); err != nil {
			return err
		}
	}
	if _, err := os.Stat(cfg.Kernel); err != nil {
		return fmt.Errorf("kernel not found at %s", cfg.Kernel)
	}
	if _, err := os.Stat(cfg.Initrd); err != nil {
		return fmt.Errorf("initramfs not found at %s", cfg.Initrd)
	}
	return nil
}

const releaseBaseURL = "https://github.com/solcreek/dew/releases/latest/download"

// releaseBaseURLOverride is set by tests to redirect downloads to a
// local httptest server. Empty in production.
var releaseBaseURLOverride string

// downloadAssets fetches kernel + initramfs for the given profile.
// When force is true, existing files at the destination paths are
// re-downloaded; otherwise existence at the destination is treated
// as a hit and the file is left alone. The `dew assets pull --force`
// flag is the user-facing way to invalidate; the auto-download path
// in resolveAssets is always non-force so a normal `dew up` doesn't
// re-pull on every invocation.
//
// Release builds (binary built by release.yml with ExpectedAssetSHA
// populated) verify each downloaded file against the embedded SHA
// before installing it at the destination path. A mismatch — CDN
// drift, partial download not caught by .partial, mid-transit
// corruption — fails loudly here rather than letting Apple VZ reject
// the bytes later with the opaque Code=1 the 2026-06 M4 Max reporter
// spent days debugging. Dev / local builds (empty manifest) skip
// verification — the user is expected to know what they built.
func downloadAssets(dataDir, profile, kernelPath, initrdPath string, force bool) error {
	os.MkdirAll(dataDir, 0755)

	base := releaseBaseURL
	if releaseBaseURLOverride != "" {
		base = releaseBaseURLOverride
	}

	kAsset := kernelAssetName()
	iAsset := initrdAssetName(profile)

	files := []struct {
		url  string
		dest string
		name string
		sha  string // expected SHA256; "" in dev builds → no verify
	}{
		{
			fmt.Sprintf("%s/%s", base, kAsset),
			kernelPath,
			"kernel",
			ExpectedAssetSHA[kAsset],
		},
		{
			fmt.Sprintf("%s/%s", base, iAsset),
			initrdPath,
			fmt.Sprintf("initramfs (%s)", profile),
			ExpectedAssetSHA[iAsset],
		},
	}

	type result struct {
		name    string
		written int64
		err     error
	}
	results := make([]result, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		if _, err := os.Stat(f.dest); err == nil {
			if !force {
				results[i] = result{name: f.name, written: -1}
				continue
			}
			// Remove the existing file so fetchAsset's tmp+rename
			// dance starts from a clean slate. Missing-file error
			// is fine (the rename target will be re-created).
			_ = os.Remove(f.dest)
		}
		fmt.Fprintf(os.Stderr, "  downloading %s...\n", f.name)
		wg.Add(1)
		go func(idx int, file struct {
			url  string
			dest string
			name string
			sha  string
		}) {
			defer wg.Done()
			results[idx] = fetchAsset(file.url, file.dest, file.name, profile, file.sha)
		}(i, f)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			return r.err
		}
		if r.written > 0 {
			fmt.Fprintf(os.Stderr, "  ✓ %s %dMB\n", r.name, r.written/1024/1024)
		}
	}
	return nil
}

func fetchAsset(url, dest, name, profile, expectedSHA string) (r struct {
	name    string
	written int64
	err     error
}) {
	r.name = name
	resp, err := http.Get(url)
	if err != nil {
		r.err = fmt.Errorf("download %s: %w", name, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		r.err = fmt.Errorf("download %s: HTTP %d\n\n  Assets not available at %s\n  Build locally: bash initramfs/build.sh %s",
			name, resp.StatusCode, url, profile)
		return
	}
	tmp := dest + ".partial"
	// dest may live in a directory that doesn't exist yet (e.g. a
	// --kernel path pointing somewhere fresh); create it rather than
	// failing the download with a confusing "no such file" error.
	if err := os.MkdirAll(filepath.Dir(tmp), 0755); err != nil {
		r.err = fmt.Errorf("create %s: %w", filepath.Dir(tmp), err)
		return
	}
	out, err := os.Create(tmp)
	if err != nil {
		r.err = fmt.Errorf("create %s: %w", tmp, err)
		return
	}
	// Stream the body through a sha256 hasher while writing to disk.
	// Costs nothing extra vs the io.Copy that was already happening,
	// and the digest is ready before we rename — so a mismatch deletes
	// the .partial file rather than installing it under dest.
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, hasher), resp.Body)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		r.err = fmt.Errorf("download %s: %w", name, err)
		return
	}
	if expectedSHA != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != expectedSHA {
			os.Remove(tmp)
			r.err = fmt.Errorf("verify %s: SHA mismatch\n  expected: %s\n  got:      %s\n  the asset on the release CDN doesn't match the bytes this dew binary was built against",
				name, expectedSHA, got)
			return
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		r.err = fmt.Errorf("install %s: %w", name, err)
		return
	}
	r.written = written
	return
}

// parseAssetsArgs extracts the profile and force flag from
// `dew assets <sub> ...` args (args[0] is the subcommand). The profile
// may be given positionally (`dew assets pull standard`) or via
// `--profile standard`; a later --profile wins. Falls back to def.
//
// The positional form was previously ignored — `dew assets pull
// standard` silently pulled the default (minimal), so users couldn't
// refresh a specific profile's assets.
func parseAssetsArgs(args []string, def string) (profile string, force bool) {
	profile = def
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" && i+1 < len(args):
			profile = args[i+1]
			i++
		case a == "--force":
			force = true
		case !strings.HasPrefix(a, "-"):
			profile = a
		}
	}
	return profile, force
}

// staleDiskHint returns a recovery message for a disk-profile VM that
// booted but whose agent never came up. The most common cause is a
// stale/corrupt persistent disk image from a previous version: the
// guest panics at switch_root ("/init-stage2: Exec format error"), so
// the host sees only an agent timeout, never a VZ error — meaning the
// VZ Code=2 "delete the image" path can't fire. Returns "" with no disk.
func staleDiskHint(diskPath, rebuildCmd string) string {
	if diskPath == "" {
		return ""
	}
	return fmt.Sprintf(
		"the VM booted but its agent never came up — a stale disk image from a\n"+
			"  previous version is a common cause. Rebuild it with:\n"+
			"    %s\n"+
			"  or delete it manually (resets VM state):\n"+
			"    rm %s",
		rebuildCmd, diskPath)
}

func cmdAssets(args []string) error {
	sub := "pull"
	if len(args) > 0 {
		sub = args[0]
	}

	dataDir := dewDataDir()
	def := flagProfile
	if def == "" {
		def = "minimal"
	}
	profile, force := parseAssetsArgs(args, def)

	switch sub {
	case "pull":
		kernelPath := assetCachePath(dataDir, kernelAssetName())
		initrdPath := assetCachePath(dataDir, initrdAssetName(profile))
		if force {
			fmt.Fprintf(os.Stderr, "  profile: %s\n  target:  %s\n  mode:    force re-download\n\n", profile, dataDir)
		} else {
			fmt.Fprintf(os.Stderr, "  profile: %s\n  target:  %s\n\n", profile, dataDir)
		}
		return downloadAssets(dataDir, profile, kernelPath, initrdPath, force)

	case "list":
		entries, err := os.ReadDir(dataDir)
		if err != nil {
			fmt.Println("No assets downloaded yet.")
			return nil
		}
		for _, e := range entries {
			info, _ := e.Info()
			if info != nil {
				fmt.Printf("  %-40s %dMB\n", e.Name(), info.Size()/1024/1024)
			}
		}
		return nil

	case "path":
		fmt.Println(dataDir)
		return nil

	default:
		return fmt.Errorf("unknown assets subcommand %q (use: pull, list, path)", sub)
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(int(dewerr.CodeUsage))
	}

	// Allow global no-value flags to appear before the subcommand:
	// `dew --json apps`, `dew --events up`, etc. Previously the
	// dispatcher saw `--json` as the command and errored. Per-command
	// parseFlags() still picks up flags wherever they sit; we only
	// need to skip past leading flags when finding the subcommand
	// token. Flags that take a value go in their command-specific
	// position as before.
	dispatchArgs := stripLeadingGlobalFlags(os.Args[1:])
	if len(dispatchArgs) == 0 {
		// `dew --json` with nothing else — show usage, exit 2.
		printUsage()
		os.Exit(int(dewerr.CodeUsage))
	}
	cmd := dispatchArgs[0]

	// Pre-scan args for global no-value flags so they're set BEFORE
	// cmd dispatch — covers cases where a subcommand's own arg parser
	// doesn't run parseFlags() (e.g. cmdShare). Position-independent:
	// `dew --events share 3000` and `dew share --events 3000` both work.
	//
	// For passthrough commands (exec/run/logs) only the LEADING globals
	// count — scanning the guest command's own flags would mistake e.g.
	// `dew exec curl --json url` for dew's JSON mode. Those commands
	// parse their own dew flags, so nothing is lost.
	scan := globalFlagScanArgs(os.Args[1:], dispatchArgs, passthroughCommands[cmd])
	for _, a := range scan {
		switch a {
		case "--json":
			flagJSON = true
		case "--events":
			flagEvents = true
		case "--stream":
			flagStream = true
		case "--dry-run":
			flagDryRun = true
		}
	}

	// Only check for updates on user-facing commands, not internal (exec, start).
	// Skip in JSON mode — agents don't want noise.
	if !flagJSON && cmd != "exec" && cmd != "start" && cmd != "run" && cmd != "serve" {
		go selfupdate.CheckBackground(version)
	}

	// Per-subcommand --help: intercept before dispatch so commands
	// that didn't previously parse the flag (most of them) don't
	// error with "unknown flag". A user running `dew up --help`
	// expects a help block, not a flag error.
	subArgs := dispatchArgs[1:]
	if len(subArgs) > 0 {
		for _, a := range subArgs {
			if a == "--help" || a == "-h" {
				// Namespace-aware: `dew vm start --help` resolves the
				// "start" block; `dew vm --help` the "vm" block.
				if printSubcommandHelpPath(cmd, subArgs) {
					return
				}
				break
			}
		}
	}

	// `dew help` and the top-level `dew --help`/`-h` render the grouped
	// usage. (Per-command --help is handled by the interception above.)
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		printUsage()
		return
	}
	// `dew version` is a cobra command, but the bare flag aliases
	// `--version`/`-v` aren't subcommands — handle them here so they
	// keep printing the version (they did before the cobra migration).
	if cmd == "--version" || cmd == "-v" {
		fmt.Printf("dew %s\n", version)
		return
	}

	// All commands now dispatch through the cobra root (see cobra.go).
	var err error
	handled, derr := dispatchCobra(cmd, subArgs)
	if handled {
		err = derr
	} else {
		err = dewerr.Newf(dewerr.CodeUsage, "unknown command %q", cmd)
		if !flagJSON {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
			printUsage()
			os.Exit(int(dewerr.CodeUsage))
		}
	}
	if err != nil {
		code := dewerr.CodeOf(err)
		if flagJSON {
			emitErrorJSON(err, code)
		} else {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
		}
		os.Exit(int(code))
	}
}

// emitErrorJSON writes the --json error envelope to stdout. The shape
// is documented in docs/exit-codes.md and is stable across additive
// schema_version minor bumps.
func emitErrorJSON(err error, code dewerr.Code) {
	errObj := map[string]any{
		"code":      code.Slug(),
		"exit_code": int(code),
		"message":   err.Error(),
		"retryable": dewerr.Retryable(err),
	}
	var typed *dewerr.Error
	if errors.As(err, &typed) {
		if typed.RetryAfter > 0 {
			errObj["retry_after_ms"] = typed.RetryAfter.Milliseconds()
		}
		if len(typed.Hint) > 0 {
			errObj["hint"] = typed.Hint
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":             false,
		"schema_version": schemaVersion,
		"error":          errObj,
	})
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dew — sandboxed Linux compute, agent-native and human-friendly

Try: dew run -- uname -a

Dev (project-aware):
  dew up [dir]                   Start dev environment (auto-detect project)
  dew up --with postgres,redis   Dev with services
  dew services                   List services + connection strings
  dew logs <service>             Show a service's container logs
  dew down                       Stop dev environment

VM (generic compute primitive):
  dew vm start [--profile X]     Boot a VM without project detection
  dew vm stop                    Stop the running VM
  dew vm status                  Show whether a VM is running
  dew vm forward add HOST:GUEST  Add a host→guest port forward
  dew vm forward remove H:G      Remove an existing forward
  dew vm forward list            List active forwards

Share:
  dew share [port]               Create temporary public HTTPS URL

Deploy (to a server from 'dew server create' or any VPS):
  dew build [dir]                Package app for deployment
  dew deploy <target>            Deploy to remote server
  dew env ...                    Manage environment variables
  dew auth ...                   Manage credentials

Infrastructure:
  dew server create [--provider]  Provision a VPS
  dew server list                 List managed servers
  dew server destroy <name>       Remove a server
  dew serve                       Run deploy receiver

Advanced:
  dew run [--] <cmd>             Execute in ephemeral VM
  dew exec <cmd>                 Execute in running VM
  dew assets ...                 Manage VM images
  dew doctor [--verbose]         Diagnose environment issues
                                 (--verbose dumps the VM config
                                  during the boot test)
  dew update                     Update to latest version
  dew version                    Print version

Output:
  --json        Machine-readable JSON (all commands)
  --events      NDJSON lifecycle stream
  --stream      Stream stdout/stderr
  --dry-run     Validate without executing (up, deploy, server create)

Network:
  --network     Enable guest networking (off by default for dew run).
                dew up and dew vm start enable it automatically.
  --network-policy open|restricted
                Implies --network. open (default when --network is
                set) allows all outbound; restricted is default-DROP
                except loopback, DNS, and IPs added via --allow-host.
                Package installs (apk, npm, pip) will fail under
                restricted unless you allow the registry's hosts.
  --allow-host HOST
                Repeatable. Resolves HOST on the host side and permits
                the guest to reach those IPs. Only meaningful with
                --network-policy=restricted.

Containers:
  --image REF   (dew run) Pull an OCI image on the host and run it in the VM
                via crun. A trailing -- <cmd> overrides the image entrypoint.
                dew run does not auto-forward ports — add --publish/-p (or
                --forward), or use dew up --with for managed services.
  --publish, -p HOST:CONTAINER
                Repeatable. Forward host port HOST to the container's
                CONTAINER port (the container runs --net=host, so this is
                --forward by another name). Example: -p 8080:80
  --env, -e KEY=VALUE
                (dew run --image) Repeatable. Appended to the image's own
                env in the container. Example: -e LOG_LEVEL=debug
  --volume, -v NAME:/path | /guest:/path
                (dew run --image) Persistent bind mount. NAME maps to
                /var/lib/dew/volumes/NAME on the VM's disk and survives
                across runs. One volume per run for now. Example:
                -v pgdata:/var/lib/postgresql/data
  --platform OS/ARCH
                Image platform to pull (default: the guest arch). Set
                linux/amd64 with --rosetta to run an amd64 image.
  --with NAMES  (dew up) Comma-separated predefined services to run alongside
                the project (e.g. postgres,redis). Each is pulled on the host,
                run via crun, and its port forwarded.

Compatibility:
  --rosetta     Apple Silicon only. Mounts Apple's Rosetta translator into
                the guest and registers binfmt_misc, so x86_64/amd64 binaries
                run under translation. To run an amd64 container, pair it with
                --image and --platform:
                dew run --rosetta --platform linux/amd64 --image <ref>
                Expect ~0.7-0.8x native speed on compiled code; far slower on
                crypto/SIMD work.
`)
}

// stripLeadingGlobalFlags consumes leading no-value global flags
// (--json / --events / --stream / --dry-run) and returns the remaining
// args starting at the subcommand. The flags themselves are not lost:
// the main pre-scan above already set flagJSON, and each subcommand's
// parseFlags() picks the others up wherever they appear. The point is
// just to skip past them when picking the dispatch token, so users
// can write `dew --json apps` instead of being forced into the rigid
// `dew apps --json`.
//
// Flags that take a value (--cpus N, --memory N, --with X, --profile X,
// etc.) are not skipped here — they go in their command-specific
// position as before. If someone writes `dew --with postgres apps`, the
// first non-flag token (postgres) wins as the subcommand, which surfaces
// a clear unknown-command error rather than silently misparsing.
func stripLeadingGlobalFlags(args []string) []string {
	noValueGlobals := map[string]bool{
		"--json":    true,
		"--events":  true,
		"--stream":  true,
		"--dry-run": true,
	}
	i := 0
	for i < len(args) && noValueGlobals[args[i]] {
		i++
	}
	return args[i:]
}

func parseFlags(args []string) (vm.Config, []string, error) {
	cfg := vm.Config{
		CPUs:     1,
		MemoryMB: 512,
		CmdLine:  "console=hvc0",
	}
	var remaining []string
	// Reset command-scoped globals: parseFlags runs once per command, but tests
	// reuse the process, so a prior --image/--platform/--timeout must not leak
	// into a later invocation that didn't pass them.
	flagTimeout = 0
	flagImage = ""
	flagPlatform = ""
	flagEnv = nil
	flagVolumes = nil
	flagWith = ""
	flagServicesOnly = false
	flagResetDisk = false
	flagVMName = ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			remaining = args[i+1:]
			return cfg, remaining, nil
		case "--timeout":
			i++
			if i >= len(args) {
				return cfg, nil, dewerr.New(dewerr.CodeUsage, "--timeout requires a duration (e.g. 90s, 5m)")
			}
			d, perr := time.ParseDuration(args[i])
			if perr != nil || d <= 0 {
				return cfg, nil, dewerr.Newf(dewerr.CodeUsage, "--timeout: invalid duration %q", args[i])
			}
			flagTimeout = d
		case "--kernel":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--kernel requires a path")
			}
			cfg.Kernel = args[i]
		case "--initrd":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--initrd requires a path")
			}
			cfg.Initrd = args[i]
		case "--cpus":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--cpus requires a number")
			}
			n := 0
			for _, c := range args[i] {
				if c < '0' || c > '9' {
					return cfg, nil, fmt.Errorf("--cpus: invalid number %q", args[i])
				}
				n = n*10 + int(c-'0')
			}
			cfg.CPUs = uint(n)
		case "--memory":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--memory requires a number (MB)")
			}
			n := uint64(0)
			for _, c := range args[i] {
				if c < '0' || c > '9' {
					return cfg, nil, fmt.Errorf("--memory: invalid number %q", args[i])
				}
				n = n*10 + uint64(c-'0')
			}
			cfg.MemoryMB = n
		case "--network":
			cfg.Network = true
		case "--rosetta":
			cfg.EnableRosetta = true
		case "--network-policy":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--network-policy requires open|restricted")
			}
			switch args[i] {
			case "open", "restricted":
				cfg.NetworkPolicy = args[i]
				cfg.Network = true // policy implies network is on
			default:
				return cfg, nil, fmt.Errorf("--network-policy: must be open or restricted (got %q)", args[i])
			}
		case "--allow-host":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--allow-host requires a hostname or IP")
			}
			h := args[i]
			ips, err := net.LookupHost(h)
			if err != nil {
				return cfg, nil, fmt.Errorf("--allow-host %q: %w", h, err)
			}
			for _, ip := range ips {
				// IPv4 only for now — guest iptables-restore is v4-style.
				if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
					cfg.AllowHosts = append(cfg.AllowHosts, parsed.To4().String())
				}
			}
		case "--vsock":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--vsock requires a port number")
			}
			n := uint32(0)
			for _, c := range args[i] {
				if c < '0' || c > '9' {
					return cfg, nil, fmt.Errorf("--vsock: invalid port %q", args[i])
				}
				n = n*10 + uint32(c-'0')
			}
			cfg.VsockPort = n
		case "--share":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--share requires hostpath[:rw|:ro] or tag:hostpath[:rw|:ro]")
			}
			sd, err := parseShare(args[i])
			if err != nil {
				return cfg, nil, err
			}
			cfg.SharedDirs = append(cfg.SharedDirs, sd)
		case "--profile":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--profile requires a name")
			}
			flagProfile = args[i]
		case "--name":
			i++
			if i >= len(args) {
				return cfg, nil, dewerr.New(dewerr.CodeUsage, "--name requires a VM name")
			}
			if err := validateVMName(args[i]); err != nil {
				return cfg, nil, err
			}
			flagVMName = args[i]
		case "--disk":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--disk requires a path")
			}
			cfg.DiskPath = args[i]
		case "--forward":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--forward requires hostPort:guestPort")
			}
			fwd, err := parseForward(args[i])
			if err != nil {
				return cfg, nil, err
			}
			cfg.Forwards = append(cfg.Forwards, fwd)
		case "--with":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--with requires service names (e.g. postgres,redis)")
			}
			flagWith = args[i]
		case "--services-only", "--no-dev":
			flagServicesOnly = true
		case "--image":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--image requires an OCI image reference (e.g. docker.io/library/redis:7-alpine)")
			}
			flagImage = args[i]
		case "--platform":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--platform requires an os/arch (e.g. linux/amd64)")
			}
			flagPlatform = args[i]
		case "--env", "-e":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--env requires KEY=VALUE")
			}
			if !strings.Contains(args[i], "=") {
				return cfg, nil, fmt.Errorf("--env: expected KEY=VALUE, got %q", args[i])
			}
			flagEnv = append(flagEnv, args[i])
		case "--publish", "-p":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--publish requires hostPort:containerPort")
			}
			// crun runs --net=host, so the container binds its port on the VM
			// network: publishing is just a host->guest forward whose guest
			// port is the container port. Reuse the --forward parser/transport.
			fwd, ferr := parseForward(args[i])
			if ferr != nil {
				return cfg, nil, fmt.Errorf("--publish: expected hostPort:containerPort, got %q", args[i])
			}
			cfg.Forwards = append(cfg.Forwards, fwd)
		case "--volume", "-v":
			i++
			if i >= len(args) {
				return cfg, nil, fmt.Errorf("--volume requires name:/path or /host:/path")
			}
			if _, _, verr := parseVolume(args[i]); verr != nil {
				return cfg, nil, verr
			}
			flagVolumes = append(flagVolumes, args[i])
		case "--stream":
			flagStream = true
		case "--events":
			flagEvents = true
			flagStream = true
		case "--json":
			flagJSON = true
		case "--dry-run":
			flagDryRun = true
		case "--reset-disk":
			flagResetDisk = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return cfg, nil, fmt.Errorf("unknown flag %q", args[i])
			}
			remaining = args[i:]
			// Continue scanning past the first positional so flags
			// appearing AFTER it still get parsed. Without this,
			// `dew up <dir> --dry-run` silently dropped --dry-run.
			collected := []string{args[i]}
			for j := i + 1; j < len(args); j++ {
				if strings.HasPrefix(args[j], "-") {
					// Recurse the remaining flag stream so each
					// known flag still gets its case-arm.
					_, _, err := parseFlags(args[j:])
					if err != nil {
						return cfg, nil, err
					}
					break
				}
				collected = append(collected, args[j])
			}
			return cfg, collected, nil
		}
	}

	return cfg, remaining, nil
}

// validateVMName guards the VM name before it becomes a filesystem path
// component (<name>.sock and a <name>/ state dir under the dew state
// directory). Restricting to a small safe charset prevents path
// traversal and collisions — important because a front door may derive
// the name from an untrusted source (e.g. an SSH username).
func validateVMName(name string) error {
	switch {
	case name == "":
		return dewerr.New(dewerr.CodeUsage, "--name requires a non-empty VM name")
	case name == "default":
		// "default" is the reserved on-disk name for the unnamed VM
		// (default.sock); taking it explicitly would alias two notions
		// of the same VM. Plain `dew vm ...` already targets it.
		return dewerr.New(dewerr.CodeUsage, `"default" is reserved; omit --name to target the default VM`)
	case len(name) > 64:
		return dewerr.Newf(dewerr.CodeUsage, "--name too long (%d chars, max 64)", len(name))
	}
	for _, r := range name {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return dewerr.Newf(dewerr.CodeUsage, "invalid VM name %q: use letters, digits, '-' or '_'", name)
		}
	}
	return nil
}

// popNameFlag extracts a `--name <vm>` pair from args for the commands
// that don't run parseFlags (status/stop/forward/exec), sets flagVMName,
// and returns args with the pair removed. flagVMName is reset first so a
// name never leaks across commands in a reused process (e.g. tests).
func popNameFlag(args []string) ([]string, error) {
	flagVMName = ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--name" {
			i++
			if i >= len(args) {
				return nil, dewerr.New(dewerr.CodeUsage, "--name requires a VM name")
			}
			if err := validateVMName(args[i]); err != nil {
				return nil, err
			}
			flagVMName = args[i]
			continue
		}
		out = append(out, args[i])
	}
	return out, nil
}

// clearVMState removes the VM's lifecycle file and, for a named VM, its
// now-empty <name>/ state directory, so stopped named VMs don't leave
// dangling dirs under the state root. Best-effort: os.Remove leaves a
// non-empty dir (and the shared default state root) untouched.
func clearVMState(name string, pid int) {
	dir := vmstate.DirFor(daemon.SocketDir(), name)
	vmstate.Clear(dir, pid)
	if name != "" {
		os.Remove(dir)
	}
}

// appendGuestParams plumbs the host-side flags that the guest's
// init-stage2 reads from /proc/cmdline. Both cmdStart and cmdRun call
// it so --share and --network-policy behave identically across them.
// dew.cmd (the base64-encoded boot-time command) is cmdStart-only and
// stays inline there because cmdRun delivers the command over vsock.
func appendGuestParams(cfg *vm.Config) {
	for _, sd := range cfg.SharedDirs {
		cfg.CmdLine += fmt.Sprintf(" dew.share=%s:/%s", sd.Tag, sd.Tag)
	}
	if cfg.EnableRosetta {
		cfg.CmdLine += " dew.rosetta=1"
	}
	if cfg.Network && cfg.NetworkPolicy == "restricted" {
		cfg.CmdLine += " dew.netpolicy=restricted"
		if len(cfg.AllowHosts) > 0 {
			cfg.CmdLine += " dew.allow=" + strings.Join(cfg.AllowHosts, ",")
		} else if !flagJSON {
			// Surface the most common foot-gun: restricted mode
			// blocks everything outbound by default, so package
			// installs (apk add, npm install, pip install) silently
			// fail when no --allow-host has been passed. The agent
			// report flagged apk under restricted as "exits -1 with
			// no error output" — this hint sits before the boot so
			// the user has a chance to ^C and retry.
			fmt.Fprintf(os.Stderr,
				"dew: --network-policy=restricted with no --allow-host — "+
					"package installs and other outbound traffic will be blocked.\n"+
					"     Add --allow-host <host> for each registry/dependency host, "+
					"or use --network-policy=open.\n")
		}
	}
}

func cmdStart(args []string) error {
	cfg, cmdArgs, err := parseFlags(args)
	if err != nil {
		return err
	}
	if err := resolveAssets(&cfg); err != nil {
		return err
	}

	// --reset-disk: rebuild the persistent disk fresh (recovery for a
	// stale/corrupt image from a previous version). See cmdUp.
	if flagResetDisk && cfg.DiskPath != "" {
		if err := os.Remove(cfg.DiskPath); err == nil {
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "dew: reset disk: %s\n", cfg.DiskPath)
			}
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "dew: could not reset disk %s: %v\n", cfg.DiskPath, err)
		}
	}

	// Network on by default for `dew vm start` (and its legacy alias
	// `dew start`). The help text has always claimed this; the
	// implementation did not. Aligning the two — grove and any other
	// caller that expects a network-enabled VM no longer has to pass
	// --network. Opt-out is `--network-policy=restricted` (which
	// still implies Network=true but locks down outbound traffic).
	if !cfg.Network && cfg.NetworkPolicy == "" {
		cfg.Network = true
	}

	// Always enable vsock (needed for daemon exec + port forwarding)
	if cfg.VsockPort == 0 {
		cfg.VsockPort = uint32(vsockProto.DefaultPort)
	}

	appendGuestParams(&cfg)

	token := generateToken()

	if len(cmdArgs) > 0 {
		raw := strings.Join(cmdArgs, " ")
		encoded := base64Encode(raw)
		cfg.CmdLine += " dew.cmd=" + encoded
	}

	// Machine-readable mode promises NDJSON-only stdout; the guest
	// serial console defaults to os.Stdout and would interleave boot
	// logs with the event stream, so route it to stderr instead.
	if (flagJSON || flagEvents) && cfg.Console == nil {
		cfg.Console = &vm.ConsoleFiles{In: os.Stdin, Out: os.Stderr}
	}

	d, err := darwin.New(cfg)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dew: booting VM (cpus=%d, memory=%dMB)\n", cfg.CPUs, cfg.MemoryMB)
	start := time.Now()

	// --timeout for start mode bounds the path to readiness (boot +
	// token handshake). Once ready, the process stays in the
	// foreground indefinitely — the VM lives and dies with it.
	budget := newRunBudget(flagTimeout)

	stateDir := vmstate.DirFor(daemon.SocketDir(), flagVMName)
	startProfile := cfgProfileName()
	startedAt := time.Now().UTC()
	_ = vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseBooting, Mode: "start",
		Profile: startProfile, StartedAt: startedAt,
	})
	defer clearVMState(flagVMName, os.Getpid())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	bootCtx := ctx
	if flagTimeout > 0 {
		var bootCancel context.CancelFunc
		bootCtx, bootCancel = context.WithTimeout(ctx, budget.window(time.Hour))
		defer bootCancel()
	}
	if err := d.Start(bootCtx); err != nil {
		if budget.expired() {
			return dewerr.Newf(dewerr.CodeTimeout, "vm start: timed out after %s during boot (--timeout)", flagTimeout)
		}
		return err
	}

	fmt.Fprintf(os.Stderr, "dew: VM running (%s)\n", time.Since(start).Round(time.Millisecond))

	// Remove stale socket from previous run
	os.Remove(daemon.SocketPath(flagVMName))

	// Wait for guest agent and inject auth token. Wall-clock deadline,
	// not attempt count — see the cmdRun agent wait for why.
	fmt.Fprintf(os.Stderr, "dew: waiting for guest agent\n")
	tokenSent := false
	tokenDeadline := time.Now().Add(budget.window(30 * time.Second))
	for {
		if err := sendToken(d, cfg.VsockPort, token); err == nil {
			tokenSent = true
			break
		}
		if time.Now().After(tokenDeadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !tokenSent {
		// With an explicit budget, "not ready in time" is a hard
		// failure the caller asked to be told about — stop the VM and
		// exit with the timeout code instead of limping along.
		if flagTimeout > 0 {
			d.Stop(context.Background())
			return dewerr.Newf(dewerr.CodeTimeout, "vm start: guest agent not ready within %s (--timeout) — run 'dew doctor' to check assets", flagTimeout)
		}
		fmt.Fprintf(os.Stderr, "dew: warning: token handshake failed, daemon may not work (run 'dew doctor' to check assets)\n")
		if h := staleDiskHint(cfg.DiskPath, "dew vm start --reset-disk"); h != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
	}

	// Start daemon socket AFTER token is set (so clients can exec immediately).
	// Initial cfg.Forwards register via daemon.AddForward so the same path
	// serves runtime forward-add requests (`dew forward add ...`).
	dmn := &daemon.State{
		VM:         d,
		Token:      token,
		VsockPort:  cfg.VsockPort,
		SocketPath: daemon.SocketPath(flagVMName),
	}
	if err := dmn.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "dew: daemon: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "dew: daemon socket %s\n", dmn.SocketPath)
		// Machine-readable ready marker (--json/--events): one NDJSON
		// line on stdout once exec is actually available, so agents
		// can wait on it instead of scraping stderr. Mirrors the
		// `dew up` ready event pattern (see ready_event_test.go).
		emitEvent("ready", startReadyEvent(dmn.SocketPath, os.Getpid(), startProfile, time.Since(start).Milliseconds()))
	}
	_ = vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "start",
		Profile: startProfile, StartedAt: startedAt,
	})
	for _, f := range cfg.Forwards {
		if _, err := dmn.AddForward(f.HostPort, f.GuestPort); err != nil {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "dew: forwarding 127.0.0.1:%d → guest:%d\n", f.HostPort, f.GuestPort)
	}

	<-ctx.Done()

	dmn.Stop()
	fmt.Fprintf(os.Stderr, "\ndew: stopping VM\n")
	return d.Stop(context.Background())
}

func cmdRun(args []string) error {
	cfg, cmdArgs, err := parseFlags(args)
	if err != nil {
		return err
	}
	// -e/--env and -v/--volume only feed the OCI spec, so they're meaningless
	// without --image. Erroring beats silently dropping what the user set.
	if len(flagEnv) > 0 && flagImage == "" {
		return fmt.Errorf("--env is only supported with --image")
	}
	if len(flagVolumes) > 0 && flagImage == "" {
		return fmt.Errorf("--volume is only supported with --image")
	}
	// The guest dew-oci-run launcher takes a single --data bind, so cap at one
	// for now rather than silently mounting only the last.
	if len(flagVolumes) > 1 {
		return fmt.Errorf("only one --volume is supported for now")
	}
	// --image runs the container via crun overlay, which needs an ext4 disk.
	// Default to the node profile when the user didn't pick a disk profile.
	if flagImage != "" && (flagProfile == "" || flagProfile == "minimal") {
		flagProfile = "node"
	}
	if err := resolveAssets(&cfg); err != nil {
		return err
	}

	// One wall-clock budget spans host-side staging + boot + agent wait + exec,
	// so --timeout bounds the whole run (and cancels a slow registry pull).
	budget := newRunBudget(flagTimeout)

	// --image <ref>: pull+stage an arbitrary OCI image on the host, share its
	// bundle at /oci-stage, and run it in the guest via crun (dew-oci-run). Any
	// `-- <cmd>` overrides the image entrypoint. Foreground so output streams
	// and the container's exit code propagates.
	if flagImage != "" {
		stageRoot := filepath.Join(dewDataDir(), "oci-stage", strconv.Itoa(os.Getpid()))
		os.RemoveAll(stageRoot)
		defer os.RemoveAll(stageRoot)
		fmt.Fprintf(os.Stderr, "dew: staging image %s\n", flagImage)
		stageCtx := context.Background()
		if flagTimeout > 0 {
			var cancel context.CancelFunc
			stageCtx, cancel = context.WithTimeout(stageCtx, budget.window(flagTimeout))
			defer cancel()
		}
		// A single -v/--volume becomes a persistent bind on the guest ext4
		// disk: dew-oci-run --data mkdir's the source, and ociSpec writes the
		// mount into config.json. Pre-validated at parse time.
		var data *ocistage.Bind
		var dataArg string
		if len(flagVolumes) == 1 {
			src, dest, _ := parseVolume(flagVolumes[0])
			data = &ocistage.Bind{Source: src, Destination: dest}
			dataArg = src + ":" + dest
		}
		if _, err := ocistage.Stage(stageCtx, flagImage, ocistage.Options{
			StageDir: filepath.Join(stageRoot, "app"),
			Name:     "app",
			Cmd:      cmdArgs,
			Env:      flagEnv,      // extra -e/--env vars appended to the image env
			Data:     data,         // optional -v/--volume persistent bind mount
			Platform: flagPlatform, // empty = guest arch; set e.g. linux/amd64 with --rosetta
		}); err != nil {
			return fmt.Errorf("stage image: %w", err)
		}
		cfg.SharedDirs = append(cfg.SharedDirs, vm.SharedDir{Tag: "oci-stage", HostPath: stageRoot, ReadOnly: true})
		cmdArgs = []string{"dew-oci-run"}
		if dataArg != "" {
			cmdArgs = append(cmdArgs, "--data", dataArg)
		}
		cmdArgs = append(cmdArgs, "/oci-stage/app", "app")
	}

	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command specified (use -- <cmd> or --image <ref>)")
	}

	// Always enable vsock for run mode
	if cfg.VsockPort == 0 {
		cfg.VsockPort = 1024
	}

	appendGuestParams(&cfg)

	token := generateToken()

	console, hostReader, hostWriter, err := vm.NewConsolePipe()
	if err != nil {
		return fmt.Errorf("console pipe: %w", err)
	}
	cfg.Console = console

	d, err := darwin.New(cfg)
	if err != nil {
		return err
	}

	// budget was created before staging so --timeout spans the whole run.
	timeoutErr := func(stage string) error {
		return dewerr.Newf(dewerr.CodeTimeout, "run: timed out after %s during %s (--timeout)", flagTimeout, stage)
	}

	stateDir := vmstate.DirFor(daemon.SocketDir(), flagVMName)
	profile := cfgProfileName()
	startedAt := time.Now().UTC()
	_ = vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseBooting, Mode: "run",
		Profile: profile, StartedAt: startedAt,
	})
	defer clearVMState(flagVMName, os.Getpid())

	fmt.Fprintf(os.Stderr, "dew: booting VM\n")
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), budget.window(60*time.Second))
	defer cancel()

	if err := d.Start(ctx); err != nil {
		if budget.expired() {
			return timeoutErr("boot")
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "dew: VM running (%s)\n", time.Since(start).Round(time.Millisecond))

	// serialexec's reader goroutine starts draining the console from
	// here on, so the guest never blocks on a full console pipe and
	// readiness is latched as soon as the boot banner appears.
	sExec := serialexec.New(hostReader, hostWriter)

	// Wait for guest agent to come up on vsock, then send the auth
	// token. The wait is a wall-clock deadline, not an attempt count:
	// a single connect attempt can eat seconds (or, against a guest
	// with no vsock transport, would block forever without the bound
	// in vsockConnectAttempt), so counting iterations measured nothing.
	// 60s covers cold first-boot of any profile.
	fmt.Fprintf(os.Stderr, "dew: waiting for guest agent\n")
	const vsockReadySec = 60
	agentDeadline := time.Now().Add(budget.window(vsockReadySec * time.Second))
	var tokenSent bool
	for {
		remaining := time.Until(agentDeadline)
		if remaining <= 0 {
			break
		}
		conn, err := vsockConnectAttempt(d, cfg.VsockPort, remaining)
		if err == nil {
			req := vsockProto.SetTokenRequest{Type: vsockProto.TypeSetToken, Token: token}
			vsockProto.WriteJSON(conn, &req)
			var resp vsockProto.ConnectResponse
			vsockProto.ReadJSONTimeout(conn, &resp, 5*time.Second)
			conn.Close()
			tokenSent = resp.OK
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if tokenSent {
		_ = vmstate.Write(stateDir, vmstate.State{
			PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "run",
			Profile: profile, StartedAt: startedAt,
		})
	}

	// Port forwards (-p/--publish, --forward) for `dew run`. Unlike
	// `dew vm start`, run has no daemon socket, but AddForward only needs the
	// VM + vsock token to stand up a host listener that proxies into the guest,
	// so reuse it directly. Listeners live for the duration of the run (e.g. a
	// foreground server image) and are reclaimed when the process exits. Gated
	// on tokenSent because the guest proxy leg authenticates with the token.
	if tokenSent && len(cfg.Forwards) > 0 {
		fwd := &daemon.State{VM: d, Token: token, VsockPort: cfg.VsockPort}
		for _, f := range cfg.Forwards {
			if _, ferr := fwd.AddForward(f.HostPort, f.GuestPort); ferr != nil {
				fmt.Fprintf(os.Stderr, "dew: %v\n", ferr)
				continue
			}
			fmt.Fprintf(os.Stderr, "dew: forwarding 127.0.0.1:%d → guest:%d\n", f.HostPort, f.GuestPort)
		}
	}

	// argv-or-shell decision: 2+ args → exec argv directly (no
	// outer sh -c wrap). Single arg → shell-wrap so users can still
	// pass `dew run "echo a; echo b"`. See argvOrShellWrap.
	execCommand, execArgs := argvOrShellWrap(cmdArgs)
	cmd := strings.Join(cmdArgs, " ") // retained for the serial-fallback path below, which has no argv mode
	var result *RunResult

	if tokenSent {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err == nil {
			if flagStream {
				exitCode, serr := execVsockStreamArgv(conn, token, execCommand, execArgs, budget.guestTimeout())
				conn.Close()
				d.Stop(context.Background())
				hostReader.Close()
				hostWriter.Close()
				if serr != nil {
					if budget.expired() {
						return timeoutErr("exec")
					}
					return fmt.Errorf("exec: %w", serr)
				}
				if exitCode != 0 {
					os.Exit(exitCode)
				}
				return nil
			}
			result, err = execVsockConnArgv(conn, token, execCommand, execArgs, budget.guestTimeout())
			conn.Close()
		}
	}

	if result == nil {
		if budget.expired() {
			d.Stop(context.Background())
			return timeoutErr("agent wait")
		}
		fmt.Fprintf(os.Stderr, "dew: vsock unavailable, using serial\n")
		if err := sExec.WaitReady(budget.window(60 * time.Second)); err != nil {
			d.Stop(context.Background())
			if budget.expired() {
				return timeoutErr("agent wait")
			}
			return fmt.Errorf("guest not ready: %w — the guest may have failed to boot (kernel/initramfs mismatch?); run 'dew doctor' to check assets", err)
		}
		_ = vmstate.Write(stateDir, vmstate.State{
			PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "run",
			Profile: profile, StartedAt: startedAt,
		})
		if t := budget.guestTimeout(); t > 0 {
			sExec.Timeout = t
		}
		output, exitCode, serr := sExec.Run(cmd)
		if serr != nil {
			d.Stop(context.Background())
			if budget.expired() {
				return timeoutErr("exec")
			}
			return fmt.Errorf("exec: %w", serr)
		}
		result = &RunResult{ExitCode: exitCode, Stdout: output}
		err = nil
	}
	if err != nil {
		d.Stop(context.Background())
		if budget.expired() {
			return timeoutErr("exec")
		}
		return fmt.Errorf("exec: %w", err)
	}

	if flagJSON {
		// Under --json, dew exits 0 if dew itself succeeded (VM came up,
		// command dispatched, result captured). The guest's exit code
		// lives inside `data.guest_exit_code` so an agent reading the
		// JSON never has to disambiguate "did dew fail or did the guest
		// fail" from $?. See docs/exit-codes.md.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data": map[string]any{
				"guest_exit_code": result.ExitCode,
				"stdout":          result.Stdout,
				"stderr":          result.Stderr,
			},
		})
		d.Stop(context.Background())
		hostReader.Close()
		hostWriter.Close()
		return nil
	}

	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}
	fmt.Fprintf(os.Stderr, "dew: exit code %d\n", result.ExitCode)
	exitCode := result.ExitCode

	d.Stop(context.Background())
	hostReader.Close()
	hostWriter.Close()

	// Shell mode: pass the guest's exit code through to the host shell
	// so $? carries it. Mirror docker/podman/kubectl exec.
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

type RunResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr,omitempty"`
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// shellQuote wraps s for safe inclusion as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// summariseApkFailure picks a one-line hint out of apk's stderr/stdout
// for display in the spinner. Full output is preserved in the
// --events stream.
func summariseApkFailure(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "apk install failed (no output)"
	}
	switch {
	case strings.Contains(out, "Could not resolve") ||
		strings.Contains(out, "Name or service not known") ||
		strings.Contains(out, "no address associated") ||
		strings.Contains(strings.ToLower(out), "temporary failure in name resolution"):
		return "apk install failed — DNS/network unreachable"
	case strings.Contains(out, "no such file or directory") ||
		strings.Contains(out, "ERROR: unable to select packages"):
		return "apk install failed — package name or repo error"
	case strings.Contains(out, "Permission denied"):
		return "apk install failed — permission denied (unexpected; report this)"
	default:
		// Show the last non-empty line; usually the most useful.
		lines := strings.Split(out, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			l := strings.TrimSpace(lines[i])
			if l != "" {
				if len(l) > 100 {
					l = l[:97] + "..."
				}
				return "apk install failed — " + l
			}
		}
		return "apk install failed"
	}
}

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func execVsockStreamArgv(conn net.Conn, token, command string, args []string, timeout time.Duration) (int, error) {
	req := vsockProto.ExecRequest{
		Type: vsockProto.TypeExec, Token: token, Stream: true,
		Command: command, Args: args,
	}
	if timeout > 0 {
		req.TimeoutMs = int(timeout / time.Millisecond)
	}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return -1, err
	}
	for {
		header := make([]byte, 4)
		if _, err := io.ReadFull(conn, header); err != nil {
			return -1, err
		}
		length := uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
		data := make([]byte, length)
		if _, err := io.ReadFull(conn, data); err != nil {
			return -1, err
		}
		// Try ExecDone first
		var done vsockProto.ExecDone
		if json.Unmarshal(data, &done); done.ExitCode != 0 || done.Error != "" || len(data) < 50 {
			// Check if this is actually a done message
			var check struct {
				Stream string `json:"stream"`
			}
			json.Unmarshal(data, &check)
			if check.Stream == "" {
				if flagEvents {
					event, _ := json.Marshal(map[string]interface{}{"type": "exit", "exit_code": done.ExitCode, "error": done.Error})
					fmt.Println(string(event))
				}
				return done.ExitCode, nil
			}
		}
		var chunk vsockProto.OutputChunk
		json.Unmarshal(data, &chunk)
		if flagEvents {
			event, _ := json.Marshal(map[string]string{"type": chunk.Stream, "data": chunk.Data})
			fmt.Println(string(event))
		} else {
			switch chunk.Stream {
			case "stderr":
				fmt.Fprint(os.Stderr, chunk.Data)
			default:
				fmt.Print(chunk.Data)
			}
		}
	}
}

// stagedService is a --with service resolved and pulled on the host, ready to
// launch in the guest via dew-oci-run.
type stagedService struct {
	name    string
	port    int
	bundle  string // guest path of the staged bundle (/oci-stage/<name>)
	dataArg string // "hostsrc:contdest" for dew-oci-run --data, or ""
}

type serviceFailure struct {
	name       string
	reason     string
	suggestion string
}

// stageServices resolves each --with name, pulls+stages its image into
// stageRoot/<name> on the host (content-addressed cache), and returns the
// staged services plus any failures. A service with a DataDir gets a persistent
// bind mount on the guest's ext4 disk so its data survives restarts.
func stageServices(ctx context.Context, names []string, stageRoot string) ([]stagedService, []serviceFailure) {
	var staged []stagedService
	var failures []serviceFailure
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		svc := services.Lookup(name)
		if svc == nil {
			failures = append(failures, serviceFailure{
				name: name, reason: "unknown service",
				suggestion: "available: " + strings.Join(services.Names(), ", "),
			})
			continue
		}
		var data *ocistage.Bind
		dataArg := ""
		if svc.DataDir != "" {
			src := "/var/lib/dew/services/" + svc.Name + "/data"
			data = &ocistage.Bind{Source: src, Destination: svc.DataDir}
			dataArg = src + ":" + svc.DataDir
		}
		if _, err := ocistage.Stage(ctx, svc.Image, ocistage.Options{
			StageDir: filepath.Join(stageRoot, svc.Name),
			Name:     svc.Name,
			Env:      svc.Env,
			Data:     data,
			Append:   svc.Args,
		}); err != nil {
			failures = append(failures, serviceFailure{name: svc.Name, reason: err.Error()})
			continue
		}
		staged = append(staged, stagedService{
			name: svc.Name, port: svc.Port,
			bundle: "/oci-stage/" + svc.Name, dataArg: dataArg,
		})
	}
	return staged, failures
}

func cmdUp(args []string) error {
	parsedCfg, remaining, err := parseFlags(args)
	if err != nil {
		return err
	}
	dir := "."
	if len(remaining) > 0 {
		dir = remaining[0]
	}

	var proj *detect.Project
	if flagServicesOnly {
		// Services-only: no project, no dev server — just boot a VM and
		// bring up the requested --with services. Lets users run
		// `dew up --services-only --with postgres` for a throwaway DB
		// without inventing a fake package.json. node has the ext4 disk +
		// overlayfs crun needs.
		if flagWith == "" {
			return fmt.Errorf("--services-only requires --with <service> (e.g. dew up --services-only --with postgres)")
		}
		proj = &detect.Project{Profile: "node"}
	} else {
		proj, err = detect.Detect(dir)
		if err != nil {
			return err
		}
		if proj.Framework == "" && proj.Runtime == "" {
			// "Floor = works" — don't punish first contact. Surface multiple
			// exits so beginners + agents have a parseable next step. Error
			// code `no_project_detected` is grep-able for agents. Every
			// suggested command below must work today; never point at planned
			// commands that don't yet exist.
			return fmt.Errorf("no project detected in %s [no_project_detected]\n\nQuick options:\n  • dew up --profile minimal       — boot a minimal Linux VM here\n  • dew vm start --profile minimal — same, returns immediately, use 'dew exec' afterwards\n  • dew app run code               — run an OSS app like VS Code\n\nDocs: https://dewvm.dev/start", dir)
		}
	}

	emit := func(data map[string]interface{}) {
		if flagEvents || flagJSON {
			line, _ := json.Marshal(data)
			fmt.Println(string(line))
		}
	}

	emit(map[string]interface{}{
		"type": "detect", "framework": proj.Framework,
		"pkg_mgr": proj.PackageMgr, "port": proj.Port,
		"dev_cmd": proj.DevCmd, "install_cmd": proj.InstallCmd,
	})

	if !flagJSON && !flagEvents {
		fmt.Fprintf(os.Stderr, "\n  💧 dew up\n\n")
		if flagServicesOnly {
			fmt.Fprintf(os.Stderr, "  services-only: %s\n\n", flagWith)
		} else {
			fmt.Fprintf(os.Stderr, "  detected: %s", proj.Framework)
			if proj.PackageMgr != "" {
				fmt.Fprintf(os.Stderr, " (%s)", proj.PackageMgr)
			}
			fmt.Fprintf(os.Stderr, "\n\n")
		}
	}

	absDir, _ := filepath.Abs(dir)
	flagProfile = proj.Profile
	// --with services run via crun (host-pulled rootfs + overlay), which needs
	// an ext4 disk + overlayfs. Every non-minimal profile has that; only a
	// diskless minimal needs upgrading (to node).
	if flagWith != "" && flagProfile == "minimal" {
		flagProfile = "node"
	}

	// --dry-run: print the resolved plan and exit. No VM boot, no
	// asset download, no install. The plan is the same data we'd
	// otherwise act on, so an agent piping --json | jq can preview
	// exactly what will happen.
	if flagDryRun {
		plan := map[string]interface{}{
			"type":          "dry-run",
			"project_dir":   absDir,
			"framework":     proj.Framework,
			"runtime":       proj.Runtime,
			"package_mgr":   proj.PackageMgr,
			"profile":       flagProfile,
			"install_cmd":   proj.InstallCmd,
			"dev_cmd":       proj.DevCmd,
			"port":          proj.Port,
			"cpus":          parsedCfg.CPUs,
			"memory_mb":     parsedCfg.MemoryMB,
			"with_services": flagWith,
			"would_boot":    false,
		}
		if flagJSON || flagEvents {
			b, _ := json.Marshal(plan)
			fmt.Println(string(b))
		} else {
			fmt.Fprintf(os.Stderr, "  dry run — would do:\n")
			fmt.Fprintf(os.Stderr, "    project: %s (%s)\n", absDir, proj.Framework)
			fmt.Fprintf(os.Stderr, "    profile: %s\n", flagProfile)
			if proj.InstallCmd != "" {
				fmt.Fprintf(os.Stderr, "    install: %s\n", proj.InstallCmd)
			}
			if proj.DevCmd != "" {
				fmt.Fprintf(os.Stderr, "    dev:     %s\n", proj.DevCmd)
			}
			if proj.Port > 0 {
				fmt.Fprintf(os.Stderr, "    port:    %d → host %d\n", proj.Port, proj.Port)
			}
			if flagWith != "" {
				fmt.Fprintf(os.Stderr, "    with:    %s\n", flagWith)
			}
			fmt.Fprintf(os.Stderr, "\n  No VM started. No changes made.\n\n")
		}
		return nil
	}

	cfg := vm.Config{
		CPUs:     parsedCfg.CPUs,
		MemoryMB: parsedCfg.MemoryMB,
		Kernel:   parsedCfg.Kernel,
		Initrd:   parsedCfg.Initrd,
		CmdLine:  "console=hvc0",
		Network:  true,
		// Initial host-port is provisional: it's the framework's
		// default (Vite=5173, Next=3000, etc.). The dev server may
		// bind to a different port (vite.config.ts can override),
		// in which case the launch loop below detects + adds the
		// real forward dynamically. We also shift the host side if
		// preferred is busy — matches grove's anti-collision logic
		// so a stray dev server on the host doesn't break dew up.
		Forwards: []vm.PortForward{provisionalForward(proj.Port)},
		SharedDirs: []vm.SharedDir{
			{Tag: "project", HostPath: absDir, ReadOnly: false},
		},
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 512
	}
	if err := resolveAssets(&cfg); err != nil {
		return err
	}
	cfg.VsockPort = uint32(vsockProto.DefaultPort)
	cfg.CmdLine += " dew.share=project:/app"

	// --reset-disk: delete the persistent disk image before boot so it's
	// rebuilt fresh from the current initramfs. The one-command recovery
	// for a stale/corrupt disk image left by a previous version (which
	// otherwise guest-panics at switch_root with "Exec format error").
	if flagResetDisk && cfg.DiskPath != "" {
		if err := os.Remove(cfg.DiskPath); err == nil {
			emit(map[string]interface{}{"type": "disk", "status": "reset", "path": cfg.DiskPath})
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "  reset disk: %s\n", cfg.DiskPath)
			}
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "dew: could not reset disk %s: %v\n", cfg.DiskPath, err)
		}
	}

	// In machine-readable modes the serial console must not share stdout
	// with the NDJSON lifecycle stream — interleaved kernel/boot lines
	// (EXT4-fs, udhcpc, "Bridge firewalling registered") break `| jq`.
	// Route the console to stderr so stdout carries structured events
	// only (matches the convention dew vm start already uses).
	if (flagJSON || flagEvents) && cfg.Console == nil {
		cfg.Console = &vm.ConsoleFiles{In: os.Stdin, Out: os.Stderr}
	}

	token := generateToken()

	var spin *progress.Spinner
	if !flagJSON && !flagEvents {
		spin = progress.New()
	}

	// Stage --with service images on the host before boot, so the virtiofs
	// share holding their rootfs bundles exists when the guest comes up. crun
	// (baked into the initramfs) runs them in the guest — no containerd.
	var stagedSvcs []stagedService
	var svcFailures []serviceFailure
	if flagWith != "" {
		stageRoot := filepath.Join(dewDataDir(), "oci-stage", strconv.Itoa(os.Getpid()))
		os.RemoveAll(stageRoot)
		defer os.RemoveAll(stageRoot)
		if spin != nil {
			spin.Step("pulling service images")
		}
		stagedSvcs, svcFailures = stageServices(context.Background(), strings.Split(flagWith, ","), stageRoot)
		if len(stagedSvcs) > 0 {
			cfg.SharedDirs = append(cfg.SharedDirs, vm.SharedDir{Tag: "oci-stage", HostPath: stageRoot, ReadOnly: true})
			cfg.CmdLine += " dew.share=oci-stage:/oci-stage"
		}
	}

	// Remove stale socket
	os.Remove(daemon.SocketPath(flagVMName))

	d, err := darwin.New(cfg)
	if err != nil {
		return err
	}

	emit(map[string]interface{}{"type": "boot", "status": "starting"})
	if spin != nil {
		spin.Step("booting")
	}
	start := time.Now()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	stateDir := vmstate.DirFor(daemon.SocketDir(), flagVMName)
	upStartedAt := time.Now().UTC()
	_ = vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseBooting, Mode: "up",
		Profile: cfgProfileName(), StartedAt: upStartedAt,
	})
	defer clearVMState(flagVMName, os.Getpid())

	if err := d.Start(ctx); err != nil {
		emit(map[string]interface{}{"type": "boot", "status": "failed", "error": err.Error()})
		// VZ Code=2 ("storage device attachment is invalid") means the
		// existing disk image is unusable. The darwin layer already prints
		// the rm recovery; also point at the one-command rebuild.
		if !flagResetDisk && cfg.DiskPath != "" &&
			strings.Contains(err.Error(), "storage device attachment is invalid") {
			return fmt.Errorf("%w\n\n  Rebuild the disk in one step: dew up --reset-disk", err)
		}
		return err
	}
	bootMs := time.Since(start).Milliseconds()
	emit(map[string]interface{}{"type": "boot", "status": "ready", "elapsed_ms": bootMs})

	// Wait for agent + inject token. Wall-clock deadline, not attempt
	// count — see the cmdRun agent wait for why.
	tokenSent := false
	tokenDeadline := time.Now().Add(30 * time.Second)
	for {
		if err := sendToken(d, cfg.VsockPort, token); err == nil {
			tokenSent = true
			break
		}
		if time.Now().After(tokenDeadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !tokenSent {
		ev := map[string]interface{}{"type": "agent", "status": "failed", "error": "token handshake timeout"}
		if h := staleDiskHint(cfg.DiskPath, "dew up --reset-disk"); h != "" {
			ev["hint"] = h
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "\n  %s\n", h)
			}
		}
		emit(ev)
		if spin != nil {
			spin.Fail("agent not ready")
		}
	}

	dmn := &daemon.State{
		VM: d, Token: token, VsockPort: cfg.VsockPort,
		SocketPath: daemon.SocketPath(flagVMName),
	}
	dmn.Start()
	_ = vmstate.Write(stateDir, vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "up",
		Profile: cfgProfileName(), StartedAt: upStartedAt,
	})
	for _, f := range cfg.Forwards {
		if _, err := dmn.AddForward(f.HostPort, f.GuestPort); err != nil {
			fmt.Fprintf(os.Stderr, "dew: %v\n", err)
		}
	}

	execInVMTimeout := func(cmd string, timeout time.Duration) (*RunResult, error) {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return execVsockConnTimeout(conn, token, cmd, timeout)
	}
	execInVM := func(cmd string) (*RunResult, error) {
		return execInVMTimeout(cmd, 0)
	}

	// Project is mounted via virtiofs at /app (live sync from host).
	// node_modules is redirected away from virtiofs to a path that's
	// (a) Linux+musl ABI-compatible (the host might be macOS), and
	// (b) not on virtiofs, where `rm -rf` is unreliable.
	//
	// Two destinations are possible:
	//   - With a lockfile: /var/cache/dew/nm/{key}/node_modules on the
	//     persistent node-profile disk, keyed by project path. Survives
	//     `dew down`, so a subsequent `dew up` skips npm install
	//     entirely if the lockfile hasn't changed (see internal/nmcache).
	//   - Without a lockfile: /tmp/nm on tmpfs. Disappears with the VM.
	//     We can't safely cache without a stable input hash.
	if proj.InstallCmd != "" {
		// Decide cache strategy by looking for a supported lockfile.
		// ErrNoLockfile → tmpfs fallback (matches pre-cache behavior).
		// Anything else (read error, etc.) is non-fatal: log and fall
		// back to tmpfs.
		cacheKey := nmcache.ProjectKey(dir)
		wantStamp, stampErr := nmcache.ComputeStamp(dir)
		cacheable := stampErr == nil
		cacheHit := false

		if cacheable {
			// Setup script bind-mounts the persistent path and tells
			// us whether the existing stamp matches the want stamp.
			// Crash recovery (wiping a half-installed tree from a
			// previous failed install) also happens here, inside the
			// script — see internal/nmcache/cache.sh.
			setupCmd := nmcache.SetupCommand(cacheKey, wantStamp.Marshal())
			res, err := execInVMTimeout(setupCmd, 30*time.Second)
			switch {
			case err != nil:
				emit(map[string]interface{}{
					"type": "cache", "status": "setup-failed",
					"error": err.Error(),
				})
				cacheable = false
			case res != nil && res.ExitCode != 0:
				emit(map[string]interface{}{
					"type": "cache", "status": "setup-failed",
					"error": res.Stderr,
				})
				cacheable = false
			case res != nil && strings.Contains(res.Stdout, "DEW_NM_CACHE=hit"):
				cacheHit = true
				emit(map[string]interface{}{
					"type": "cache", "status": "hit",
					"lockfile": wantStamp.Lockfile,
				})
				if spin != nil {
					spin.Step("node_modules cached")
				}
			default:
				emit(map[string]interface{}{
					"type": "cache", "status": "miss",
					"lockfile": wantStamp.Lockfile,
				})
			}
		}

		// Pre-flight: does this project's lockfile (or package.json,
		// pre-install) reference a package that compiles native code?
		// If so, install build-base + python3 upfront so npm install
		// doesn't fail mid-flight. If we miss here, the reactive
		// fallback below catches it from npm's stderr.
		runInstall := func() (*RunResult, error, int64) {
			t := time.Now()
			// `npm install` on a fresh Vite + React tree runs ~15-60 s;
			// the agent's default 30 s timeout was killing it mid-fetch
			// and surfacing as "install failed" with an empty error
			// string. Give project installs up to 10 minutes — they're
			// long-running but bounded, and the user can Ctrl+C if
			// truly stuck.
			//
			// When the cache layer has already bind-mounted
			// /app/node_modules to a persistent path, the mount step
			// here is a no-op (mountpoint -q skips it). When it
			// hasn't (no lockfile case), this falls back to tmpfs.
			res, err := execInVMTimeout(
				"mkdir -p /tmp/nm /app/node_modules && "+
					"mountpoint -q /app/node_modules || mount --bind /tmp/nm /app/node_modules && "+
					"cd /app && "+proj.InstallCmd,
				10*time.Minute)
			return res, err, time.Since(t).Milliseconds()
		}
		installBuildTools := func(reason string) (success bool) {
			emit(map[string]interface{}{"type": "build-tools", "status": "starting", "reason": reason})
			if spin != nil {
				spin.Step(fmt.Sprintf("installing build tools (%s)", reason))
			}
			t := time.Now()
			// build-base = gcc + g++ + make + musl-dev + binutils; python3
			// is required by node-gyp. apk install over the network on
			// musl is typically 25-40 s on aarch64, 20-35 s on x86_64.
			res, execErr := execInVMTimeout(
				"apk update 2>&1 && apk add --no-cache build-base python3 2>&1",
				5*time.Minute)
			elapsed := time.Since(t).Milliseconds()
			failed := execErr != nil || (res != nil && res.ExitCode != 0)
			if failed {
				stderr := ""
				if res != nil {
					stderr = res.Stderr
					if stderr == "" {
						stderr = res.Stdout
					}
				} else if execErr != nil {
					stderr = execErr.Error()
				}
				emit(map[string]interface{}{
					"type": "build-tools", "status": "failed",
					"reason": reason, "elapsed_ms": elapsed, "error": stderr,
				})
				if spin != nil {
					// Surface a one-line hint; full stderr stays in the
					// --events stream / JSON output.
					hint := summariseApkFailure(stderr)
					spin.Fail(hint)
				}
				return false
			}
			emit(map[string]interface{}{
				"type": "build-tools", "status": "done",
				"reason": reason, "elapsed_ms": elapsed,
			})
			return true
		}

		// Pre-flight install if the lockfile names anything known to need
		// node-gyp. We treat a build-tools install failure as fatal-ish:
		// surface it, but still try `npm install` — sharp et al. usually
		// have prebuilt binaries, so the install may still succeed
		// (network was just down for apk briefly).
		var buildToolsTried bool
		if needs, matched := detect.NeedsNativeBuildTools(dir); needs {
			buildToolsTried = true
			installBuildTools(strings.Join(matched, ", "))
		}

		// Cache hit: install was committed on a previous boot, skip
		// the package-manager step entirely. The bind-mount has
		// already been set up by the setup script above.
		if cacheHit {
			emit(map[string]interface{}{"type": "install", "status": "cached"})
			// Skip past the rest of the install block to whatever
			// runs after (services, app start). The closing brace
			// further down handles flow.
		} else {

			emit(map[string]interface{}{"type": "install", "status": "starting", "cmd": proj.InstallCmd})
			if spin != nil {
				spin.Step("installing deps")
			}
			result, err, installMs := runInstall()

			// Reactive fallback: if the first install failed with what looks
			// like a missing-toolchain error AND we haven't tried installing
			// the toolchain yet, install build-base + python3 and retry once.
			// Catches projects where the native dep is transitive or uses an
			// unlisted package name.
			if !buildToolsTried &&
				err == nil && result != nil && result.ExitCode != 0 &&
				detect.ScanInstallStderrForNativeBuild(result.Stderr) {
				if installBuildTools("npm install needs native compile") {
					if spin != nil {
						spin.Step("installing deps (retry)")
					}
					result, err, installMs = runInstall()
				}
			}

			if err != nil || (result != nil && result.ExitCode != 0) {
				errMsg := ""
				suggestion := ""
				if result != nil {
					errMsg = result.Stderr
					switch {
					case strings.Contains(errMsg, "peer dep") || strings.Contains(errMsg, "ERESOLVE"):
						suggestion = "try adding --legacy-peer-deps to install command"
					case detect.ScanInstallStderrForNativeBuild(errMsg):
						if buildToolsTried {
							suggestion = "build tools installed but compile still failed; check stderr above"
						} else {
							suggestion = "looks like a missing-toolchain failure dew didn't catch; please file an issue with the package name"
						}
					}
				}
				emit(map[string]interface{}{
					"type": "install", "status": "failed",
					"elapsed_ms": installMs, "error": errMsg, "suggestion": suggestion,
				})
				if spin != nil {
					if suggestion != "" {
						spin.Fail(suggestion)
					} else {
						spin.Fail("install failed")
					}
				}
			} else {
				emit(map[string]interface{}{"type": "install", "status": "done", "elapsed_ms": installMs})
				// Install succeeded; atomically commit the cache stamp.
				// Failure here is non-fatal — the cache just stays in
				// "in-progress" state, and the next boot will rebuild
				// (crash-recovery path).
				if cacheable {
					if _, cerr := execInVMTimeout(nmcache.CommitCommand(cacheKey), 10*time.Second); cerr != nil {
						emit(map[string]interface{}{
							"type": "cache", "status": "commit-failed",
							"error": cerr.Error(),
						})
					} else {
						emit(map[string]interface{}{
							"type": "cache", "status": "committed",
						})
					}
				}
			}
		} // end of else: non-cache-hit install
	}

	// Start services (--with postgres,redis). Images were staged on the host
	// before boot (shared at /oci-stage); launch each via crun (dew-oci-run).
	for _, f := range svcFailures {
		emit(map[string]interface{}{
			"type": "service", "status": "failed", "name": f.name,
			"error": f.reason, "suggestion": f.suggestion,
		})
		if spin != nil {
			spin.Fail(fmt.Sprintf("%s: %s", f.name, f.reason))
		}
	}
	for _, s := range stagedSvcs {
		emit(map[string]interface{}{"type": "service", "status": "starting", "name": s.name, "port": s.port})
		if spin != nil {
			spin.Step(fmt.Sprintf("%s (port %d)", s.name, s.port))
		}
		runCmd := "dew-oci-run --detach"
		if s.dataArg != "" {
			runCmd += " --data " + s.dataArg
		}
		runCmd += " " + s.bundle + " " + s.name
		// dew-oci-run exits non-zero (and logs to stderr) if crun didn't come
		// up, so don't claim "started" — and don't forward a dead port.
		res, rerr := execInVM(runCmd)
		if rerr != nil || (res != nil && res.ExitCode != 0) {
			reason := "container failed to start"
			if rerr != nil {
				reason = rerr.Error()
			} else if res != nil && strings.TrimSpace(res.Stderr) != "" {
				reason = strings.TrimSpace(res.Stderr)
			}
			ev := map[string]interface{}{"type": "service", "status": "failed", "name": s.name, "error": reason}
			appendServiceDiag(ev, execInVM, s.name)
			emit(ev)
			if spin != nil {
				spin.Fail(fmt.Sprintf("%s: %s", s.name, reason))
			}
			continue
		}

		// Add the port forward. AddForward falls back to a free host port
		// when the requested one is busy (e.g. a local postgres already on
		// 5432), so capture the ACTUAL bound port — otherwise the started
		// event would advertise a port nothing is listening on.
		hostFwd := s.port
		if addr, err := dmn.AddForward(s.port, s.port); err != nil {
			fmt.Fprintf(os.Stderr, "dew: forward %d: %v\n", s.port, err)
		} else if _, p, e := net.SplitHostPort(addr); e == nil {
			if n, e2 := strconv.Atoi(p); e2 == nil {
				hostFwd = n
			}
		}
		cfg.Forwards = append(cfg.Forwards, vm.PortForward{HostPort: hostFwd, GuestPort: s.port})
		if hostFwd != s.port && !flagJSON && !flagEvents {
			fmt.Fprintf(os.Stderr, "  %s: host :%d busy → forwarding :%d\n", s.name, s.port, hostFwd)
		}

		// Health gate: `dew-oci-run` only confirms the crun process is
		// "running", not that the service bound its port. Poll the guest's
		// IPv4 LISTEN socket and only report "started" once it truly
		// accepts connections; otherwise report "failed" with the captured
		// log so a service that came up then died doesn't masquerade as
		// ready (the recurring "started but the port is dead" report).
		ready := waitGuestReady(func() bool {
			pr, perr := execInVMTimeout(services.ListenProbeCmd(s.port), 5*time.Second)
			return perr == nil && pr != nil && pr.ExitCode == 0
		}, 30, time.Second)
		if !ready {
			ev := map[string]interface{}{
				"type": "service", "status": "failed", "name": s.name,
				"port":  s.port,
				"error": "service did not start accepting connections within 30s",
			}
			appendServiceDiag(ev, execInVM, s.name)
			emit(ev)
			if spin != nil {
				spin.Fail(fmt.Sprintf("%s never became ready", s.name))
			}
			continue
		}

		// Report the actual host port and a ready-to-use connection string
		// so agents don't have to reconstruct credentials.
		startedEv := map[string]interface{}{
			"type": "service", "status": "started", "name": s.name,
			"port": s.port, "host_port": hostFwd,
		}
		if svc := services.Lookup(s.name); svc != nil {
			startedEv["conn"] = services.ConnString(*svc, hostFwd)
		}
		emit(startedEv)
	}

	// Services-only: there's no dev server to start or port to probe.
	// The services above were already health-gated, so emit a top-level
	// ready (independent of any dev-server port) and wait for ^C. This
	// also fixes the "ready never fires without a bound dev port" gap for
	// the no-dev-server case.
	if flagServicesOnly {
		emit(map[string]interface{}{
			"type": "ready", "mode": "services-only",
			"services":   strings.Split(flagWith, ","),
			"elapsed_ms": time.Since(start).Milliseconds(),
		})
		if !flagJSON && !flagEvents {
			if spin != nil {
				spin.Done("services ready")
			}
			fmt.Fprintf(os.Stderr, "  Ctrl+C to stop\n")
		}
		<-ctx.Done()
		dmn.Stop()
		fmt.Fprintf(os.Stderr, "\n  stopping...\n")
		return d.Stop(context.Background())
	}

	// Start dev server. The earlier "cmd &" launched vite inside a shell
	// that immediately exited; the dev server then either received SIGHUP
	// or noticed its stdin/stdout pipes had been closed, and quit a few
	// seconds later. setsid + redirected stdio detaches it fully and
	// keeps the log in the VM for `dew exec` to inspect.
	emit(map[string]interface{}{"type": "serve", "status": "starting", "cmd": proj.DevCmd})
	if spin != nil {
		spin.Step("dev server")
	}
	execInVM("cd /app && setsid sh -c " + shellQuote(proj.DevCmd) + " </dev/null >>/tmp/dew-dev.log 2>&1 &")
	healthy := false
	// HostPort and GuestPort start at proj.Port (framework default).
	// On detection of the dev server's actual announced port we
	// dynamically add a new forward and switch the URL we probe.
	// Without this, a Vite app whose vite.config.ts sets port=3000
	// would have us forward 5173 forever and the agent could never
	// reach the dev server.
	hostPort := cfg.Forwards[0].HostPort
	guestPort := proj.Port
	url := fmt.Sprintf("http://localhost:%d/", hostPort)
	detected := false
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)

		// Re-detect the real port until we've found one different
		// from the provisional guess; then stop scanning the log to
		// avoid pointless exec calls every tick.
		if !detected {
			if p := readDetectedDevPort(d, token, cfg.VsockPort); p > 0 && p != guestPort {
				freeHost, _, perr := pickFreeHostPort(p, 50)
				if perr == nil {
					if _, err := dmn.AddForward(freeHost, p); err == nil {
						hostPort = freeHost
						guestPort = p
						url = fmt.Sprintf("http://localhost:%d/", hostPort)
						emit(map[string]interface{}{
							"type":                "port-redetected",
							"detected_guest_port": p, "host_port": freeHost,
							"replaces_initial": proj.Port,
						})
						if !flagJSON && !flagEvents {
							fmt.Fprintf(os.Stderr, "  detected actual dev port: %d → %s\n", p, url)
						}
					}
				}
				detected = true
			}
		}

		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			healthy = true
			break
		}
	}
	// Final URL printed downstream reflects whatever the loop
	// converged on (provisional or detected).

	totalElapsed := time.Since(start)
	totalMs := totalElapsed.Milliseconds()
	if healthy {
		// `ready` is the canonical signal an agent should grep for.
		// Always emit it in machine-readable modes — agents shouldn't
		// have to scrape "http://" out of human progress lines (which
		// can collide with kernel dmesg noise like the X.509 cert log).
		readyEvent := map[string]interface{}{
			"type": "ready", "url": url, "port": hostPort,
			"guest_port": guestPort,
			"framework":  proj.Framework, "elapsed_ms": totalMs,
		}
		// emit() fires in either --events or --json mode (see the
		// emit closure above). So one call is enough; no double-print.
		emit(readyEvent)
		// Keep the legacy health event for tools wired to the old name.
		emit(map[string]interface{}{
			"type": "health", "status": "ok",
			"url": url, "elapsed_ms": totalMs,
		})
		if !flagJSON && !flagEvents && spin != nil {
			spin.Done(url)
			fmt.Fprintf(os.Stderr, "  Ctrl+C to stop\n")
		}
	} else {
		// Same shape as ready, but with type=timeout — lets an agent
		// distinguish "URL is up" from "URL was tried, server didn't
		// answer in time" with a single grep.
		timeoutEvent := map[string]interface{}{
			"type": "timeout", "url": url, "port": hostPort,
			"guest_port": guestPort,
			"framework":  proj.Framework, "elapsed_ms": totalMs,
			"hint": "server may still be starting, try opening the URL manually",
		}
		emit(timeoutEvent)
		emit(map[string]interface{}{
			"type": "health", "status": "timeout",
			"url": url, "elapsed_ms": totalMs,
		})
		if !flagJSON && !flagEvents && spin != nil {
			spin.Timeout(url)
			fmt.Fprintf(os.Stderr, "  Ctrl+C to stop\n")
		}
	}

	<-ctx.Done()

	dmn.Stop()
	fmt.Fprintf(os.Stderr, "\n  stopping...\n")
	return d.Stop(context.Background())
}

func cmdDown(args []string) error {
	args, err := popNameFlag(args)
	if err != nil {
		return err
	}
	for _, a := range args {
		if a == "--json" {
			flagJSON = true
		}
	}
	sockPath := daemon.SocketPath(flagVMName)
	if _, err := os.Stat(sockPath); err != nil {
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.Encode(map[string]interface{}{"status": "not_running"})
		} else {
			fmt.Fprintf(os.Stderr, "dew: no running VM\n")
		}
		return nil
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		os.Remove(sockPath)
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.Encode(map[string]interface{}{"status": "stopped", "note": "stale socket removed"})
		} else {
			fmt.Fprintf(os.Stderr, "dew: removed stale socket\n")
		}
		return nil
	}
	conn.Close()

	// Send a signal to stop — the daemon's VM process handles cleanup
	// Find the dew process holding the socket and send SIGTERM
	out, err := exec.Command("lsof", "-t", sockPath).Output()
	if err == nil {
		pid := strings.TrimSpace(string(out))
		if pid != "" {
			exec.Command("kill", pid).Run()
		}
	}
	os.Remove(sockPath)

	// The VM process is killed (SIGTERM) and doesn't run its own
	// deferred cleanup, so stop drops the lifecycle record here — and,
	// for a named VM, its now-empty <name>/ state dir. Best-effort:
	// os.Remove leaves the shared default state root untouched.
	stateDir := vmstate.DirFor(daemon.SocketDir(), flagVMName)
	os.Remove(vmstate.Path(stateDir))
	if flagVMName != "" {
		os.Remove(stateDir)
	}

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(map[string]interface{}{"status": "stopped"})
	} else {
		fmt.Fprintf(os.Stderr, "dew: stopped\n")
	}
	return nil
}

func cmdExec(args []string) error {
	args, err := popNameFlag(args)
	if err != nil {
		return err
	}
	// Strip --json so it isn't joined into the guest command.
	// Also consume --timeout DURATION so callers can override the
	// guest agent's 30s default. Without this, a `dew exec nerdctl
	// pull <big-image>` against an 800 MB image gets cut off mid-
	// download with an unhelpful "timeout after 30s" stderr line.
	wantJSON := flagJSON
	var timeoutMs int
	var interactive bool
	var tty bool
	filtered := args[:0]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			wantJSON = true
			flagJSON = true
			continue
		}
		if a == "--interactive" || a == "-i" {
			interactive = true
			continue
		}
		if a == "--tty" || a == "-t" {
			tty = true
			continue
		}
		if a == "-it" || a == "-ti" {
			interactive, tty = true, true
			continue
		}
		if a == "--timeout" {
			i++
			if i >= len(args) {
				return dewerr.New(dewerr.CodeUsage, "--timeout requires a duration (e.g. 5m, 300s)")
			}
			d, perr := time.ParseDuration(args[i])
			if perr != nil {
				return dewerr.Newf(dewerr.CodeUsage, "--timeout: invalid duration %q: %v", args[i], perr)
			}
			timeoutMs = int(d / time.Millisecond)
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage, "usage: dew exec [-it] [--timeout DUR] <cmd...>")
	}
	if interactive || tty {
		return runExecStreaming(args, timeoutMs, tty)
	}
	return runExecRequest(args, wantJSON, timeoutMs)
}

// runExecRequest sends an exec request to the running VM's daemon and
// renders the response. Split out of cmdExec so both the legacy flag
// scanner and the cobra exec command (which parses --json/--timeout
// natively) share one body. args is the guest command (argv form when
// len>=2, shell string when len==1); wantJSON selects the envelope.
func runExecRequest(args []string, wantJSON bool, timeoutMs int) error {
	sockPath := daemon.SocketPath(flagVMName)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return dewerr.Wrapf(err, dewerr.CodeConflict, "no running VM (socket %s)", sockPath)
	}
	defer conn.Close()

	// argv-or-shell: see argvOrShellWrap in cmdRun. 2+ args → argv
	// path (no double sh -c wrap); 1 arg → legacy shell string.
	var req daemon.ExecRequest
	if len(args) >= 2 {
		req = daemon.ExecRequest{Argv: args}
	} else {
		req = daemon.ExecRequest{Command: args[0]}
	}
	req.TimeoutMs = timeoutMs
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return dewerr.Wrap(err, dewerr.CodeNetwork, "send")
	}

	var resp vsockProto.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return dewerr.Wrap(err, dewerr.CodeNetwork, "recv")
	}

	if wantJSON {
		// JSON mode: dew exits 0 if dew itself succeeded; the guest's
		// exit code lives in data.guest_exit_code. See cmdRun.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             resp.Error == "",
			"schema_version": schemaVersion,
			"data": map[string]any{
				"guest_exit_code": resp.ExitCode,
				"stdout":          resp.Stdout,
				"stderr":          resp.Stderr,
			},
		})
		if resp.Error != "" {
			// dew-side problem, not guest exit — surface as an error.
			return dewerr.New(dewerr.CodeGeneric, resp.Error)
		}
		return nil
	}

	if resp.Stdout != "" {
		fmt.Print(resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.Error != "" {
		return dewerr.New(dewerr.CodeGeneric, resp.Error)
	}
	// Shell mode: passthrough guest exit (mirror docker exec / kubectl exec).
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

// runExecStreaming runs an interactive exec: it streams the guest's
// stdout/stderr back live AND forwards this process's stdin into the
// guest, so `dew exec -i ... -- /bin/sh` is a usable session and
// `echo cmd | dew exec -i ...` pipes input through. Mirrors the
// non-streaming guest-exit-code passthrough (os.Exit) so callers — a
// shell, an SSH front door — see the guest's real exit status.
func runExecStreaming(args []string, timeoutMs int, tty bool) error {
	sockPath := daemon.SocketPath(flagVMName)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return dewerr.Wrapf(err, dewerr.CodeConflict, "no running VM (socket %s)", sockPath)
	}
	defer conn.Close()

	var req daemon.ExecRequest
	if len(args) >= 2 {
		req = daemon.ExecRequest{Argv: args}
	} else {
		req = daemon.ExecRequest{Command: args[0]}
	}
	req.Stream = true
	req.Stdin = true
	req.TimeoutMs = timeoutMs

	// TTY mode: request a guest pty, send the initial window size, and put
	// our own terminal in raw mode so keystrokes pass through untouched.
	// Terminal bytes are binary → base64 on the wire (see protocol.go).
	stdinFd := int(os.Stdin.Fd())
	var oldState *term.State
	if tty && term.IsTerminal(stdinFd) {
		req.TTY = true
		if w, h, e := term.GetSize(stdinFd); e == nil {
			req.Cols, req.Rows = uint16(w), uint16(h)
		}
		if st, e := term.MakeRaw(stdinFd); e == nil {
			oldState = st
		}
	} else if tty {
		req.TTY = true // -t without a real terminal: still allocate a pty
	}
	restore := func() {
		if oldState != nil {
			_ = term.Restore(stdinFd, oldState)
			oldState = nil
		}
	}
	defer restore()

	enc := func(b []byte) string {
		if req.TTY {
			return base64.StdEncoding.EncodeToString(b)
		}
		return string(b)
	}
	dec := func(s string) []byte {
		if req.TTY {
			b, _ := base64.StdEncoding.DecodeString(s)
			return b
		}
		return []byte(s)
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return dewerr.Wrap(err, dewerr.CodeNetwork, "send")
	}

	// One serialized writer for all host→guest frames (stdin + resizes).
	var sendMu sync.Mutex
	jenc := json.NewEncoder(conn)
	send := func(v any) error { sendMu.Lock(); defer sendMu.Unlock(); return jenc.Encode(v) }

	// Window-resize → InputChunk{Winch}.
	if req.TTY {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				if w, h, e := term.GetSize(stdinFd); e == nil {
					_ = send(vsockProto.InputChunk{Winch: true, Rows: uint16(h), Cols: uint16(w)})
				}
			}
		}()
	}

	// Forward our stdin until EOF.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if send(vsockProto.InputChunk{Data: enc(buf[:n])}) != nil {
					return
				}
			}
			if rerr != nil {
				_ = send(vsockProto.InputChunk{EOF: true})
				return
			}
		}
	}()

	// Relay output frames (one JSON object per line) until ExecDone.
	r := bufio.NewReader(conn)
	exitCode := 0
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			var chunk vsockProto.OutputChunk
			if json.Unmarshal(line, &chunk) == nil && chunk.Stream != "" {
				if chunk.Stream == "stderr" && !req.TTY {
					os.Stderr.Write(dec(chunk.Data))
				} else {
					os.Stdout.Write(dec(chunk.Data))
				}
			} else {
				var done vsockProto.ExecDone
				_ = json.Unmarshal(line, &done)
				exitCode = done.ExitCode
				if done.Error != "" {
					restore()
					fmt.Fprintf(os.Stderr, "dew: %s\n", done.Error)
				}
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	// Passthrough guest exit (mirror docker exec / kubectl exec). Restore
	// the terminal first — os.Exit skips deferred restore.
	if exitCode != 0 {
		restore()
		os.Exit(exitCode)
	}
	return nil
}

// argvOrShellWrap turns a slice of CLI args into the (command, args)
// pair we send to the guest agent.
//
//   - len >= 2: direct argv. argv[0] is the program, argv[1:] its
//     args, no shell wrapping. `dew run -- sh -c 'echo a; echo b'`
//     becomes /bin/sh -c "echo a; echo b" exactly — no outer shell.
//   - len == 1: legacy shell wrap. Single-arg input like
//     `dew run "echo a; echo b"` is treated as a shell string and
//     wrapped in /bin/sh -c.
//   - len == 0: returns empty pair; caller validates.
//
// The rule comes from the principle that a user who has already
// structured their input as argv knows they're not going through a
// shell; the only time we should wrap is when there's a single string
// that the user implicitly expects shell parsing on.
func argvOrShellWrap(cliArgs []string) (command string, args []string) {
	if len(cliArgs) == 0 {
		return "", nil
	}
	if len(cliArgs) == 1 {
		return "/bin/sh", []string{"-c", cliArgs[0]}
	}
	return cliArgs[0], cliArgs[1:]
}

func execVsockConn(conn net.Conn, token string, cmd string) (*RunResult, error) {
	return execVsockConnTimeout(conn, token, cmd, 0)
}

// execVsockConnTimeout sends a per-call timeout to the guest agent so
// long-running commands (`npm install`, `pip install`, the dependency
// install path in `dew up`) don't trip the agent's default 30 s exec
// timeout. Pass 0 to use the agent default. The cmd is shell-wrapped;
// for argv-direct exec see execVsockConnArgv.
func execVsockConnTimeout(conn net.Conn, token string, cmd string, timeout time.Duration) (*RunResult, error) {
	return execVsockExec(conn, token, "/bin/sh", []string{"-c", cmd}, timeout)
}

// execVsockConnArgv runs a program directly in the guest without an
// implicit /bin/sh -c wrap. Used when the host has CLI argv structured
// by the user (dew run -- sh -c '...'). See argvOrShellWrap for the
// argv-vs-shell decision.
func execVsockConnArgv(conn net.Conn, token, command string, args []string, timeout time.Duration) (*RunResult, error) {
	return execVsockExec(conn, token, command, args, timeout)
}

func execVsockExec(conn net.Conn, token, command string, args []string, timeout time.Duration) (*RunResult, error) {
	req := vsockProto.ExecRequest{Token: token, Command: command, Args: args}
	if timeout > 0 {
		req.TimeoutMs = int(timeout / time.Millisecond)
	}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return nil, err
	}
	// The agent enforces the exec timeout guest-side (default 30s) and
	// always replies, so the host bound below only fires when the agent
	// or transport died mid-exec: guest budget plus grace, never a
	// blocking read with no way out.
	var resp vsockProto.ExecResponse
	if err := vsockProto.ReadJSONTimeout(conn, &resp, hostReadBudget(timeout)); err != nil {
		return nil, err
	}
	return &RunResult{
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}, nil
}

// hostReadGrace is how much longer than the guest's exec budget the
// host waits for the response before declaring the agent dead. A var,
// not a const, so tests can shrink it.
var hostReadGrace = 15 * time.Second

// hostReadBudget converts a guest exec timeout into the host-side
// read deadline for the response: the guest's budget (agent default
// 30s when unset) plus grace for scheduling and transport.
func hostReadBudget(guestTimeout time.Duration) time.Duration {
	if guestTimeout <= 0 {
		guestTimeout = 30 * time.Second
	}
	return guestTimeout + hostReadGrace
}

func sendToken(v vm.VM, port uint32, token string) error {
	conn, err := connectVsock(v, port)
	if err != nil {
		return fmt.Errorf("vsock connect for token: %w", err)
	}
	defer conn.Close()
	req := vsockProto.SetTokenRequest{Type: vsockProto.TypeSetToken, Token: token}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return fmt.Errorf("send token: %w", err)
	}
	var resp vsockProto.ConnectResponse
	if err := vsockProto.ReadJSONTimeout(conn, &resp, 5*time.Second); err != nil {
		return fmt.Errorf("token response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("token rejected: %s", resp.Error)
	}
	return nil
}

func connectVsock(v vm.VM, port uint32) (net.Conn, error) {
	return connectVsockDeadline(v, port, 5*time.Second)
}

// connectVsockDeadline retries vsock connects until the wall-clock
// deadline. The bound is total elapsed time, NOT attempt count — the
// previous 500×10ms loop assumed each attempt fails fast, but a vz
// connect against a guest with no vsock transport blocks (the
// completion handler never fires), which turned "5 seconds" into
// "forever" and `dew run` into a process that hangs until killed.
func connectVsockDeadline(v vm.VM, port uint32, total time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(total)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = fmt.Errorf("no attempt completed")
			}
			return nil, fmt.Errorf("vsock connect: guest agent not reachable within %s: %w", total, lastErr)
		}
		conn, err := vsockConnectAttempt(v, port, remaining)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
}

// vsockConnectAttempt bounds a single VsockConnect call. The darwin
// backend bounds its own connects too; this layer exists so the CLI's
// deadlines hold for any vm.VM implementation (and so the behavior is
// unit-testable with a fake VM, no Virtualization.framework needed).
func vsockConnectAttempt(v vm.VM, port uint32, timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := v.VsockConnect(port)
		ch <- result{conn, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.conn, nil
	case <-timer.C:
		go func() {
			// Close only on success: a failed dial may carry a
			// typed-nil conn whose Close() would panic.
			if r := <-ch; r.err == nil && r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("vsock connect: no response within %s", timeout)
	}
}

func parseForward(s string) (vm.PortForward, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return vm.PortForward{}, fmt.Errorf("--forward: expected hostPort:guestPort, got %q", s)
	}
	host, err1 := strconv.Atoi(parts[0])
	guest, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || host < 1 || guest < 1 {
		return vm.PortForward{}, fmt.Errorf("--forward: invalid ports %q", s)
	}
	return vm.PortForward{HostPort: host, GuestPort: guest}, nil
}

// parseVolume parses a -v/--volume value into a guest source path and an
// in-container destination for the `dew run --image` bind mount. Two forms:
//
//	name:/path      named persistent volume at /var/lib/dew/volumes/<name>
//	/guest:/path    an explicit absolute guest path
//
// The destination must be absolute. A bare name must be a safe identifier
// because it becomes a guest path component and is passed to the guest
// dew-oci-run launcher (which mkdir's it), so disallow separators/traversal.
func parseVolume(s string) (src, dest string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--volume: expected name:/path or /host:/path, got %q", s)
	}
	left, dst := parts[0], parts[1]
	if !strings.HasPrefix(dst, "/") {
		return "", "", fmt.Errorf("--volume: container path must be absolute, got %q", dst)
	}
	if strings.HasPrefix(left, "/") {
		return left, dst, nil
	}
	for _, r := range left {
		ok := r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", "", fmt.Errorf("--volume: invalid volume name %q (use letters, digits, '-' or '_')", left)
		}
	}
	return "/var/lib/dew/volumes/" + left, dst, nil
}

// parseShare accepts three forms:
//
//	hostpath                 tag derived from basename, ro
//	hostpath:rw|:ro          tag derived from basename, mode explicit
//	tag:hostpath[:rw|:ro]    tag explicit, mode optional (ro default)
//
// The earlier shape was tag:hostpath[:rw] only — the help text
// described `<hostdir>[:rw|:ro]` (host-first) so users following the
// docs hit a confusing "stat ro: no such file or directory" error.
// Now both shapes work; an explicit `:rw` or `:ro` suffix is detected
// unambiguously and stripped before the tag/hostpath split.
func parseShare(s string) (vm.SharedDir, error) {
	parts := strings.Split(s, ":")
	readOnly := true
	// Trailing :rw or :ro is the mode marker; strip it first.
	if n := len(parts); n >= 2 {
		switch parts[n-1] {
		case "rw":
			readOnly = false
			parts = parts[:n-1]
		case "ro":
			readOnly = true
			parts = parts[:n-1]
		}
	}
	var tag, hostPath string
	switch len(parts) {
	case 1:
		// hostpath only — derive tag from basename so the mount
		// point inside the guest is predictable.
		hostPath = parts[0]
		tag = filepath.Base(hostPath)
	case 2:
		tag = parts[0]
		hostPath = parts[1]
	default:
		return vm.SharedDir{}, fmt.Errorf(
			"--share: expected hostpath[:rw|:ro] or tag:hostpath[:rw|:ro], got %q", s)
	}
	if hostPath == "" {
		return vm.SharedDir{}, fmt.Errorf("--share: empty hostpath in %q", s)
	}
	return vm.SharedDir{
		Tag:      tag,
		HostPath: hostPath,
		ReadOnly: readOnly,
	}, nil
}
