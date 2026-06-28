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
	// ProtectSystem=strict + ReadWritePaths are now enforced (read-only rootfs).
	if !p.ReadOnlyRoot {
		t.Error("ProtectSystem=strict should set ReadOnlyRoot")
	}
	if len(p.ReadWritePaths) != 1 || p.ReadWritePaths[0] != "/var/lib/app" {
		t.Errorf("ReadWritePaths = %v, want [/var/lib/app]", p.ReadWritePaths)
	}

	// Unenforced directives must be surfaced, not silently dropped. (ProtectSystem
	// is no longer here — =strict is enforced above.)
	joined := strings.Join(p.Unsupported, "\n")
	for _, want := range []string{"SystemCallFilter", "MemoryDenyWriteExecute", "DynamicUser=yes approximated"} {
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

// A ~negation after a positive set must revoke the cap from the kept set, not
// leave it (which would be more permissive than systemd).
func TestParse_NegationAfterPositiveSet(t *testing.T) {
	p, err := Parse(strings.NewReader(
		"[Service]\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN\nCapabilityBoundingSet=~CAP_SYS_ADMIN\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := p.SetprivArgs()
	want := []string{"setpriv", "--bounding-set", "-all,+cap_net_bind_service"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetprivArgs() = %v, want %v (CAP_SYS_ADMIN must be revoked)", got, want)
	}
}

// Malformed numeric directives must fail parsing rather than silently drop an
// intended cap; "infinity" stays a legitimate unset.
func TestParse_InvalidNumericDirectives(t *testing.T) {
	for _, unit := range []string{
		"[Service]\nTasksMax=abc\n",
		"[Service]\nTasksMax=-5\n",
		"[Service]\nCPUQuota=abc\n",
		"[Service]\nCPUQuota=0.0001%\n", // rounds to 0
		"[Service]\nMemoryMax=0\n",      // 0 is not a valid hard cap
		"[Service]\nMemoryMax=0M\n",
		"[Service]\nCPUQuota=99999999999999999%\n", // overflows int64
	} {
		if _, err := Parse(strings.NewReader(unit)); err == nil {
			t.Errorf("expected parse error for unit:\n%s", unit)
		}
	}
	// infinity is a valid "unset", not an error.
	if _, err := Parse(strings.NewReader("[Service]\nTasksMax=infinity\nCPUQuota=infinity\n")); err != nil {
		t.Errorf("infinity should not error: %v", err)
	}
}

// A unit setting only Group= must still drop the group (NeedsSetpriv true).
func TestParse_GroupOnly(t *testing.T) {
	p, err := Parse(strings.NewReader("[Service]\nGroup=appgroup\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.NeedsSetpriv() || !p.Confined() {
		t.Fatalf("Group=-only should need setpriv, got %+v", p)
	}
	got := p.SetprivArgs()
	want := []string{"setpriv", "--regid", "appgroup", "--clear-groups"}
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

// ProtectSystem=strict maps to a read-only rootfs; ReadWritePaths accumulate
// (across multiple keys and space-separated values) as the writable exceptions.
func TestParse_ReadOnlyFilesystem(t *testing.T) {
	p, err := Parse(strings.NewReader(
		"[Service]\nProtectSystem=strict\nReadWritePaths=/var/lib/app /var/log/app\nReadWritePaths=/run/app\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.ReadOnlyRoot {
		t.Error("ProtectSystem=strict should set ReadOnlyRoot")
	}
	want := []string{"/var/lib/app", "/var/log/app", "/run/app"}
	if !reflect.DeepEqual(p.ReadWritePaths, want) {
		t.Errorf("ReadWritePaths = %v, want %v", p.ReadWritePaths, want)
	}
	if !p.Confined() {
		t.Error("a unit with ProtectSystem=strict should be Confined")
	}
	if p.NeedsSetpriv() {
		t.Error("ProtectSystem=strict alone must not require setpriv (it's a mount-ns op)")
	}
}

// An empty ReadWritePaths= resets the accumulated list (systemd drop-in
// semantics), so stale earlier entries don't linger as writable exceptions.
func TestParse_ReadWritePathsReset(t *testing.T) {
	p, err := Parse(strings.NewReader(
		"[Service]\nProtectSystem=strict\nReadWritePaths=/var/lib/app /var/log/app\nReadWritePaths=\nReadWritePaths=/run/app\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/run/app"}
	if !reflect.DeepEqual(p.ReadWritePaths, want) {
		t.Errorf("ReadWritePaths = %v, want %v (empty assignment should reset)", p.ReadWritePaths, want)
	}
}

// ProtectSystem=true/full protect only a subset; we don't approximate them with
// strict, so they stay surfaced as unenforced and don't set ReadOnlyRoot.
func TestParse_ProtectSystemNonStrict(t *testing.T) {
	for _, v := range []string{"true", "full", "yes"} {
		p, err := Parse(strings.NewReader("[Service]\nProtectSystem=" + v + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if p.ReadOnlyRoot {
			t.Errorf("ProtectSystem=%s must not set ReadOnlyRoot", v)
		}
		if !strings.Contains(strings.Join(p.Unsupported, "\n"), "ProtectSystem="+v) {
			t.Errorf("ProtectSystem=%s should be surfaced as unenforced", v)
		}
	}
}

// ReadWritePaths without ProtectSystem=strict would be a silent no-op (the root
// stays writable), so they must be surfaced as unenforced and dropped.
func TestParse_ReadWritePathsWithoutStrict(t *testing.T) {
	p, err := Parse(strings.NewReader("[Service]\nReadWritePaths=/var/lib/app\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ReadWritePaths) != 0 {
		t.Errorf("ReadWritePaths should be dropped without ProtectSystem=strict, got %v", p.ReadWritePaths)
	}
	if !strings.Contains(strings.Join(p.Unsupported, "\n"), "ReadWritePaths=") {
		t.Error("ReadWritePaths without strict should be surfaced as unenforced")
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
