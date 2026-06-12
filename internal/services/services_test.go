package services

import (
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

func TestNames(t *testing.T) {
	names := Names()
	if len(names) < 5 {
		t.Errorf("Names() = %v, want at least 5", names)
	}
}
