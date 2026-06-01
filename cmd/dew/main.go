//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/base64"
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
	"time"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/detect"
	"github.com/solcreek/dew/internal/progress"
	"github.com/solcreek/dew/internal/selfupdate"
	"github.com/solcreek/dew/internal/services"
	"github.com/solcreek/dew/internal/serialexec"
	"github.com/solcreek/dew/internal/session"
	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
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
var flagDryRun bool
var flagProfile string

func dewDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dew")
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

	// Profile-aware defaults
	switch profile {
	case "node":
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 1024
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "node.img")
			cfg.DiskGB = 4
		}
	case "python":
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 1024
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "python.img")
			cfg.DiskGB = 4
		}
	case "standard":
		if cfg.MemoryMB == 512 {
			cfg.MemoryMB = 2048
		}
		if cfg.DiskPath == "" {
			cfg.DiskPath = filepath.Join(dataDir, "standard.img")
			cfg.DiskGB = 10
		}
	}

	if cfg.Kernel == "" {
		cfg.Kernel = filepath.Join(dataDir, "vmlinuz")
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
		cfg.Initrd = filepath.Join(dataDir, "initramfs-"+profile+".cpio.gz")
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
		if err := downloadAssets(dataDir, profile, cfg.Kernel, cfg.Initrd); err != nil {
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

func downloadAssets(dataDir, profile, kernelPath, initrdPath string) error {
	os.MkdirAll(dataDir, 0755)

	base := releaseBaseURL
	if releaseBaseURLOverride != "" {
		base = releaseBaseURLOverride
	}

	arch := "x86_64"
	if goArch := os.Getenv("GOARCH"); goArch == "arm64" {
		arch = "aarch64"
	}
	// Detect arm64 from runtime
	if filepath.Base(os.Args[0]) != "" {
		// runtime detection via uname
		cmd := exec.Command("uname", "-m")
		if out, err := cmd.Output(); err == nil {
			a := strings.TrimSpace(string(out))
			if a == "arm64" || a == "aarch64" {
				arch = "aarch64"
			}
		}
	}

	files := []struct {
		url  string
		dest string
		name string
	}{
		{
			fmt.Sprintf("%s/vmlinuz-%s", base, arch),
			kernelPath,
			"kernel",
		},
		{
			fmt.Sprintf("%s/initramfs-%s-%s.cpio.gz", base, profile, arch),
			initrdPath,
			fmt.Sprintf("initramfs (%s)", profile),
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
			results[i] = result{name: f.name, written: -1}
			continue
		}
		fmt.Fprintf(os.Stderr, "  downloading %s...\n", f.name)
		wg.Add(1)
		go func(idx int, file struct {
			url  string
			dest string
			name string
		}) {
			defer wg.Done()
			results[idx] = fetchAsset(file.url, file.dest, file.name, profile)
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

func fetchAsset(url, dest, name, profile string) (r struct {
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
	out, err := os.Create(tmp)
	if err != nil {
		r.err = fmt.Errorf("create %s: %w", tmp, err)
		return
	}
	written, err := io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		r.err = fmt.Errorf("download %s: %w", name, err)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		r.err = fmt.Errorf("install %s: %w", name, err)
		return
	}
	r.written = written
	return
}

func cmdAssets(args []string) error {
	sub := "pull"
	if len(args) > 0 {
		sub = args[0]
	}

	dataDir := dewDataDir()
	profile := flagProfile
	if profile == "" {
		profile = "minimal"
	}
	// Parse --profile from remaining args
	for i, a := range args {
		if a == "--profile" && i+1 < len(args) {
			profile = args[i+1]
		}
	}

	switch sub {
	case "pull":
		kernelPath := filepath.Join(dataDir, "vmlinuz")
		initrdPath := filepath.Join(dataDir, "initramfs-"+profile+".cpio.gz")
		fmt.Fprintf(os.Stderr, "  profile: %s\n  target:  %s\n\n", profile, dataDir)
		return downloadAssets(dataDir, profile, kernelPath, initrdPath)

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

	// Pre-scan args for --json so the global flag is set BEFORE any
	// cmd dispatch (including the unknown-command error path).
	for _, a := range os.Args[1:] {
		if a == "--json" {
			flagJSON = true
			break
		}
	}

	// Only check for updates on user-facing commands, not internal (exec, start).
	// Skip in JSON mode — agents don't want noise.
	cmd := os.Args[1]
	if !flagJSON && cmd != "exec" && cmd != "start" && cmd != "run" && cmd != "serve" {
		go selfupdate.CheckBackground(version)
	}

	var err error
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "install":
		err = cmdInstall(os.Args[2:])
	case "app":
		err = cmdInstall(os.Args[2:])
	case "apps":
		err = cmdInstallList()
	case "build":
		err = cmdBuild(os.Args[2:])
	case "deploy":
		err = cmdDeploy(os.Args[2:])
	case "rollback":
		err = cmdRollback(os.Args[2:])
	case "share":
		err = cmdShare(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown()
	case "assets":
		err = cmdAssets(os.Args[2:])
	case "session":
		if len(os.Args) < 3 {
			err = dewerr.New(dewerr.CodeUsage, "usage: dew session <create|exec|destroy> [args]")
			break
		}
		err = cmdSession(os.Args[2], os.Args[3:])
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "env":
		err = cmdEnv(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "server":
		err = cmdServer(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "update":
		err = selfupdate.Update(version)
	case "version", "--version", "-v":
		fmt.Printf("dew %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		err = dewerr.Newf(dewerr.CodeUsage, "unknown command %q", os.Args[1])
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

Dev:
  dew up [dir]                   Start dev environment (auto-detect project)
  dew up --with postgres,redis   Dev with services
  dew down                       Stop dev environment

Share:
  dew share [port]               Create temporary public HTTPS URL

Apps:
  Run open-source apps locally — no Docker needed.
  dew apps                       Browse available apps
  dew app run <name> [--port N]  Run an app
  dew app stop <name>            Stop an app
  dew app list [--json]           Show running apps

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
  dew session ...                Persistent VM sessions
  dew assets ...                 Manage VM images
  dew doctor                     Diagnose environment issues
  dew update                     Update to latest version
  dew version                    Print version

Output:
  --json        Machine-readable JSON (all commands)
  --events      NDJSON lifecycle stream
  --stream      Stream stdout/stderr
  --dry-run     Validate without executing (deploy, app run, server create)

Network (for commands that take --network):
  --network-policy open|restricted
                When restricted, the guest's outbound traffic is
                default-DROP except loopback, DNS, and IPs added via
                --allow-host. Hostname-aware allowlist is planned.
  --allow-host HOST
                Repeatable. Resolves HOST at the host and permits the
                guest to reach those IPs. Only meaningful with
                --network-policy=restricted.
`)
}

func parseFlags(args []string) (vm.Config, []string, error) {
	cfg := vm.Config{
		CPUs:     1,
		MemoryMB: 512,
		CmdLine:  "console=hvc0",
	}
	var remaining []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			remaining = args[i+1:]
			return cfg, remaining, nil
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
				return cfg, nil, fmt.Errorf("--share requires tag:hostpath[:ro]")
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
		case "--stream":
			flagStream = true
		case "--events":
			flagEvents = true
			flagStream = true
		case "--json":
			flagJSON = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return cfg, nil, fmt.Errorf("unknown flag %q", args[i])
			}
			remaining = args[i:]
			return cfg, remaining, nil
		}
	}

	return cfg, remaining, nil
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
	if cfg.Network && cfg.NetworkPolicy == "restricted" {
		cfg.CmdLine += " dew.netpolicy=restricted"
		if len(cfg.AllowHosts) > 0 {
			cfg.CmdLine += " dew.allow=" + strings.Join(cfg.AllowHosts, ",")
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

	d, err := darwin.New(cfg)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dew: booting VM (cpus=%d, memory=%dMB)\n", cfg.CPUs, cfg.MemoryMB)
	start := time.Now()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dew: VM running (%s)\n", time.Since(start).Round(time.Millisecond))

	// Remove stale socket from previous run
	os.Remove(daemon.SocketPath(""))

	// Wait for guest agent and inject auth token (retry until VM is ready)
	fmt.Fprintf(os.Stderr, "dew: waiting for guest agent\n")
	tokenSent := false
	for i := 0; i < 300; i++ { // up to 30s
		if err := sendToken(d, cfg.VsockPort, token); err == nil {
			tokenSent = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !tokenSent {
		fmt.Fprintf(os.Stderr, "dew: warning: token handshake failed, daemon may not work\n")
	}
	startPortForwards(d, token, cfg.Forwards)

	// Start daemon socket AFTER token is set (so clients can exec immediately)
	dmn := &daemon.State{
		VM:         d,
		Token:      token,
		VsockPort:  cfg.VsockPort,
		SocketPath: daemon.SocketPath(""),
	}
	if err := dmn.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "dew: daemon: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "dew: daemon socket %s\n", dmn.SocketPath)
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
	if err := resolveAssets(&cfg); err != nil {
		return err
	}
	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command specified (use -- <cmd>)")
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

	fmt.Fprintf(os.Stderr, "dew: booting VM\n")
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "dew: VM running (%s)\n", time.Since(start).Round(time.Millisecond))

	sExec := serialexec.New(hostReader, hostWriter)

	// Wait for guest agent to come up on vsock, then send the auth
	// token. Poll up to 60s — covers cold first-boot of any profile,
	// where the guest's dew-agent may take a while to listen after
	// VM start. Previous 10s+5s window was just enough for warm boot
	// and failed cold every time.
	const vsockReadySec = 60
	var tokenSent bool
	for i := 0; i < vsockReadySec*10; i++ {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err == nil {
			req := vsockProto.SetTokenRequest{Type: vsockProto.TypeSetToken, Token: token}
			vsockProto.WriteJSON(conn, &req)
			var resp vsockProto.ConnectResponse
			vsockProto.ReadJSON(conn, &resp)
			conn.Close()
			tokenSent = resp.OK
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Kick the serial path in parallel as a fallback for guests that
	// somehow don't bring up vsock (rare; older agent builds).
	go func() { sExec.WaitReady(time.Duration(vsockReadySec) * time.Second) }()

	cmd := strings.Join(cmdArgs, " ")
	var result *RunResult

	if tokenSent {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err == nil {
			if flagStream {
				exitCode, serr := execVsockStream(conn, token, cmd)
				conn.Close()
				d.Stop(context.Background())
				hostReader.Close()
				hostWriter.Close()
				if serr != nil {
					return fmt.Errorf("exec: %w", serr)
				}
				if exitCode != 0 {
					os.Exit(exitCode)
				}
				return nil
			}
			result, err = execVsockConn(conn, token, cmd)
			conn.Close()
		}
	}

	if result == nil {
		fmt.Fprintf(os.Stderr, "dew: vsock unavailable, using serial\n")
		if err := sExec.WaitReady(60 * time.Second); err != nil {
			d.Stop(context.Background())
			return fmt.Errorf("guest not ready: %w", err)
		}
		output, exitCode, serr := sExec.Run(cmd)
		if serr != nil {
			d.Stop(context.Background())
			return fmt.Errorf("exec: %w", serr)
		}
		result = &RunResult{ExitCode: exitCode, Stdout: output}
		err = nil
	}
	if err != nil {
		d.Stop(context.Background())
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

func execVsockStream(conn net.Conn, token string, cmd string) (int, error) {
	req := vsockProto.ExecRequest{
		Type: vsockProto.TypeExec, Token: token, Stream: true,
		Command: "/bin/sh", Args: []string{"-c", cmd},
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
			var check struct{ Stream string `json:"stream"` }
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

func cmdUp(args []string) error {
	parsedCfg, remaining, err := parseFlags(args)
	if err != nil {
		return err
	}
	dir := "."
	if len(remaining) > 0 {
		dir = remaining[0]
	}

	proj, err := detect.Detect(dir)
	if err != nil {
		return err
	}
	if proj.Framework == "" && proj.Runtime == "" {
		// "Floor = works" — don't punish first contact. Surface multiple
		// exits so beginners + agents have a parseable next step. Error
		// code `no_project_detected` is grep-able for agents. Every
		// suggested command below must work today; never point at planned
		// commands that don't yet exist.
		return fmt.Errorf("no project detected in %s [no_project_detected]\n\nQuick options:\n  • dew up --profile minimal     — boot a minimal Linux VM here\n  • dew start --profile minimal  — same, returns immediately, use 'dew exec' afterwards\n  • dew app run code             — run an OSS app like VS Code\n\nDocs: https://dewvm.dev/start", dir)
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
		fmt.Fprintf(os.Stderr, "  detected: %s", proj.Framework)
		if proj.PackageMgr != "" {
			fmt.Fprintf(os.Stderr, " (%s)", proj.PackageMgr)
		}
		fmt.Fprintf(os.Stderr, "\n\n")
	}

	absDir, _ := filepath.Abs(dir)
	flagProfile = proj.Profile
	// --with requires containerd → upgrade to standard profile
	if flagWith != "" && flagProfile != "standard" {
		flagProfile = "standard"
	}
	cfg := vm.Config{
		CPUs:     parsedCfg.CPUs,
		MemoryMB: parsedCfg.MemoryMB,
		Kernel:   parsedCfg.Kernel,
		Initrd:   parsedCfg.Initrd,
		CmdLine:  "console=hvc0",
		Network:  true,
		Forwards: []vm.PortForward{{HostPort: proj.Port, GuestPort: proj.Port}},
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

	token := generateToken()

	var spin *progress.Spinner
	if !flagJSON && !flagEvents {
		spin = progress.New()
	}

	// Remove stale socket
	os.Remove(daemon.SocketPath(""))

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

	if err := d.Start(ctx); err != nil {
		emit(map[string]interface{}{"type": "boot", "status": "failed", "error": err.Error()})
		return err
	}
	bootMs := time.Since(start).Milliseconds()
	emit(map[string]interface{}{"type": "boot", "status": "ready", "elapsed_ms": bootMs})

	// Wait for agent + inject token
	tokenSent := false
	for i := 0; i < 300; i++ {
		if err := sendToken(d, cfg.VsockPort, token); err == nil {
			tokenSent = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !tokenSent {
		emit(map[string]interface{}{"type": "agent", "status": "failed", "error": "token handshake timeout"})
		if spin != nil {
			spin.Fail("agent not ready")
		}
	}

	startPortForwards(d, token, cfg.Forwards)

	dmn := &daemon.State{
		VM: d, Token: token, VsockPort: cfg.VsockPort,
		SocketPath: daemon.SocketPath(""),
	}
	dmn.Start()

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
	// node_modules is intentionally redirected to the guest's tmpfs so
	// the native bindings are Linux+musl (the host might be macOS) and
	// so npm doesn't churn the host filesystem.
	//
	// Earlier versions tried `ln -sf /tmp/nm node_modules`, which
	// silently descended into a pre-existing node_modules directory.
	// virtiofs also makes `rm -rf` unreliable from inside the guest
	// (exit code 1, partial deletion). A bind mount sidesteps both:
	// /app/node_modules as seen by the guest is the empty tmpfs at
	// /tmp/nm, regardless of whatever stale tree the host still has
	// underneath.
	if proj.InstallCmd != "" {
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
		}
	}

	// Start services (--with postgres,redis)
	if flagWith != "" {
		svcNames := strings.Split(flagWith, ",")
		for _, name := range svcNames {
			name = strings.TrimSpace(name)
			svc := services.Lookup(name)
			if svc == nil {
				emit(map[string]interface{}{
					"type": "service", "status": "failed", "name": name,
					"error": "unknown service", "suggestion": "available: " + strings.Join(services.Names(), ", "),
				})
				if spin != nil {
					spin.Fail(fmt.Sprintf("unknown service: %s", name))
				}
				continue
			}
			emit(map[string]interface{}{"type": "service", "status": "starting", "name": svc.Name, "port": svc.Port})
			if spin != nil {
				spin.Step(fmt.Sprintf("%s (port %d)", svc.Name, svc.Port))
			}
			runCmd := services.NerdctlRunCmd(*svc)
			execInVM(runCmd)

			// Add port forward for this service
			cfg.Forwards = append(cfg.Forwards, vm.PortForward{HostPort: svc.Port, GuestPort: svc.Port})
			go func(f vm.PortForward) {
				ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.HostPort))
				if err != nil {
					return
				}
				for {
					tcpConn, err := ln.Accept()
					if err != nil {
						return
					}
					go proxyToGuest(d, token, tcpConn, f.GuestPort)
				}
			}(vm.PortForward{HostPort: svc.Port, GuestPort: svc.Port})

			emit(map[string]interface{}{"type": "service", "status": "started", "name": svc.Name, "port": svc.Port})
		}
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
	url := fmt.Sprintf("http://localhost:%d/", proj.Port)
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			healthy = true
			break
		}
	}

	totalElapsed := time.Since(start)
	totalMs := totalElapsed.Milliseconds()
	if healthy {
		emit(map[string]interface{}{
			"type": "health", "status": "ok",
			"url": url, "elapsed_ms": totalMs,
		})
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.Encode(map[string]interface{}{
				"status": "ready", "url": url, "port": proj.Port,
				"framework": proj.Framework, "elapsed_ms": totalMs,
			})
		} else if spin != nil {
			spin.Done(url)
			fmt.Fprintf(os.Stderr, "  Ctrl+C to stop\n")
		}
	} else {
		emit(map[string]interface{}{
			"type": "health", "status": "timeout",
			"url": url, "elapsed_ms": totalMs,
			"suggestion": "server may still be starting, try opening the URL manually",
		})
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.Encode(map[string]interface{}{
				"status": "timeout", "url": url, "port": proj.Port,
				"framework": proj.Framework, "elapsed_ms": totalMs,
			})
		} else if spin != nil {
			spin.Timeout(url)
			fmt.Fprintf(os.Stderr, "  Ctrl+C to stop\n")
		}
	}

	<-ctx.Done()

	dmn.Stop()
	fmt.Fprintf(os.Stderr, "\n  stopping...\n")
	return d.Stop(context.Background())
}

func cmdDown() error {
	for _, a := range os.Args[2:] {
		if a == "--json" {
			flagJSON = true
		}
	}
	sockPath := daemon.SocketPath("")
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

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(map[string]interface{}{"status": "stopped"})
	} else {
		fmt.Fprintf(os.Stderr, "dew: stopped\n")
	}
	return nil
}

func cmdExec(args []string) error {
	// Strip --json so it isn't joined into the guest command.
	wantJSON := flagJSON
	filtered := args[:0]
	for _, a := range args {
		if a == "--json" {
			wantJSON = true
			flagJSON = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage, "usage: dew exec <cmd...>")
	}
	cmd := strings.Join(args, " ")

	sockPath := daemon.SocketPath("")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return dewerr.Wrapf(err, dewerr.CodeConflict, "no running VM (socket %s)", sockPath)
	}
	defer conn.Close()

	req := daemon.ExecRequest{Command: cmd}
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

func execVsockConn(conn net.Conn, token string, cmd string) (*RunResult, error) {
	return execVsockConnTimeout(conn, token, cmd, 0)
}

// execVsockConnTimeout sends a per-call timeout to the guest agent so
// long-running commands (`npm install`, `pip install`, the dependency
// install path in `dew up`) don't trip the agent's default 30 s exec
// timeout. Pass 0 to use the agent default.
func execVsockConnTimeout(conn net.Conn, token string, cmd string, timeout time.Duration) (*RunResult, error) {
	req := vsockProto.ExecRequest{Token: token, Command: "/bin/sh", Args: []string{"-c", cmd}}
	if timeout > 0 {
		req.TimeoutMs = int(timeout / time.Millisecond)
	}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return nil, err
	}
	var resp vsockProto.ExecResponse
	if err := vsockProto.ReadJSON(conn, &resp); err != nil {
		return nil, err
	}
	return &RunResult{
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}, nil
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
	if err := vsockProto.ReadJSON(conn, &resp); err != nil {
		return fmt.Errorf("token response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("token rejected: %s", resp.Error)
	}
	return nil
}

func connectVsock(v vm.VM, port uint32) (net.Conn, error) {
	var conn net.Conn
	var err error
	for i := 0; i < 500; i++ {
		conn, err = v.VsockConnect(port)
		if err == nil {
			return conn, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, err
}

// sessions stores active sessions in-process (for daemon mode).
// For CLI usage, session create forks a background process.
var activeSessions = map[string]*session.Session{}
var sessionMu sync.Mutex

func cmdSession(sub string, args []string) error {
	switch sub {
	case "create":
		return cmdSessionCreate(args)
	case "exec":
		return cmdSessionExec(args)
	case "destroy":
		return cmdSessionDestroy(args)
	default:
		return fmt.Errorf("unknown session subcommand %q", sub)
	}
}

func cmdSessionCreate(args []string) error {
	cfg, _, err := parseFlags(args)
	if err != nil {
		return err
	}
	if err := resolveAssets(&cfg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dew: creating session\n")
	start := time.Now()

	s, err := session.Create(cfg)
	if err != nil {
		return err
	}

	sessionMu.Lock()
	activeSessions[s.ID] = s
	sessionMu.Unlock()

	fmt.Fprintf(os.Stderr, "dew: session ready (%s)\n", time.Since(start).Round(time.Millisecond))

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(map[string]string{"id": s.ID})
	} else {
		fmt.Println(s.ID)
	}
	return nil
}

func cmdSessionExec(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: dew session exec <id> <cmd...>")
	}
	id := args[0]
	cmd := strings.Join(args[1:], " ")

	sessionMu.Lock()
	s, ok := activeSessions[id]
	sessionMu.Unlock()

	if !ok {
		return fmt.Errorf("session %q not found (sessions are in-process only)", id)
	}

	start := time.Now()
	result, err := s.Exec(cmd, 0)
	if err != nil {
		return err
	}

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(result)
	} else {
		if result.Stdout != "" {
			fmt.Print(result.Stdout)
		}
		if result.Stderr != "" {
			fmt.Fprint(os.Stderr, result.Stderr)
		}
		fmt.Fprintf(os.Stderr, "dew: exec %s (exit %d)\n", time.Since(start).Round(time.Millisecond), result.ExitCode)
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

func cmdSessionDestroy(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: dew session destroy <id>")
	}
	id := args[0]

	sessionMu.Lock()
	s, ok := activeSessions[id]
	if ok {
		delete(activeSessions, id)
	}
	sessionMu.Unlock()

	if !ok {
		return fmt.Errorf("session %q not found", id)
	}

	return s.Destroy()
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

func startPortForwards(v vm.VM, token string, forwards []vm.PortForward) {
	for _, fwd := range forwards {
		go func(f vm.PortForward) {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.HostPort))
			if err != nil {
				fmt.Fprintf(os.Stderr, "dew: forward %d:%d listen failed: %v\n", f.HostPort, f.GuestPort, err)
				return
			}
			fmt.Fprintf(os.Stderr, "dew: forwarding 127.0.0.1:%d → guest:%d\n", f.HostPort, f.GuestPort)
			for {
				tcpConn, err := ln.Accept()
				if err != nil {
					return
				}
				go proxyToGuest(v, token, tcpConn, f.GuestPort)
			}
		}(fwd)
	}
}

func proxyToGuest(v vm.VM, token string, tcpConn net.Conn, guestPort int) {
	defer tcpConn.Close()

	vsockConn, err := v.VsockConnect(uint32(vsockProto.DefaultPort))
	if err != nil {
		return
	}
	defer vsockConn.Close()

	req := vsockProto.ConnectRequest{
		Type:  vsockProto.TypeConnect,
		Token: token,
		Addr:  fmt.Sprintf("127.0.0.1:%d", guestPort),
	}
	if err := vsockProto.WriteJSON(vsockConn, &req); err != nil {
		return
	}

	var resp vsockProto.ConnectResponse
	if err := vsockProto.ReadJSON(vsockConn, &resp); err != nil {
		return
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "dew: forward connect failed: %s\n", resp.Error)
		return
	}

	done := make(chan struct{})
	go func() { io.Copy(vsockConn, tcpConn); close(done) }()
	io.Copy(tcpConn, vsockConn)
	<-done
}

func parseShare(s string) (vm.SharedDir, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return vm.SharedDir{}, fmt.Errorf("--share: expected tag:hostpath[:rw], got %q", s)
	}
	sd := vm.SharedDir{
		Tag:      parts[0],
		HostPath: parts[1],
		ReadOnly: true,
	}
	if len(parts) == 3 && parts[2] == "rw" {
		sd.ReadOnly = false
	}
	return sd, nil
}
