//go:build linux

package main

import (
	"reflect"
	"testing"

	protocol "github.com/solcreek/dew/internal/vsock"
)

func TestResolveCap(t *testing.T) {
	if n, err := resolveCap("cap_net_bind_service"); err != nil || n != 10 {
		t.Errorf("cap_net_bind_service = %d,%v; want 10,nil", n, err)
	}
	if n, err := resolveCap("CAP_SYS_ADMIN"); err != nil || n != 21 {
		t.Errorf("CAP_SYS_ADMIN (case-insensitive) = %d,%v; want 21,nil", n, err)
	}
	if _, err := resolveCap("cap_not_a_real_cap"); err == nil {
		t.Error("unknown cap should error (fail closed)")
	}
}

func TestCapsToDrop(t *testing.T) {
	// DropAllCaps with a keep list → every cap in [0,lastCap] except the kept.
	got, err := capsToDrop(&protocol.Confinement{
		DropAllCaps: true, KeepCaps: []string{"cap_net_bind_service", "cap_chown"},
	}, 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12} // 0 (chown) and 10 (net_bind) kept
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DropAllCaps keep set = %v, want %v", got, want)
	}

	// DropAllCaps with no keep → the whole range.
	got, _ = capsToDrop(&protocol.Confinement{DropAllCaps: true}, 2)
	if !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("DropAllCaps no-keep = %v, want [0 1 2]", got)
	}

	// Negated set → only the named caps, regardless of lastCap.
	got, _ = capsToDrop(&protocol.Confinement{DropCaps: []string{"cap_sys_admin", "cap_net_raw"}}, 40)
	if !reflect.DeepEqual(got, []int{21, 13}) {
		t.Errorf("DropCaps = %v, want [21 13]", got)
	}

	// Unknown cap name fails closed.
	if _, err := capsToDrop(&protocol.Confinement{DropAllCaps: true, KeepCaps: []string{"cap_bogus"}}, 40); err == nil {
		t.Error("unknown KeepCaps entry should error")
	}
}

func TestKeptCaps(t *testing.T) {
	got, err := keptCaps(&protocol.Confinement{DropAllCaps: true, KeepCaps: []string{"cap_net_bind_service"}})
	if err != nil || !reflect.DeepEqual(got, []int{10}) {
		t.Errorf("keptCaps = %v,%v; want [10],nil", got, err)
	}
	// KeepCaps only matters with DropAllCaps (a negated set keeps the rest, so
	// there is no explicit keep list to make ambient).
	if got, _ := keptCaps(&protocol.Confinement{KeepCaps: []string{"cap_chown"}}); got != nil {
		t.Errorf("keptCaps without DropAllCaps = %v, want nil", got)
	}
}

func TestResolveDropID(t *testing.T) {
	// Numeric User= → uid+gid both set. (Numeric avoids an /etc/passwd lookup.)
	id, err := resolveDropID(&protocol.Confinement{User: "1000"}, "")
	if err != nil || !id.setUID || !id.setGID || id.uid != 1000 || id.gid != 1000 {
		t.Errorf("User=1000 → %+v,%v", id, err)
	}

	// Group= overrides the gid.
	id, _ = resolveDropID(&protocol.Confinement{User: "1000", Group: "2000"}, "")
	if id.gid != 2000 || id.uid != 1000 {
		t.Errorf("Group override → %+v", id)
	}

	// Group= alone → drop the gid only, leave the uid (root). Regression guard
	// for a Group-only unit being silently ignored.
	id, _ = resolveDropID(&protocol.Confinement{Group: "2000"}, "")
	if id.setUID || !id.setGID || id.gid != 2000 {
		t.Errorf("Group-only → %+v, want setGID gid=2000, setUID=false", id)
	}

	// DynamicUser → the nobody fallback.
	id, _ = resolveDropID(&protocol.Confinement{DynamicUser: true}, "")
	if !id.setUID || id.uid != 65534 {
		t.Errorf("DynamicUser → %+v, want uid 65534", id)
	}

	// No unit identity but DEW_EXEC_USER set → drop to it.
	id, _ = resolveDropID(&protocol.Confinement{}, "1001")
	if !id.setUID || id.uid != 1001 {
		t.Errorf("DEW_EXEC_USER fallback → %+v, want uid 1001", id)
	}

	// Unit User= wins over DEW_EXEC_USER.
	id, _ = resolveDropID(&protocol.Confinement{User: "1000"}, "1001")
	if id.uid != 1000 {
		t.Errorf("unit User should win over DEW_EXEC_USER, got uid %d", id.uid)
	}

	// Nothing → no drop (stay root).
	id, _ = resolveDropID(&protocol.Confinement{}, "")
	if id.setUID || id.setGID {
		t.Errorf("no identity → should not drop, got %+v", id)
	}
}
