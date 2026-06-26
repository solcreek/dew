//go:build darwin

package main

import "testing"

// A services-only / no-dev-server run (devPort 0) must get NO provisional
// forward — otherwise a 0 guest port reaches daemon.AddForward and logs a
// spurious "host_port and guest_port required" at boot. A real dev port gets
// exactly one forward whose guest side is that port.
func TestInitialForwards(t *testing.T) {
	for _, devPort := range []int{0, -1} {
		if f := initialForwards(devPort); f != nil {
			t.Errorf("initialForwards(%d) = %v, want nil", devPort, f)
		}
	}

	f := initialForwards(5173)
	if len(f) != 1 {
		t.Fatalf("initialForwards(5173) len = %d, want 1", len(f))
	}
	if f[0].GuestPort != 5173 {
		t.Errorf("GuestPort = %d, want 5173", f[0].GuestPort)
	}
	if f[0].HostPort == 0 {
		t.Error("HostPort is 0; provisional forward must pick a host port")
	}
}
