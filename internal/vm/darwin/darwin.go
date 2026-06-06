//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/solcreek/dew/internal/vm"
)

// rosettaTag is the virtiofs mount tag the guest's init-stage2 looks for
// to find Apple's Rosetta translator binary. Shared between the reserved-tag
// guard here and the arm64 Rosetta share builder, and mirrored by the guest
// init script (initramfs/build.sh).
const rosettaTag = "rosetta"

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
	// macOS 26 deprecated the legacy NAT attachment we're calling here.
	// VMs still boot, the guest gets a 192.168.64.x address, and the
	// host gateway is reachable — but ALL outbound traffic times out
	// (ICMP/DNS/TCP all fail to reach the public internet). Replacement
	// is VZVmnetNetworkDeviceAttachment (Apple docs: VZVmnet...). The
	// Code-Hex/vz library has open PRs adding it (#205, #218) but
	// hasn't shipped. When that lands and we update the dep, we'll
	// switch the implementation under here. Until then the VM boots
	// and works for everything not requiring outbound (host shares
	// via --share, vsock to the host, etc.). Warn on stderr so users
	// hitting "curl: connection timeout" know it's not their fault.
	if host := readHostInfo(); strings.HasPrefix(host.OSVersion, "26.") {
		fmt.Fprintln(os.Stderr,
			"  ⚠ Apple VZ NAT is broken on macOS 26 — guest outbound network won't reach the internet.")
		fmt.Fprintln(os.Stderr,
			"    Tracking upstream Code-Hex/vz#218 (VZVmnetNetworkDeviceAttachment).")
		fmt.Fprintln(os.Stderr,
			"    Workarounds: use --share <hostdir> for host files, vsock for host services.")
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
	// macOS 26 VZ rejects the basic NewDiskImageStorageDeviceAttachment
	// constructor with VZErrorDomain Code=2 "storage device attachment is
	// invalid" — config.Validate() refuses an attachment without
	// explicit caching + sync metadata. The WithCacheAndSync variant
	// (macOS 12+) lets us pass both. We pick the conservative defaults:
	//   - Automatic caching: lets the host decide, matches pre-macOS-26 behavior
	//   - Full synchronization: every guest flush is an fsync — safest,
	//     matches what the basic constructor implicitly did before
	// dew already requires macOS 13+ (checked in doctor), so the macOS 12
	// availability gate of WithCacheAndSync is always satisfied.
	attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		d.cfg.DiskPath, false,
		vz.DiskImageCachingModeAutomatic,
		vz.DiskImageSynchronizationModeFull,
	)
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
	var fsDirs []vz.DirectorySharingDeviceConfiguration
	for _, sd := range d.cfg.SharedDirs {
		if d.cfg.EnableRosetta && sd.Tag == rosettaTag {
			return fmt.Errorf("shared dir tag %q is reserved for the Rosetta share when --rosetta is set", sd.Tag)
		}
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
	if d.cfg.EnableRosetta {
		rosettaDev, err := rosettaShareDevice()
		if err != nil {
			return fmt.Errorf("rosetta: %w", err)
		}
		fsDirs = append(fsDirs, rosettaDev)
	}
	if len(fsDirs) == 0 {
		return nil
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

	// Diagnostic dump: lets bug reports show exactly what we sent to
	// Apple Virtualization, since VZErrorDomain Code=1 rarely surfaces
	// the actual cause. Opt-in via DEW_DEBUG=1 so normal users aren't
	// noised; `dew doctor --verbose` sets it automatically.
	if os.Getenv("DEW_DEBUG") != "" {
		dumpConfigSummary(os.Stderr, d.cfg)
	}

	if err := machine.Start(); err != nil {
		d.state = vm.StateError
		// Don't prefix with "dew:" — caller will add it.
		// On boot failure also append the host model so bug reports
		// surface hardware/OS specifics without needing the user to
		// remember sysctl. Cheap; only runs on the cold-error path.
		host := readHostInfo()
		hint := ""
		// The "storage device attachment is invalid" error on macOS 26
		// fires when an existing disk image file was created by older
		// VZ versions in a format the new VZ rejects. Fresh images
		// work fine. The user has to delete the offending file to
		// recover (destructive — loses VM state).
		if d.cfg.DiskPath != "" &&
			strings.Contains(err.Error(), "storage device attachment is invalid") {
			hint = fmt.Sprintf(
				"\n\n  This usually means the disk image at\n"+
					"    %s\n"+
					"  was created by an older macOS/VZ combination and is no longer\n"+
					"  accepted by the current VZ. To recover, delete it (resets VM state):\n"+
					"    rm %s",
				d.cfg.DiskPath, d.cfg.DiskPath)
		}
		return fmt.Errorf("VM start failed on %s macOS %s: %w%s",
			strNonEmpty(host.Model, "<unknown>"),
			strNonEmpty(host.OSVersion, "<unknown>"), err, hint)
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
