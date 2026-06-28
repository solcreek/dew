package vsock

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfinement_Set(t *testing.T) {
	if (&Confinement{}).Set() {
		t.Error("empty Confinement should not be Set")
	}
	if (*Confinement)(nil).Set() {
		t.Error("nil Confinement should not be Set")
	}
	for name, c := range map[string]*Confinement{
		"user":     {User: "appuser"},
		"group":    {Group: "appgroup"},
		"dynamic":  {DynamicUser: true},
		"nnp":      {NoNewPrivs: true},
		"dropall":  {DropAllCaps: true},
		"keepcaps": {KeepCaps: []string{"cap_net_bind_service"}},
		"dropcaps": {DropCaps: []string{"cap_sys_admin"}},
		"rofs":     {ReadOnlyRoot: true},
		"rwpaths":  {ReadWritePaths: []string{"/var/lib/app"}},
	} {
		if !c.Set() {
			t.Errorf("%s: expected Set() true", name)
		}
	}
}

// An unconfined ExecRequest must not carry a confine object on the wire, so an
// older agent (and the common fast path) is unaffected.
func TestExecRequest_ConfineOmittedWhenNil(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &ExecRequest{Command: "ls"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "confine") {
		t.Errorf("nil Confine should be omitted from the wire, got: %s", buf.String())
	}
}

func TestWriteReadJSON_Confinement(t *testing.T) {
	req := ExecRequest{
		Command: "id",
		Confine: &Confinement{
			User: "65534", Group: "65534", DynamicUser: true, NoNewPrivs: true,
			DropAllCaps: true, KeepCaps: []string{"cap_net_bind_service"},
			ReadOnlyRoot: true, ReadWritePaths: []string{"/var/lib/app"},
		},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got ExecRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Confine == nil || !got.Confine.Set() {
		t.Fatalf("Confine did not round-trip: %+v", got.Confine)
	}
	if got.Confine.User != "65534" || !got.Confine.ReadOnlyRoot ||
		len(got.Confine.KeepCaps) != 1 || got.Confine.KeepCaps[0] != "cap_net_bind_service" {
		t.Errorf("Confine fields lost in round-trip: %+v", got.Confine)
	}
}
