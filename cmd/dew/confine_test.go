//go:build darwin

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/confine"
	vsockProto "github.com/solcreek/dew/internal/vsock"
	"github.com/solcreek/dew/pkg/dewerr"
)

// confineUnenforceableErr fails closed whenever a --confine spec can't reach
// and be applied by the agent shim; nil spec or an acking vsock agent is fine.
func TestConfineUnenforceableErr(t *testing.T) {
	spec := &vsockProto.Confinement{User: "1000"}

	// No spec → never an error, whatever the channel.
	if err := confineUnenforceableErr(nil, true, false, false, true); err != nil {
		t.Errorf("nil spec should be enforceable, got %v", err)
	}

	// Happy path: vsock handshake done, agent advertised support.
	if err := confineUnenforceableErr(spec, false, true, true, false); err != nil {
		t.Errorf("acking vsock agent should be enforceable, got %v", err)
	}

	// --stream → usage error (the shim runs on the batch path only).
	if err := confineUnenforceableErr(spec, true, true, true, false); err == nil {
		t.Error("--stream should be rejected")
	} else if dewerr.CodeOf(err) != dewerr.CodeUsage {
		t.Errorf("--stream code = %v, want CodeUsage", dewerr.CodeOf(err))
	}

	// Old agent: handshake completed but no confine ack → fail closed.
	if err := confineUnenforceableErr(spec, false, true, false, false); err == nil {
		t.Error("non-acking agent should be rejected")
	} else if dewerr.CodeOf(err) != dewerr.CodeUnavailable {
		t.Errorf("old-agent code = %v, want CodeUnavailable", dewerr.CodeOf(err))
	}

	// Serial fallback → fail closed (no ExecRequest channel).
	if err := confineUnenforceableErr(spec, false, false, false, true); err == nil {
		t.Error("serial fallback should be rejected")
	} else if dewerr.CodeOf(err) != dewerr.CodeUnavailable {
		t.Errorf("serial code = %v, want CodeUnavailable", dewerr.CodeOf(err))
	}

	// No handshake and not yet on the serial branch → defer (no error), so the
	// caller's serial path emits the accurate "vsock unavailable" message.
	if err := confineUnenforceableErr(spec, false, false, false, false); err != nil {
		t.Errorf("pre-serial no-handshake should defer, got %v", err)
	}
}

// confinementFromPlan now carries the whole spec to the agent (the shim applies
// caps/uid/no_new_privs natively, not setpriv), so a privilege-drop-only unit
// also yields a spec.
func TestConfinementFromPlan(t *testing.T) {
	// Nothing the shim applies → nil.
	if c := confinementFromPlan(confine.Plan{MemoryBytes: 1 << 20}); c != nil {
		t.Errorf("cgroup-only plan should map to nil confine spec, got %+v", c)
	}

	// Privilege-drop-only unit → spec carrying the caps/uid/nnp fields.
	c := confinementFromPlan(confine.Plan{
		UID: "1000", GID: "1000", DropAllCaps: true,
		KeepCaps: []string{"cap_net_bind_service"}, NoNewPrivs: true,
	})
	if c == nil || !c.Set() {
		t.Fatalf("priv-drop unit should map to a spec, got %+v", c)
	}
	if c.User != "1000" || c.Group != "1000" || !c.DropAllCaps || !c.NoNewPrivs ||
		!reflect.DeepEqual(c.KeepCaps, []string{"cap_net_bind_service"}) {
		t.Errorf("priv-drop fields lost: %+v", c)
	}

	// DynamicUser= is pre-resolved to the fallback uid so the agent needs no
	// confine constant.
	c = confinementFromPlan(confine.Plan{DynamicUser: true})
	if c == nil || c.User != confine.DynamicUserUID {
		t.Errorf("DynamicUser should pre-resolve to %s, got %+v", confine.DynamicUserUID, c)
	}

	// Read-only fs + writable exceptions still carried.
	c = confinementFromPlan(confine.Plan{
		ReadOnlyRoot:   true,
		ReadWritePaths: []string{"/var/lib/app"},
	})
	if c == nil || !c.ReadOnlyRoot ||
		!reflect.DeepEqual(c.ReadWritePaths, []string{"/var/lib/app"}) {
		t.Fatalf("ro-fs spec lost: %+v", c)
	}
}

// Integration: a real hardened unit parses to a Plan and maps to a wire spec
// the agent shim can act on (caps + uid + nnp + ro-fs all present).
func TestConfinementFromPlan_ParsedUnit(t *testing.T) {
	unit := `
[Service]
User=appuser
DynamicUser=no
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ReadWritePaths=/var/lib/app
`
	p, err := confine.Parse(strings.NewReader(unit))
	if err != nil {
		t.Fatal(err)
	}
	c := confinementFromPlan(p)
	if c == nil {
		t.Fatal("hardened unit should map to a confinement spec")
	}
	if c.User != "appuser" || !c.NoNewPrivs || !c.DropAllCaps || !c.ReadOnlyRoot {
		t.Errorf("spec missing fields: %+v", c)
	}
	if !reflect.DeepEqual(c.KeepCaps, []string{"cap_net_bind_service"}) {
		t.Errorf("KeepCaps = %v", c.KeepCaps)
	}
	if !reflect.DeepEqual(c.ReadWritePaths, []string{"/var/lib/app"}) {
		t.Errorf("ReadWritePaths = %v", c.ReadWritePaths)
	}
}
