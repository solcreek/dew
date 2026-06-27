//go:build darwin

package main

import (
	"testing"

	"github.com/solcreek/dew/internal/dewfile"
)

func TestExposeHostPorts(t *testing.T) {
	df := func(ports ...int) *dewfile.File {
		f := &dewfile.File{}
		f.Host.Expose = ports
		return f
	}
	eq := func(a, b []int) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	tests := []struct {
		name  string
		flags []int
		df    *dewfile.File
		want  []int
	}{
		{"empty", nil, nil, nil},
		{"flags-only-sorted", []int{50051, 5432}, nil, []int{5432, 50051}},
		{"toml-only", nil, df(6379), []int{6379}},
		{"merge-dedup-sort", []int{50051}, df(5432, 50051), []int{5432, 50051}},
		{"out-of-range-dropped", []int{0, 70000, 8080}, nil, []int{8080}},
		{"nil-df-ok", []int{3000}, nil, []int{3000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exposeHostPorts(tt.flags, tt.df)
			if !eq(got, tt.want) {
				t.Errorf("exposeHostPorts(%v, %v) = %v, want %v", tt.flags, tt.df, got, tt.want)
			}
		})
	}
}

// The --expose-host flag must parse a valid port and reject junk.
func TestParseFlags_ExposeHost(t *testing.T) {
	cfg, _, err := parseFlags([]string{"--expose-host", "50051", "--expose-host", "5432"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	_ = cfg
	if len(flagExposeHost) != 2 || flagExposeHost[0] != 50051 || flagExposeHost[1] != 5432 {
		t.Errorf("flagExposeHost = %v, want [50051 5432]", flagExposeHost)
	}
	if _, _, err := parseFlags([]string{"--expose-host", "0"}); err == nil {
		t.Error("expected error for out-of-range --expose-host 0")
	}
	if _, _, err := parseFlags([]string{"--expose-host", "notaport"}); err == nil {
		t.Error("expected error for non-numeric --expose-host")
	}
}
