//go:build darwin

package main

import (
	"encoding/base64"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

func TestParseShare(t *testing.T) {
	tests := []struct {
		input   string
		want    vm.SharedDir
		wantErr bool
	}{
		{
			input: "app:/tmp/myapp",
			want:  vm.SharedDir{Tag: "app", HostPath: "/tmp/myapp", ReadOnly: true},
		},
		{
			input: "src:/Users/me/project:ro",
			want:  vm.SharedDir{Tag: "src", HostPath: "/Users/me/project", ReadOnly: true},
		},
		{
			input: "data:/tmp/data:rw",
			want:  vm.SharedDir{Tag: "data", HostPath: "/tmp/data", ReadOnly: false},
		},
		{
			input:   "nocolon",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		got, err := parseShare(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseShare(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseShare(%q) error: %v", tt.input, err)
			continue
		}
		if got.Tag != tt.want.Tag {
			t.Errorf("parseShare(%q).Tag = %q, want %q", tt.input, got.Tag, tt.want.Tag)
		}
		if got.HostPath != tt.want.HostPath {
			t.Errorf("parseShare(%q).HostPath = %q, want %q", tt.input, got.HostPath, tt.want.HostPath)
		}
		if got.ReadOnly != tt.want.ReadOnly {
			t.Errorf("parseShare(%q).ReadOnly = %v, want %v", tt.input, got.ReadOnly, tt.want.ReadOnly)
		}
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	cfg, remaining, err := parseFlags([]string{"--kernel", "/tmp/vmlinuz"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kernel != "/tmp/vmlinuz" {
		t.Errorf("Kernel = %q", cfg.Kernel)
	}
	if cfg.CPUs != 1 {
		t.Errorf("CPUs = %d, want 1", cfg.CPUs)
	}
	if cfg.MemoryMB != 512 {
		t.Errorf("MemoryMB = %d, want 512", cfg.MemoryMB)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining = %v, want empty", remaining)
	}
}

func TestParseFlags_AllFlags(t *testing.T) {
	cfg, _, err := parseFlags([]string{
		"--kernel", "/k",
		"--initrd", "/i",
		"--cpus", "4",
		"--memory", "2048",
		"--network",
		"--vsock", "1024",
		"--share", "app:/tmp/app:ro",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kernel != "/k" {
		t.Errorf("Kernel = %q", cfg.Kernel)
	}
	if cfg.Initrd != "/i" {
		t.Errorf("Initrd = %q", cfg.Initrd)
	}
	if cfg.CPUs != 4 {
		t.Errorf("CPUs = %d", cfg.CPUs)
	}
	if cfg.MemoryMB != 2048 {
		t.Errorf("MemoryMB = %d", cfg.MemoryMB)
	}
	if !cfg.Network {
		t.Error("Network should be true")
	}
	if cfg.VsockPort != 1024 {
		t.Errorf("VsockPort = %d", cfg.VsockPort)
	}
	if len(cfg.SharedDirs) != 1 || cfg.SharedDirs[0].Tag != "app" {
		t.Errorf("SharedDirs = %v", cfg.SharedDirs)
	}
}

func TestParseFlags_DoubleDash(t *testing.T) {
	cfg, remaining, err := parseFlags([]string{
		"--kernel", "/k", "--", "ls", "-la",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kernel != "/k" {
		t.Errorf("Kernel = %q", cfg.Kernel)
	}
	if len(remaining) != 2 || remaining[0] != "ls" || remaining[1] != "-la" {
		t.Errorf("remaining = %v, want [ls -la]", remaining)
	}
}

func TestParseFlags_NoDash(t *testing.T) {
	cfg, remaining, err := parseFlags([]string{
		"--kernel", "/k", "echo", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kernel != "/k" {
		t.Errorf("Kernel = %q", cfg.Kernel)
	}
	if len(remaining) != 2 || remaining[0] != "echo" || remaining[1] != "hello" {
		t.Errorf("remaining = %v, want [echo hello]", remaining)
	}
}

func TestParseFlags_UnknownFlag(t *testing.T) {
	_, _, err := parseFlags([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseFlags_Forward(t *testing.T) {
	cfg, _, err := parseFlags([]string{
		"--kernel", "/k", "--forward", "3000:8080", "--forward", "5432:5432",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Forwards) != 2 {
		t.Fatalf("Forwards len = %d, want 2", len(cfg.Forwards))
	}
	if cfg.Forwards[0].HostPort != 3000 || cfg.Forwards[0].GuestPort != 8080 {
		t.Errorf("Forwards[0] = %+v, want 3000:8080", cfg.Forwards[0])
	}
	if cfg.Forwards[1].HostPort != 5432 || cfg.Forwards[1].GuestPort != 5432 {
		t.Errorf("Forwards[1] = %+v, want 5432:5432", cfg.Forwards[1])
	}
}

func TestParseForward(t *testing.T) {
	tests := []struct {
		input   string
		want    vm.PortForward
		wantErr bool
	}{
		{"3000:8080", vm.PortForward{HostPort: 3000, GuestPort: 8080}, false},
		{"5432:5432", vm.PortForward{HostPort: 5432, GuestPort: 5432}, false},
		{"80:80", vm.PortForward{HostPort: 80, GuestPort: 80}, false},
		{"nocolon", vm.PortForward{}, true},
		{"abc:123", vm.PortForward{}, true},
		{"0:80", vm.PortForward{}, true},
		{":80", vm.PortForward{}, true},
	}
	for _, tt := range tests {
		got, err := parseForward(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseForward(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseForward(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseForward(%q) = %+v, want %+v", tt.input, got, tt.want)
		}
	}
}

func TestBase64Encode(t *testing.T) {
	tests := []struct {
		input string
	}{
		{"echo hello"},
		{"dew-httpd 9999"},
		{"mkdir -p /tmp/www && httpd -p 80 -h /tmp/www"},
		{""},
	}
	for _, tt := range tests {
		encoded := base64Encode(tt.input)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("base64Encode(%q) produced invalid base64: %v", tt.input, err)
			continue
		}
		if string(decoded) != tt.input {
			t.Errorf("round-trip failed: got %q, want %q", decoded, tt.input)
		}
	}
}
