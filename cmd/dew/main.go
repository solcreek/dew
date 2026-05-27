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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solcreek/dew/internal/daemon"
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
	if _, err := os.Stat(cfg.Kernel); err != nil {
		return fmt.Errorf("kernel not found at %s — run: dew assets pull", cfg.Kernel)
	}
	if _, err := os.Stat(cfg.Initrd); err != nil {
		return fmt.Errorf("initramfs not found at %s — run: dew assets pull", cfg.Initrd)
	}
	return nil
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
  dew start [flags]              Boot a Linux VM (interactive, daemon socket)
  dew run [flags] [--] <cmd>     Boot, execute command, exit
  dew exec <cmd>                 Execute in a running VM (via daemon socket)
  dew session create [flags]     Create a persistent VM session
  dew session exec <id> <cmd>    Execute in an existing session
  dew session destroy <id>       Destroy a session
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
