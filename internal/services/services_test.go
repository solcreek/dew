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
	// Must match the LISTEN state (0A) for the hex port on BOTH the IPv4 and
	// IPv6 stacks — dual-stack [::]:port services only appear in tcp6.
	for _, want := range []string{"0CEA", "0A", "/proc/net/tcp", "/proc/net/tcp6"} {
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

func TestHostInternalPorts(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []int
	}{
		{"none", []string{"REDIS_URL=redis://localhost:6379/0"}, nil},
		{"simple", []string{"ANYCABLE_RPC_HOST=host.internal:50051"}, []int{50051}},
		{"dew-alias", []string{"RPC=host.dew.internal:50051"}, []int{50051}},
		{"in-url", []string{"DB=postgres://host.internal:5432/app"}, []int{5432}},
		{"bare-no-port", []string{"HOST=host.internal"}, []int{0}},
		{
			"multiple-distinct-sorted",
			[]string{"A=host.internal:8080", "B=host.dew.internal:50051", "C=host.internal:8080"},
			[]int{8080, 50051},
		},
		{"out-of-range-ignored", []string{"X=host.internal:99999"}, nil},
		{"two-refs-one-value", []string{"X=host.internal:1 host.internal:2"}, []int{1, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HostInternalPorts(tt.env)
			if len(got) != len(tt.want) {
				t.Fatalf("HostInternalPorts(%v) = %v, want %v", tt.env, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("HostInternalPorts(%v) = %v, want %v", tt.env, got, tt.want)
				}
			}
		})
	}
}

func TestHostServiceHint(t *testing.T) {
	if got := HostServiceHint("anycable", nil); got != "" {
		t.Errorf("HostServiceHint with no ports = %q, want empty", got)
	}
	got := HostServiceHint("anycable", []int{50051})
	for _, want := range []string{"anycable", "host.internal:50051", "0.0.0.0", "127.0.0.1"} {
		if !strings.Contains(got, want) {
			t.Errorf("HostServiceHint missing %q: %q", want, got)
		}
	}
	// A portless reference names the bare alias, not "host.internal:0".
	bare := HostServiceHint("svc", []int{0})
	if strings.Contains(bare, ":0") || !strings.Contains(bare, "host.internal") {
		t.Errorf("portless hint malformed: %q", bare)
	}
}

func TestNames(t *testing.T) {
	names := Names()
	if len(names) < 5 {
		t.Errorf("Names() = %v, want at least 5", names)
	}
}
