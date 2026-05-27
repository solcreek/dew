package vm

import "testing"

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
