//go:build darwin

package darwin

import (
	"context"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vm"
)

// dew.disk=1 must be appended exactly when a data disk is attached, so the
// guest waits for /dev/vda only then — a diskless profile (minimal) otherwise
// burns init's ~1s device-probe timeout every boot. In lockstep with
// configureDisk (attaches iff DiskPath != "").
func TestDiskCmdLine(t *testing.T) {
	const base = "console=hvc0"
	if got := diskCmdLine(base, ""); got != base {
		t.Errorf("diskless: diskCmdLine(%q, \"\") = %q, want unchanged", base, got)
	}
	if got, want := diskCmdLine(base, "/x/node.img"), base+" dew.disk=1"; got != want {
		t.Errorf("with disk: diskCmdLine = %q, want %q", got, want)
	}
	// Preserves existing cmdline flags; the marker is appended after them.
	withShare := "console=hvc0 dew.share=oci-stage:/oci-stage"
	if got, want := diskCmdLine(withShare, "/x/std.img"), withShare+" dew.disk=1"; got != want {
		t.Errorf("preserves existing flags: got %q, want %q", got, want)
	}
	// Idempotent: a base that already carries the exact marker isn't given a second.
	withMarker := "console=hvc0 dew.disk=1"
	if got := diskCmdLine(withMarker, "/x/std.img"); got != withMarker {
		t.Errorf("idempotent: diskCmdLine(%q, disk) = %q, want unchanged (no duplicate marker)", withMarker, got)
	}
	// A superset token (dew.disk=10) is NOT the marker — whole-token matching
	// must still append the real "dew.disk=1" rather than false-positive on it.
	withSuperset := "console=hvc0 dew.disk=10"
	if got, want := diskCmdLine(withSuperset, "/x/std.img"), withSuperset+" dew.disk=1"; got != want {
		t.Errorf("superset token false-positive: diskCmdLine(%q, disk) = %q, want %q", withSuperset, got, want)
	}
}

func TestNew_KernelRequired(t *testing.T) {
	_, err := New(vm.Config{})
	if err == nil {
		t.Fatal("expected error for empty kernel path")
	}
}

func TestNew_Defaults(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.CPUs != 1 {
		t.Errorf("CPUs = %d, want 1", d.cfg.CPUs)
	}
	if d.cfg.MemoryMB != 512 {
		t.Errorf("MemoryMB = %d, want 512", d.cfg.MemoryMB)
	}
	if d.cfg.CmdLine != "console=hvc0" {
		t.Errorf("CmdLine = %q, want %q", d.cfg.CmdLine, "console=hvc0")
	}
}

func TestNew_CustomValues(t *testing.T) {
	d, err := New(vm.Config{
		Kernel:   "/tmp/vmlinuz",
		CPUs:     4,
		MemoryMB: 2048,
		CmdLine:  "console=hvc0 quiet",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", d.cfg.CPUs)
	}
	if d.cfg.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d, want 2048", d.cfg.MemoryMB)
	}
	if d.cfg.CmdLine != "console=hvc0 quiet" {
		t.Errorf("CmdLine = %q, want %q", d.cfg.CmdLine, "console=hvc0 quiet")
	}
}

func TestNew_InitialState(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	if d.State() != vm.StateStopped {
		t.Errorf("initial state = %v, want stopped", d.State())
	}
}

func TestStart_InvalidKernelPath(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/nonexistent/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent kernel")
	}
	if d.State() != vm.StateError {
		t.Errorf("state after failed start = %v, want error", d.State())
	}
}

func TestStart_DoubleStartRejected(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state = vm.StateRunning
	d.mu.Unlock()

	err = d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for double start")
	}
}

func TestStop_NilMachine(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("Stop on nil machine should not error: %v", err)
	}
}

func TestWaitForState_AlreadyInState(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := d.WaitForState(ctx, vm.StateStopped); err != nil {
		t.Errorf("WaitForState for current state should return nil: %v", err)
	}
}

func TestWaitForState_ContextCancelled(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = d.WaitForState(ctx, vm.StateRunning)
	if err == nil {
		t.Error("expected context deadline error")
	}
}

func TestNew_WithVsock(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz", VsockPort: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.VsockPort != 1024 {
		t.Errorf("VsockPort = %d, want 1024", d.cfg.VsockPort)
	}
}

func TestNew_WithSharedDirs(t *testing.T) {
	d, err := New(vm.Config{
		Kernel: "/tmp/vmlinuz",
		SharedDirs: []vm.SharedDir{
			{Tag: "src", HostPath: "/Users/me/project", ReadOnly: true},
			{Tag: "data", HostPath: "/tmp/data"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.cfg.SharedDirs) != 2 {
		t.Fatalf("SharedDirs len = %d, want 2", len(d.cfg.SharedDirs))
	}
	if d.cfg.SharedDirs[0].Tag != "src" {
		t.Errorf("SharedDirs[0].Tag = %q, want %q", d.cfg.SharedDirs[0].Tag, "src")
	}
	if !d.cfg.SharedDirs[0].ReadOnly {
		t.Error("SharedDirs[0].ReadOnly should be true")
	}
	if d.cfg.SharedDirs[1].ReadOnly {
		t.Error("SharedDirs[1].ReadOnly should be false")
	}
}

func TestVsockConnect_NilMachine(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.VsockConnect(1024)
	if err == nil {
		t.Fatal("expected error for VsockConnect on nil machine")
	}
}

func TestNew_WithNetwork(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz", Network: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d.cfg.Network {
		t.Error("Network should be true")
	}
}

func TestNew_WithForwards(t *testing.T) {
	d, err := New(vm.Config{
		Kernel: "/tmp/vmlinuz",
		Forwards: []vm.PortForward{
			{HostPort: 3000, GuestPort: 8080},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.cfg.Forwards) != 1 {
		t.Fatalf("Forwards len = %d, want 1", len(d.cfg.Forwards))
	}
}

func TestNew_WithConsole(t *testing.T) {
	console, hostReader, hostWriter, err := vm.NewConsolePipe()
	if err != nil {
		t.Fatal(err)
	}
	defer hostReader.Close()
	defer hostWriter.Close()
	defer console.In.Close()
	defer console.Out.Close()

	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz", Console: console})
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.Console == nil {
		t.Error("Console should be set")
	}
}

func TestStop_Idempotent(t *testing.T) {
	d, err := New(vm.Config{Kernel: "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}
