//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/solcreek/dew/internal/vm"
)

type DarwinVM struct {
	cfg     vm.Config
	machine *vz.VirtualMachine
	mu      sync.RWMutex
	state   vm.State
}

func New(cfg vm.Config) (*DarwinVM, error) {
	if cfg.Kernel == "" {
		return nil, fmt.Errorf("dew: kernel path required")
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 512
	}
	if cfg.CmdLine == "" {
		cfg.CmdLine = "console=hvc0"
	}

	return &DarwinVM{cfg: cfg, state: vm.StateStopped}, nil
}

func (d *DarwinVM) buildConfig() (*vz.VirtualMachineConfiguration, error) {
	opts := []vz.LinuxBootLoaderOption{
		vz.WithCommandLine(d.cfg.CmdLine),
	}
	if d.cfg.Initrd != "" {
		opts = append(opts, vz.WithInitrd(d.cfg.Initrd))
	}
	bootLoader, err := vz.NewLinuxBootLoader(d.cfg.Kernel, opts...)
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}

	memBytes := d.cfg.MemoryMB * 1024 * 1024
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, d.cfg.CPUs, memBytes)
	if err != nil {
		return nil, fmt.Errorf("vm config: %w", err)
	}

	if err := d.configureSerial(config); err != nil {
		return nil, err
	}
	if err := d.configureDevices(config); err != nil {
		return nil, err
	}
	if err := d.configureNetwork(config); err != nil {
		return nil, err
	}
	if err := d.configureDisk(config); err != nil {
		return nil, err
	}
	if err := d.configureVsock(config); err != nil {
		return nil, err
	}
	if err := d.configureSharedDirs(config); err != nil {
		return nil, err
	}

	ok, err := config.Validate()
	if !ok || err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	return config, nil
}

func (d *DarwinVM) configureSerial(config *vz.VirtualMachineConfiguration) error {
	readFile := os.Stdin
	writeFile := os.Stdout
	if d.cfg.Console != nil {
		readFile = d.cfg.Console.In
		writeFile = d.cfg.Console.Out
	}
	serialAttach, err := vz.NewFileHandleSerialPortAttachment(readFile, writeFile)
	if err != nil {
		return fmt.Errorf("serial attach: %w", err)
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttach)
	if err != nil {
		return fmt.Errorf("serial config: %w", err)
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})
	return nil
}

func (d *DarwinVM) configureDevices(config *vz.VirtualMachineConfiguration) error {
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("entropy: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	memBalloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("memory balloon: %w", err)
	}
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{memBalloon})
	return nil
}

func (d *DarwinVM) configureNetwork(config *vz.VirtualMachineConfiguration) error {
	if !d.cfg.Network {
		return nil
	}
	natAttach, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return fmt.Errorf("nat attach: %w", err)
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttach)
	if err != nil {
		return fmt.Errorf("net config: %w", err)
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})
	return nil
}

func (d *DarwinVM) configureDisk(config *vz.VirtualMachineConfiguration) error {
	if d.cfg.DiskPath == "" {
		return nil
	}
	// Create disk image if it doesn't exist
	if _, err := os.Stat(d.cfg.DiskPath); os.IsNotExist(err) {
		size := int64(d.cfg.DiskGB) * 1024 * 1024 * 1024
		if size == 0 {
			size = 10 * 1024 * 1024 * 1024 // default 10GB
		}
		if err := vz.CreateDiskImage(d.cfg.DiskPath, size); err != nil {
			return fmt.Errorf("create disk: %w", err)
		}
	}
	attachment, err := vz.NewDiskImageStorageDeviceAttachment(d.cfg.DiskPath, false)
	if err != nil {
		return fmt.Errorf("disk attach: %w", err)
	}
	blockDev, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("block dev: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDev})
	return nil
}

func (d *DarwinVM) configureVsock(config *vz.VirtualMachineConfiguration) error {
	if d.cfg.VsockPort == 0 {
		return nil
	}
	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})
	return nil
}

func (d *DarwinVM) configureSharedDirs(config *vz.VirtualMachineConfiguration) error {
	if len(d.cfg.SharedDirs) == 0 {
		return nil
	}
	var fsDirs []vz.DirectorySharingDeviceConfiguration
	for _, sd := range d.cfg.SharedDirs {
		sharedDir, err := vz.NewSharedDirectory(sd.HostPath, sd.ReadOnly)
		if err != nil {
			return fmt.Errorf("shared dir %q: %w", sd.Tag, err)
		}
		share, err := vz.NewSingleDirectoryShare(sharedDir)
		if err != nil {
			return fmt.Errorf("dir share %q: %w", sd.Tag, err)
		}
		fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration(sd.Tag)
		if err != nil {
			return fmt.Errorf("fs config %q: %w", sd.Tag, err)
		}
		fsConfig.SetDirectoryShare(share)
		fsDirs = append(fsDirs, fsConfig)
	}
	config.SetDirectorySharingDevicesVirtualMachineConfiguration(fsDirs)
	return nil
}

func (d *DarwinVM) Start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.state != vm.StateStopped {
		return fmt.Errorf("dew: cannot start VM in state %s", d.state)
	}
	d.state = vm.StateStarting

	config, err := d.buildConfig()
	if err != nil {
		d.state = vm.StateError
		return fmt.Errorf("dew: %w", err)
	}

	machine, err := vz.NewVirtualMachine(config)
	if err != nil {
		d.state = vm.StateError
		return fmt.Errorf("dew: create vm: %w", err)
	}
	d.machine = machine

	if err := machine.Start(); err != nil {
		d.state = vm.StateError
		// Don't prefix with "dew:" — caller will add it
		return fmt.Errorf("VM start failed: %w", err)
	}

	d.state = vm.StateRunning
	return nil
}

func (d *DarwinVM) Stop(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.machine == nil {
		return nil
	}
	d.state = vm.StateStopping

	if d.machine.CanRequestStop() {
		if ok, err := d.machine.RequestStop(); !ok || err != nil {
			return fmt.Errorf("dew: stop: %w", err)
		}
	}

	d.state = vm.StateStopped
	return nil
}

func (d *DarwinVM) State() vm.State {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

func (d *DarwinVM) VsockConnect(port uint32) (net.Conn, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.machine == nil {
		return nil, fmt.Errorf("dew: VM not running")
	}
	devices := d.machine.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("dew: VM has no vsock devices")
	}
	return devices[0].Connect(port)
}

func (d *DarwinVM) WaitForState(ctx context.Context, target vm.State) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if d.State() == target {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
