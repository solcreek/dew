// Package vm defines the platform-agnostic interface for Dew's
// virtual machine layer.
//
// macOS: Apple Virtualization.framework via Code-Hex/vz
// Linux: no VM needed — containerd runs natively
package vm

import (
	"context"
	"io"
	"os"
)

// State represents the VM lifecycle state.
type State int

const (
	StateStopped  State = iota
	StateStarting
	StateRunning
	StatePaused
	StateStopping
	StateError
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateStopping:
		return "stopping"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// SharedDir maps a host directory into the guest via virtiofs.
type SharedDir struct {
	Tag      string // mount tag visible inside the guest
	HostPath string
	ReadOnly bool
}

// Config describes the desired VM parameters.
type Config struct {
	Name       string
	CPUs       uint
	MemoryMB   uint64
	Kernel     string // path to vmlinuz
	Initrd     string // path to initramfs
	CmdLine    string // kernel command line
	DiskPath   string // optional persistent disk image
	DiskGB     uint   // disk size in GB (for creation)
	VsockPort  uint32 // if >0, create a vsock device on this port
	Network    bool   // if true, attach a NAT network device
	SharedDirs []SharedDir

	// Console overrides the serial console file handles.
	// If nil, os.Stdin/os.Stdout are used (interactive mode).
	Console *ConsoleFiles
}

// ConsoleFiles specifies explicit file handles for the serial console.
type ConsoleFiles struct {
	In  *os.File // host reads guest output from here
	Out *os.File // host writes guest input here
}

// NewConsolePipe creates a pair of OS pipes for programmatic serial
// console access. Returns (console for VM config, hostReader, hostWriter).
func NewConsolePipe() (*ConsoleFiles, io.ReadCloser, io.WriteCloser, error) {
	// guest stdout → host reads
	guestOutR, guestOutW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	// host writes → guest stdin
	guestInR, guestInW, err := os.Pipe()
	if err != nil {
		guestOutR.Close()
		guestOutW.Close()
		return nil, nil, nil, err
	}
	console := &ConsoleFiles{
		In:  guestInR,  // VM reads input from this pipe
		Out: guestOutW, // VM writes output to this pipe
	}
	return console, guestOutR, guestInW, nil
}

// VM is the platform-agnostic virtual machine handle.
type VM interface {
	// Start boots the VM. Blocks until the VM is running or errors.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the VM.
	Stop(ctx context.Context) error

	// State returns the current lifecycle state.
	State() State

	// WaitForState blocks until the VM enters the target state or
	// the context is cancelled.
	WaitForState(ctx context.Context, target State) error
}
