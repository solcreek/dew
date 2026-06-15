package services

import (
	"strings"
	"testing"
)

func TestConnString(t *testing.T) {
	tests := []struct {
		name string
		port int
		want string
	}{
		{"postgres", 5432, "postgresql://postgres:dew@127.0.0.1:5432/dew"},
		{"mysql", 13306, "mysql://root:dew@127.0.0.1:13306/dew"},
		{"redis", 6379, "redis://127.0.0.1:6379"},
		{"mongo", 27017, "mongodb://127.0.0.1:27017"},
		{"minio", 9000, "http://dew:dewpassword@127.0.0.1:9000"},
	}
	for _, tt := range tests {
		if got := ConnString(Registry[tt.name], tt.port); got != tt.want {
			t.Errorf("ConnString(%s, %d) = %q, want %q", tt.name, tt.port, got, tt.want)
		}
	}
}

func TestConnString_UsesActualPort(t *testing.T) {
	if got := ConnString(Registry["postgres"], 55432); !strings.Contains(got, ":55432/") {
		t.Errorf("ConnString did not use fallback port: %q", got)
	}
}

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

func TestListenProbeCmd(t *testing.T) {
	cmd := ListenProbeCmd(3306) // 3306 == 0x0CEA
	for _, want := range []string{"0CEA", "0A", "/proc/net/tcp"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ListenProbeCmd missing %q: %q", want, cmd)
		}
	}
}

func TestLogTailCmd(t *testing.T) {
	cmd := LogTailCmd("postgres", 20)
	for _, want := range []string{"tail -n 20", "/var/log/dew-oci-postgres.log"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("LogTailCmd missing %q: %q", want, cmd)
		}
	}
}

func TestNames(t *testing.T) {
	names := Names()
	if len(names) < 5 {
		t.Errorf("Names() = %v, want at least 5", names)
	}
}
