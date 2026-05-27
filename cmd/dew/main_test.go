//go:build darwin

package main

import (
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
			want:  vm.SharedDir{Tag: "app", HostPath: "/tmp/myapp"},
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
