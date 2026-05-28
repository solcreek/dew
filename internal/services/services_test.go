package services

import (
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		name    string
		wantNil bool
		port    int
	}{
		{"postgres", false, 5432},
		{"redis", false, 6379},
		{"mysql", false, 3306},
		{"mongo", false, 27017},
		{"minio", false, 9000},
		{"doesnotexist", true, 0},
	}
	for _, tt := range tests {
		s := Lookup(tt.name)
		if tt.wantNil {
			if s != nil {
				t.Errorf("Lookup(%q) should be nil", tt.name)
			}
			continue
		}
		if s == nil {
			t.Errorf("Lookup(%q) = nil", tt.name)
			continue
		}
		if s.Port != tt.port {
			t.Errorf("Lookup(%q).Port = %d, want %d", tt.name, s.Port, tt.port)
		}
	}
}

func TestNerdctlRunCmd(t *testing.T) {
	s := Registry["postgres"]
	cmd := NerdctlRunCmd(s)
	if !strings.Contains(cmd, "--net=host") {
		t.Errorf("missing --net=host: %q", cmd)
	}
	if !strings.Contains(cmd, "POSTGRES_PASSWORD=dew") {
		t.Errorf("missing env: %q", cmd)
	}
	if !strings.Contains(cmd, "postgres:16-alpine") {
		t.Errorf("missing image: %q", cmd)
	}
}

func TestNames(t *testing.T) {
	names := Names()
	if len(names) < 5 {
		t.Errorf("Names() = %v, want at least 5", names)
	}
}
