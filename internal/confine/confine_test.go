package confine

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_HardenedUnit(t *testing.T) {
	unit := `
[Unit]
Description=hardened gateway

[Service]
# the real artifact's hardening
DynamicUser=yes
CapabilityBoundingSet=
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/app
SystemCallFilter=@system-service
MemoryDenyWriteExecute=yes
MemoryMax=256M
TasksMax=64
CPUQuota=200%
Restart=on-failure

[Install]
WantedBy=multi-user.target
`
	p, err := Parse(strings.NewReader(unit))
	if err != nil {
		t.Fatal(err)
	}
	if p.MemoryBytes != 256*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d", p.MemoryBytes, 256*1024*1024)
	}
	if p.PidsMax != 64 {
		t.Errorf("PidsMax = %d, want 64", p.PidsMax)
	}
	if p.CPUQuota != 200000 {
		t.Errorf("CPUQuota = %d, want 200000", p.CPUQuota)
	}
	if !p.DynamicUser || !p.NoNewPrivs || !p.DropAllCaps {
		t.Errorf("expected DynamicUser+NoNewPrivs+DropAllCaps, got %+v", p)
	}
	if !p.Confined() || !p.NeedsSetpriv() {
		t.Error("hardened unit should be Confined and NeedsSetpriv")
	}

	// Unenforced directives must be surfaced, not silently dropped.
	joined := strings.Join(p.Unsupported, "\n")
	for _, want := range []string{"ProtectSystem", "SystemCallFilter", "MemoryDenyWriteExecute", "DynamicUser=yes approximated"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Unsupported missing %q; got:\n%s", want, joined)
		}
	}
}

func TestParse_SetprivArgs(t *testing.T) {
	unit := `
[Service]
User=appuser
Group=appgroup
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_CHOWN
`
	p, err := Parse(strings.NewReader(unit))
	if err != nil {
		t.Fatal(err)
	}
	got := p.SetprivArgs()
	want := []string{
		"setpriv", "--no-new-privs",
		"--bounding-set", "-all,+cap_net_bind_service,+cap_chown",
		"--reuid", "appuser",
		"--regid", "appgroup", "--clear-groups",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetprivArgs() = %v\nwant %v", got, want)
	}
}

func TestParse_DynamicUserSetpriv(t *testing.T) {
	p, _ := Parse(strings.NewReader("[Service]\nDynamicUser=yes\nCapabilityBoundingSet=\n"))
	got := p.SetprivArgs()
	want := []string{"setpriv", "--bounding-set", "-all", "--reuid", "65534", "--regid", "65534", "--clear-groups"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetprivArgs() = %v\nwant %v", got, want)
	}
}

// An empty CapabilityBoundingSet= resets the whole set per systemd, discarding
// caps dropped/kept by earlier assignments (no leftover DropCaps).
func TestParse_BoundingSetEmptyReset(t *testing.T) {
	p, err := Parse(strings.NewReader("[Service]\nCapabilityBoundingSet=~CAP_SYS_ADMIN\nCapabilityBoundingSet=\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.DropAllCaps || len(p.DropCaps) != 0 || len(p.KeepCaps) != 0 {
		t.Errorf("empty reset should drop-all with no residual caps, got %+v", p)
	}
	got := p.SetprivArgs()
	want := []string{"setpriv", "--bounding-set", "-all"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetprivArgs() = %v, want %v", got, want)
	}
}

func TestParse_OnlySectionService(t *testing.T) {
	// MemoryMax in a non-[Service] section must be ignored.
	p, _ := Parse(strings.NewReader("[Unit]\nMemoryMax=999M\n[Slice]\nTasksMax=5\n"))
	if p.MemoryBytes != 0 || p.PidsMax != 0 {
		t.Errorf("directives outside [Service] leaked: %+v", p)
	}
	if p.Confined() {
		t.Error("empty [Service] should not be Confined")
	}
}

func TestParse_InfinityAndPercentMemory(t *testing.T) {
	p, _ := Parse(strings.NewReader("[Service]\nMemoryMax=40%\nTasksMax=infinity\n"))
	if p.MemoryBytes != 0 || p.PidsMax != 0 {
		t.Errorf("infinity/percentage should leave limits unset, got %+v", p)
	}
	if !strings.Contains(strings.Join(p.Unsupported, "\n"), "percentage") {
		t.Error("percentage memory should be noted as unresolved")
	}
}

// MemoryHigh is a soft throttle; treating it as memory.max would OOM-kill a
// workload systemd would only throttle. It must NOT lower the hard cap and
// must be surfaced as not-applied.
func TestParse_MemoryHighNotHardCap(t *testing.T) {
	p, err := Parse(strings.NewReader("[Service]\nMemoryMax=512M\nMemoryHigh=256M\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.MemoryBytes != 512*1024*1024 {
		t.Errorf("MemoryBytes = %d, want 512MiB (MemoryHigh must not lower MemoryMax)", p.MemoryBytes)
	}
	if !strings.Contains(strings.Join(p.Unsupported, "\n"), "MemoryHigh") {
		t.Error("MemoryHigh should be surfaced as not-applied")
	}
}

// A negated CapabilityBoundingSet keeps the full set minus the named caps, so
// setpriv must drop only those (no -all), not wipe every capability.
func TestParse_NegatedBoundingSet(t *testing.T) {
	p, err := Parse(strings.NewReader("[Service]\nCapabilityBoundingSet=~CAP_SYS_ADMIN CAP_NET_RAW\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.DropAllCaps {
		t.Error("negated set must not drop all caps")
	}
	got := p.SetprivArgs()
	want := []string{"setpriv", "--bounding-set", "-cap_sys_admin,-cap_net_raw"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetprivArgs() = %v, want %v", got, want)
	}
}

func TestParse_SizeUnits(t *testing.T) {
	tests := map[string]int64{
		"1G": 1 << 30, "512M": 512 << 20, "64K": 64 << 10, "1048576": 1 << 20,
	}
	for in, want := range tests {
		p, err := Parse(strings.NewReader("[Service]\nMemoryMax=" + in + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p.MemoryBytes != want {
			t.Errorf("MemoryMax=%s → %d, want %d", in, p.MemoryBytes, want)
		}
	}
}
