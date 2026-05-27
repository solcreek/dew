package vm

import (
	"io"
	"testing"
)

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateStopped, "stopped"},
		{StateStarting, "starting"},
		{StateRunning, "running"},
		{StatePaused, "paused"},
		{StateStopping, "stopping"},
		{StateError, "error"},
		{State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	if cfg.CPUs != 0 {
		t.Errorf("zero value CPUs = %d, want 0", cfg.CPUs)
	}
	if cfg.MemoryMB != 0 {
		t.Errorf("zero value MemoryMB = %d, want 0", cfg.MemoryMB)
	}
	if cfg.VsockPort != 0 {
		t.Errorf("zero value VsockPort = %d, want 0", cfg.VsockPort)
	}
	if cfg.SharedDirs != nil {
		t.Error("zero value SharedDirs should be nil")
	}
}

func TestSharedDir(t *testing.T) {
	sd := SharedDir{Tag: "app", HostPath: "/tmp/myapp", ReadOnly: true}
	if sd.Tag != "app" {
		t.Errorf("Tag = %q, want %q", sd.Tag, "app")
	}
	if sd.HostPath != "/tmp/myapp" {
		t.Errorf("HostPath = %q, want %q", sd.HostPath, "/tmp/myapp")
	}
	if !sd.ReadOnly {
		t.Error("ReadOnly should be true")
	}
}

func TestPortForward(t *testing.T) {
	pf := PortForward{HostPort: 3000, GuestPort: 8080}
	if pf.HostPort != 3000 {
		t.Errorf("HostPort = %d", pf.HostPort)
	}
	if pf.GuestPort != 8080 {
		t.Errorf("GuestPort = %d", pf.GuestPort)
	}
}

func TestConfigForwards(t *testing.T) {
	cfg := Config{
		Forwards: []PortForward{
			{HostPort: 3000, GuestPort: 3000},
			{HostPort: 5432, GuestPort: 5432},
		},
	}
	if len(cfg.Forwards) != 2 {
		t.Fatalf("Forwards len = %d, want 2", len(cfg.Forwards))
	}
}

func TestNewConsolePipe(t *testing.T) {
	console, hostReader, hostWriter, err := NewConsolePipe()
	if err != nil {
		t.Fatal(err)
	}
	defer hostReader.Close()
	defer hostWriter.Close()
	defer console.In.Close()
	defer console.Out.Close()

	// Write to hostWriter → should be readable from console.In
	msg := []byte("hello from host")
	go func() {
		hostWriter.Write(msg)
		hostWriter.Close()
	}()
	buf := make([]byte, len(msg))
	n, err := io.ReadFull(console.In, buf)
	if err != nil {
		t.Fatalf("read from console.In: %v", err)
	}
	if string(buf[:n]) != "hello from host" {
		t.Errorf("got %q, want %q", buf[:n], "hello from host")
	}
}

func TestNewConsolePipe_GuestToHost(t *testing.T) {
	console, hostReader, hostWriter, err := NewConsolePipe()
	if err != nil {
		t.Fatal(err)
	}
	defer hostWriter.Close()
	defer console.In.Close()

	// Write to console.Out → should be readable from hostReader
	msg := []byte("hello from guest")
	go func() {
		console.Out.Write(msg)
		console.Out.Close()
	}()
	buf := make([]byte, len(msg))
	n, err := io.ReadFull(hostReader, buf)
	if err != nil {
		t.Fatalf("read from hostReader: %v", err)
	}
	if string(buf[:n]) != "hello from guest" {
		t.Errorf("got %q, want %q", buf[:n], "hello from guest")
	}
}
