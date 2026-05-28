//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/base64"
	"encoding/json"
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
	"github.com/solcreek/dew/internal/services"
	"github.com/solcreek/dew/internal/serialexec"
	"github.com/solcreek/dew/internal/session"
	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

const version = "0.1.0-dev"

var flagJSON bool
var flagStream bool
var flagEvents bool
var flagWith string
var flagProfile string

func dewDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dew")
}

func resolveAssets(cfg *vm.Config) error {
	dataDir := dewDataDir()
	profile := flagProfile
	if profile == "" {
		profile = "standard"
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
		cfg.Initrd = filepath.Join(dataDir, "initramfs-"+profile+".cpio.gz")
		if _, err := os.Stat(cfg.Initrd); err != nil {
			cfg.Initrd = filepath.Join(dataDir, "initramfs.cpio.gz")
		}
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

const (
	releaseBaseURL = "https://github.com/solcreek/dew/releases/download"
	releaseVersion = "v0.1.0"
)

func downloadAssets(dataDir, profile, kernelPath, initrdPath string) error {
	os.MkdirAll(dataDir, 0755)

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
			fmt.Sprintf("%s/%s/vmlinuz-%s", releaseBaseURL, releaseVersion, arch),
			kernelPath,
			"kernel",
		},
		{
			fmt.Sprintf("%s/%s/initramfs-%s-%s.cpio.gz", releaseBaseURL, releaseVersion, profile, arch),
			initrdPath,
			fmt.Sprintf("initramfs (%s)", profile),
		},
	}

	for _, f := range files {
		if _, err := os.Stat(f.dest); err == nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "  downloading %s...", f.name)

		resp, err := http.Get(f.url)
		if err != nil {
			fmt.Fprintf(os.Stderr, " failed\n")
			return fmt.Errorf("download %s: %w", f.name, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, " not found (HTTP %d)\n", resp.StatusCode)
			return fmt.Errorf("download %s: HTTP %d\n\n  Assets not available at %s\n  Build locally: bash initramfs/build.sh %s",
				f.name, resp.StatusCode, f.url, profile)
		}

		out, err := os.Create(f.dest)
		if err != nil {
			return fmt.Errorf("create %s: %w", f.dest, err)
		}
		written, err := io.Copy(out, resp.Body)
		out.Close()
		if err != nil {
			os.Remove(f.dest)
			return fmt.Errorf("download %s: %w", f.name, err)
		}
		fmt.Fprintf(os.Stderr, " %dMB\n", written/1024/1024)
	}
	return nil
}

func cmdAssets(args []string) error {
	sub := "pull"
	if len(args) > 0 {
		sub = args[0]
	}

	dataDir := dewDataDir()
	profile := flagProfile
	if profile == "" {
		profile = "standard"
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
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown()
	case "assets":
		err = cmdAssets(os.Args[2:])
	case "session":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: dew session <create|exec|destroy> [args]\n")
			os.Exit(1)
		}
		err = cmdSession(os.Args[2], os.Args[3:])
	case "version":
		fmt.Printf("dew %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "dew: unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dew: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `dew — ultra-lightweight VM (Apple Virtualization.framework)

Usage:
  dew up [dir]                   Auto-detect project and start dev environment
  dew start [flags]              Boot a Linux VM (interactive, daemon socket)
  dew run [flags] [--] <cmd>     Boot, execute command, exit
  dew exec <cmd>                 Execute in a running VM (via daemon socket)
  dew session create [flags]     Create a persistent VM session
  dew session exec <id> <cmd>    Execute in an existing session
  dew session destroy <id>       Destroy a session
  dew down                       Stop the running VM
  dew assets pull                Download VM image for current profile
  dew assets list                Show downloaded assets
  dew version                    Print version
  dew help                       Show this help

Flags:
  --kernel <path>      Path to vmlinuz (required)
  --initrd <path>      Path to initramfs
  --cpus <n>           vCPUs (default: 1)
  --memory <mb>        Memory in MB (default: 512)
  --network            Enable NAT networking
  --vsock <port>       Enable vsock on this port
  --share <tag:path>   Share host directory (read-only; tag:hostpath[:rw])
  --profile <name>      VM profile: standard (default) or minimal
  --disk <path>         Persistent disk image (created if absent, default 10GB)
  --forward <h:g>      Forward host port to guest (e.g. 3000:3000)
  --with <services>    Start services alongside app (e.g. postgres,redis)
  --stream             Stream stdout/stderr in real time
  --events             NDJSON event stream (for agent integration)
  --json               Machine-readable JSON output (run command)
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

	// Pass shared dir mount points to guest via kernel cmdline
	for _, sd := range cfg.SharedDirs {
		cfg.CmdLine += fmt.Sprintf(" dew.share=%s:/%s", sd.Tag, sd.Tag)
	}

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

	// Race vsock connect against serial ready
	vsockCh := make(chan net.Conn, 1)
	go func() {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err == nil {
			vsockCh <- conn
		}
		close(vsockCh)
	}()
	go func() {
		sExec.WaitReady(15 * time.Second)
	}()

	// Inject auth token via vsock (not kernel cmdline)
	var tokenSent bool
	select {
	case conn := <-vsockCh:
		if conn != nil {
			req := vsockProto.SetTokenRequest{Type: vsockProto.TypeSetToken, Token: token}
			vsockProto.WriteJSON(conn, &req)
			var resp vsockProto.ConnectResponse
			vsockProto.ReadJSON(conn, &resp)
			conn.Close()
			tokenSent = resp.OK
		}
	case <-time.After(10 * time.Second):
	}

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
		if err := sExec.WaitReady(5 * time.Second); err != nil {
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	} else {
		if result.Stdout != "" {
			fmt.Print(result.Stdout)
		}
		if result.Stderr != "" {
			fmt.Fprint(os.Stderr, result.Stderr)
		}
		fmt.Fprintf(os.Stderr, "dew: exit code %d\n", result.ExitCode)
	}
	exitCode := result.ExitCode

	d.Stop(context.Background())
	hostReader.Close()
	hostWriter.Close()

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
	if proj.Framework == "" {
		return fmt.Errorf("no supported project detected in %s", dir)
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
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  detected: %s", proj.Framework)
		if proj.PackageMgr != "" {
			fmt.Fprintf(os.Stderr, " (%s)", proj.PackageMgr)
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  profile:  %s\n", proj.Profile)
		fmt.Fprintf(os.Stderr, "  port:     %d\n", proj.Port)
		fmt.Fprintf(os.Stderr, "\n")
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

	// Remove stale socket
	os.Remove(daemon.SocketPath(""))

	d, err := darwin.New(cfg)
	if err != nil {
		return err
	}

	emit(map[string]interface{}{"type": "boot", "status": "starting"})
	if !flagJSON && !flagEvents {
		fmt.Fprintf(os.Stderr, "  booting...")
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
	if !flagJSON && !flagEvents {
		fmt.Fprintf(os.Stderr, " %dms\n", bootMs)
	}

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
		if !flagJSON && !flagEvents {
			fmt.Fprintf(os.Stderr, "  warning: agent not ready\n")
		}
	}

	startPortForwards(d, token, cfg.Forwards)

	dmn := &daemon.State{
		VM: d, Token: token, VsockPort: cfg.VsockPort,
		SocketPath: daemon.SocketPath(""),
	}
	dmn.Start()

	execInVM := func(cmd string) (*RunResult, error) {
		conn, err := connectVsock(d, cfg.VsockPort)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		return execVsockConn(conn, token, cmd)
	}

	// Project is mounted via virtiofs at /app (live sync from host)
	// node_modules on VM tmpfs (not on host via virtiofs)
	if proj.InstallCmd != "" {
		emit(map[string]interface{}{"type": "install", "status": "starting", "cmd": proj.InstallCmd})
		if !flagJSON && !flagEvents {
			fmt.Fprintf(os.Stderr, "  installing deps...\n")
		}
		t := time.Now()
		result, err := execInVM("mkdir -p /tmp/nm && cd /app && ln -sf /tmp/nm node_modules && " + proj.InstallCmd)
		installMs := time.Since(t).Milliseconds()
		if err != nil || (result != nil && result.ExitCode != 0) {
			errMsg := ""
			suggestion := ""
			if result != nil {
				errMsg = result.Stderr
				if strings.Contains(errMsg, "peer dep") || strings.Contains(errMsg, "ERESOLVE") {
					suggestion = "try adding --legacy-peer-deps to install command"
				}
			}
			emit(map[string]interface{}{
				"type": "install", "status": "failed",
				"elapsed_ms": installMs, "error": errMsg, "suggestion": suggestion,
			})
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "  install failed (%dms)\n", installMs)
			}
		} else {
			emit(map[string]interface{}{"type": "install", "status": "done", "elapsed_ms": installMs})
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "  deps installed (%dms)\n", installMs)
			}
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
				if !flagJSON && !flagEvents {
					fmt.Fprintf(os.Stderr, "  unknown service: %s\n", name)
				}
				continue
			}
			emit(map[string]interface{}{"type": "service", "status": "starting", "name": svc.Name, "port": svc.Port})
			if !flagJSON && !flagEvents {
				fmt.Fprintf(os.Stderr, "  starting %s (port %d)...\n", svc.Name, svc.Port)
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

	// Start dev server
	emit(map[string]interface{}{"type": "serve", "status": "starting", "cmd": proj.DevCmd})
	if !flagJSON && !flagEvents {
		fmt.Fprintf(os.Stderr, "  starting dev server...\n")
	}
	execInVM("cd /app && " + proj.DevCmd + " &")

	// Health check — poll until dev server responds
	if !flagJSON && !flagEvents {
		fmt.Fprintf(os.Stderr, "  waiting for server...")
	}
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

	totalMs := time.Since(start).Milliseconds()
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
		} else if !flagEvents {
			fmt.Fprintf(os.Stderr, " ok\n\n  ✓ http://localhost:%d\n\n  Ctrl+C to stop\n\n", proj.Port)
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
		} else if !flagEvents {
			fmt.Fprintf(os.Stderr, " timeout\n\n  ? http://localhost:%d (may still be starting)\n\n  Ctrl+C to stop\n\n", proj.Port)
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
	if len(args) == 0 {
		return fmt.Errorf("usage: dew exec <cmd...>")
	}
	cmd := strings.Join(args, " ")

	sockPath := daemon.SocketPath("")
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("no running VM (socket %s): %w", sockPath, err)
	}
	defer conn.Close()

	req := daemon.ExecRequest{Command: cmd}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	var resp vsockProto.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("recv: %w", err)
	}

	if resp.Stdout != "" {
		fmt.Print(resp.Stdout)
	}
	if resp.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Stderr)
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

func execVsockConn(conn net.Conn, token string, cmd string) (*RunResult, error) {
	req := vsockProto.ExecRequest{Token: token, Command: "/bin/sh", Args: []string{"-c", cmd}}
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
