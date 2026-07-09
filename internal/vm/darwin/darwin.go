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

// diskCmdLine appends the dew.disk=1 marker to the kernel command line when a
// data disk is attached (diskPath != ""), so the guest's init waits for
// /dev/vda only when one will actually appear. A diskless profile (minimal —
// `dew run`'s default) otherwise burns init's full device-probe timeout (~1s)
// every boot waiting for a /dev/vda that never comes. Kept in lockstep with
// configureDisk, which attaches the block device under this same condition.
//
// Idempotent: a base that already carries the marker is returned unchanged, so
// the marker can never appear twice in /proc/cmdline.
func diskCmdLine(base, diskPath string) string {
	if diskPath == "" {
		return base
	}
	// Match the marker as a whole cmdline token, not a substring: a
	// strings.Contains check would false-positive on a superset like
	// "dew.disk=10" and wrongly skip appending the real "dew.disk=1",
	// disabling the guest's /dev/vda wait gate for a disk profile.
	for _, tok := range strings.Fields(base) {
		if tok == "dew.disk=1" {
			return base
		}
	}
	return base + " dew.disk=1"
}

func (d *DarwinVM) buildConfig() (*vz.VirtualMachineConfiguration, error) {
	opts := []vz.LinuxBootLoaderOption{
		vz.WithCommandLine(diskCmdLine(d.cfg.CmdLine, d.cfg.DiskPath)),
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
	// Early 26.x builds regressed it hard: VMs booted and got a
	// 192.168.64.x address, but ALL outbound (ICMP/DNS/TCP) timed out.
	// Apple has since (at least partially) repaired it — on 26.5.1 the
	// guest reaches the public internet fine, including DNS+TLS to real
	// hosts (verified: registry.npmjs.org and api.anthropic.com both
	// reachable). We don't know the exact build the fix landed in, so we
	// can't gate on a precise version boundary. The proper long-term fix
	// is VZVmnetNetworkDeviceAttachment (Code-Hex/vz#205, #218); when that
	// ships and we update the dep we'll switch the implementation here.
	//
	// Until then we keep a *conditional* heads-up on macOS 26 — not a
	// claim of total failure (that's empirically false on current builds),
	// just enough that a user hitting a real "connection timeout" on an
	// affected build knows it's a known VZ issue, not their config. A
	// runtime reachability probe was considered and rejected: it would add
	// a per-run exec round-trip and would itself false-positive under
	// --network-policy=restricted, on an offline host, or when the probe
	// target is down.
	//
	// The wording is deliberately hedged: the most common "network broken
	// right at boot" symptom was NOT the NAT but a boot-time race — the guest
	// command running before the backgrounded DHCP lease landed — which
	// `dew run` now closes with a lease barrier (see waitGuestNetwork in
	// cmd/dew). So a failure that survives the barrier is genuinely more
	// likely a real regressed build; don't over-claim it as certain.
	if host := readHostInfo(); strings.HasPrefix(host.OSVersion, "26.") {
		fmt.Fprintln(os.Stderr,
			"  ⚠ some early macOS 26 builds had an Apple VZ NAT regression (guest gets a 192.168.64.x address but outbound times out); later builds repaired it.")
		fmt.Fprintln(os.Stderr,
			"    dew waits for the guest's DHCP lease before running your command, so a fast failure at boot is usually not this.")
		fmt.Fprintln(os.Stderr,
			"    If outbound still times out (curl/apk/npm hang) it may be the VZ issue — see Code-Hex/vz#218. Workarounds: --share <hostdir> for host files, vsock for host services.")
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
	// invalid" — config.Validate() refuses an attachment without explicit
	// caching + sync metadata. The WithCacheAndSync variant (macOS 12+) lets
	// us pass both; dew already requires macOS 13+ (checked in doctor), so the
	// availability gate is always satisfied.
	//
	// We pick Cached + Fsync, not the default Automatic + Full. Full sync maps
	// to F_FULLFSYNC on macOS/APFS, which forces every guest flush all the way
	// to physical media — heavy fsync workloads (image-layer unpack, npm
	// install, db writes, first-boot rootfs populate) crawl (~350 KB/s even on
	// a 17 MB/s link). Fsync uses a regular fsync() — data stays safe against a
	// guest crash; only an abrupt host power loss risks the last unflushed
	// writes — and Cached lets the host page cache absorb the writes. Measured
	// ~22× faster (350 KB/s → 7.8 MB/s). This is the standard dev-VM trade-off
	// (Lima/Colima do the same).
	attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		d.cfg.DiskPath, false,
		vz.DiskImageCachingModeCached,
		vz.DiskImageSynchronizationModeFsync,
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

// vsockConnectTimeout bounds a single vz socket connect. vz's
// VirtioSocketDevice.Connect never returns when the guest has no vsock
// transport at all (e.g. the virtio module failed to load): the
// completion handler simply never fires, so without a bound here every
// caller up the stack hangs forever. A healthy guest accepts or refuses
// within milliseconds; 5s is generous.
const vsockConnectTimeout = 5 * time.Second

func (d *DarwinVM) VsockConnect(port uint32) (net.Conn, error) {
	// Snapshot the machine under RLock but do NOT hold the lock across
	// the blocking Connect call: an abandoned connect goroutine that
	// held the RLock would deadlock Stop (which needs the write lock).
	d.mu.RLock()
	machine := d.machine
	d.mu.RUnlock()

	if machine == nil {
		return nil, fmt.Errorf("dew: VM not running")
	}
	devices := machine.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("dew: VM has no vsock devices")
	}
	return connectWithTimeout(func() (net.Conn, error) {
		conn, err := devices[0].Connect(port)
		if err != nil {
			// Return a true nil interface: passing the typed-nil
			// *VirtioSocketConnection through net.Conn makes
			// `conn != nil` true for callers and Close() panics.
			return nil, err
		}
		return conn, nil
	}, vsockConnectTimeout)
}

// VsockListen returns a host-side listener for guest-initiated vsock
// connections on port (the reverse of VsockConnect). The returned
// *vz.VirtioSocketListener implements net.Listener; the reverse host-forward
// accepts on it, reads a ReverseDialRequest, and proxies to 127.0.0.1.
func (d *DarwinVM) VsockListen(port uint32) (net.Listener, error) {
	d.mu.RLock()
	machine := d.machine
	d.mu.RUnlock()

	if machine == nil {
		return nil, fmt.Errorf("dew: VM not running")
	}
	devices := machine.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("dew: VM has no vsock devices")
	}
	return devices[0].Listen(port)
}

// connectWithTimeout runs dial in a goroutine and abandons it after
// timeout. The abandoned goroutine holds no locks; if the dial ever
// completes late, its conn is closed so the fd doesn't leak.
func connectWithTimeout(dial func() (net.Conn, error), timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := dial()
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
		return nil, fmt.Errorf("vsock connect: guest did not respond within %s", timeout)
	}
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
