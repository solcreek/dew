// Package vm defines the platform-agnostic interface for Dew's
// virtual machine layer.
//
// macOS: Apple Virtualization.framework via Code-Hex/vz
// Linux: no VM needed — containerd runs natively
package vm

import "context"

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
