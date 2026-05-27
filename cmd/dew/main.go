//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/solcreek/dew/internal/serialexec"
	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

const version = "0.1.0-dev"

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
  dew start [flags]          Boot a Linux VM (interactive)
  dew run [flags] -- <cmd>   Boot, execute command, exit
  dew version                Print version
  dew help                   Show this help

Flags:
  --kernel <path>      Path to vmlinuz (required)
  --initrd <path>      Path to initramfs
  --cpus <n>           vCPUs (default: 1)
  --memory <mb>        Memory in MB (default: 512)
  --network            Enable NAT networking
  --vsock <port>       Enable vsock on this port
  --share <tag:path>   Share host directory (tag:hostpath[:ro])
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
		default:
			return cfg, nil, fmt.Errorf("unknown flag %q", args[i])
		}
	}

	return cfg, remaining, nil
}

func cmdStart(args []string) error {
	cfg, _, err := parseFlags(args)
	if err != nil {
		return err
	}
	if cfg.Kernel == "" {
		return fmt.Errorf("--kernel is required")
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

	<-ctx.Done()

	fmt.Fprintf(os.Stderr, "\ndew: stopping VM\n")
	return d.Stop(context.Background())
}

func cmdRun(args []string) error {
	cfg, cmdArgs, err := parseFlags(args)
	if err != nil {
		return err
	}
	if cfg.Kernel == "" {
		return fmt.Errorf("--kernel is required")
	}
	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command specified (use -- <cmd>)")
	}

	// Always enable vsock for run mode
	if cfg.VsockPort == 0 {
		cfg.VsockPort = 1024
	}

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

	// Wait for guest init to complete (serial console)
	sExec := serialexec.New(hostReader, hostWriter)
	fmt.Fprintf(os.Stderr, "dew: waiting for guest\n")
	if err := sExec.WaitReady(15 * time.Second); err != nil {
		d.Stop(context.Background())
		return fmt.Errorf("guest not ready: %w", err)
	}

	// Try vsock exec (clean channel), fall back to serial
	cmd := strings.Join(cmdArgs, " ")
	output, exitCode, err := execViaVsock(d, cfg.VsockPort, cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dew: vsock unavailable, using serial\n")
		output, exitCode, err = sExec.Run(cmd)
	}
	if err != nil {
		d.Stop(context.Background())
		return fmt.Errorf("exec: %w", err)
	}

	if output != "" {
		fmt.Println(output)
	}
	fmt.Fprintf(os.Stderr, "dew: exit code %d\n", exitCode)

	d.Stop(context.Background())
	hostReader.Close()
	hostWriter.Close()

	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func execViaVsock(d *darwin.DarwinVM, port uint32, cmd string) (string, int, error) {
	// Retry vsock connect — agent may not be listening yet
	var conn *vz.VirtioSocketConnection
	var err error
	for i := 0; i < 20; i++ {
		conn, err = d.VsockConnect(port)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return "", -1, err
	}
	defer conn.Close()

	req := vsockProto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", cmd}}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return "", -1, err
	}

	var resp vsockProto.ExecResponse
	if err := vsockProto.ReadJSON(conn, &resp); err != nil {
		return "", -1, err
	}

	output := resp.Stdout
	if resp.Stderr != "" {
		output += resp.Stderr
	}
	return strings.TrimRight(output, "\n"), resp.ExitCode, nil
}

func parseShare(s string) (vm.SharedDir, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return vm.SharedDir{}, fmt.Errorf("--share: expected tag:hostpath[:ro], got %q", s)
	}
	sd := vm.SharedDir{
		Tag:      parts[0],
		HostPath: parts[1],
	}
	if len(parts) == 3 && parts[2] == "ro" {
		sd.ReadOnly = true
	}
	return sd, nil
}
