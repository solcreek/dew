//go:build darwin

package main

import (
	"reflect"
	"testing"

	"github.com/solcreek/dew/internal/confine"
)

// confinementFromPlan carries only the read-only-fs half to the agent; the
// uid/caps drop still rides the setpriv prefix, so a unit without
// ProtectSystem=strict yields no spec (nil) even when it drops privileges.
func TestConfinementFromPlan(t *testing.T) {
	if c := confinementFromPlan(confine.Plan{DropAllCaps: true, NoNewPrivs: true}); c != nil {
		t.Errorf("priv-drop-only plan should map to nil confine spec, got %+v", c)
	}
	c := confinementFromPlan(confine.Plan{
		ReadOnlyRoot:   true,
		ReadWritePaths: []string{"/var/lib/app"},
		DropAllCaps:    true, // setpriv handles this; must not leak into the spec
	})
	if c == nil || !c.ReadOnlyRoot {
		t.Fatalf("ProtectSystem=strict plan should map to a ro-fs spec, got %+v", c)
	}
	if c.DropAllCaps {
		t.Error("caps drop must not be carried in the spec (setpriv owns it this phase)")
	}
	if !reflect.DeepEqual(c.ReadWritePaths, []string{"/var/lib/app"}) {
		t.Errorf("ReadWritePaths = %v, want [/var/lib/app]", c.ReadWritePaths)
	}
}

// wrapWithSetpriv prepends the setpriv prefix and, for a single shell-string
// arg, wraps it in /bin/sh -c so setpriv (which doesn't parse shell syntax)
// execs a real argv.
func TestWrapWithSetpriv(t *testing.T) {
	prefix := []string{"setpriv", "--reuid", "65534"}

	// Multi-arg argv is passed straight through after `--`.
	got := wrapWithSetpriv(prefix, []string{"/x/app", "--flag"})
	want := []string{"setpriv", "--reuid", "65534", "--", "/x/app", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv form = %v, want %v", got, want)
	}

	// Single shell-string arg is wrapped in /bin/sh -c.
	got = wrapWithSetpriv(prefix, []string{"echo a; echo b"})
	want = []string{"setpriv", "--reuid", "65534", "--", "/bin/sh", "-c", "echo a; echo b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shell form = %v, want %v", got, want)
	}
}
