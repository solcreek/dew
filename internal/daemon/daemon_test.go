//go:build darwin

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSocketDir(t *testing.T) {
	dir := SocketDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "dew")
	if dir != want {
		t.Errorf("SocketDir() = %q, want %q", dir, want)
	}
}

func TestSocketPath(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"", "default.sock"},
		{"myvm", "myvm.sock"},
	}
	for _, tt := range tests {
		got := SocketPath(tt.name)
		base := filepath.Base(got)
		if base != tt.want {
			t.Errorf("SocketPath(%q) base = %q, want %q", tt.name, base, tt.want)
		}
	}
}
