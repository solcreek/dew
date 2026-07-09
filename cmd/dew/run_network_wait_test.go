//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
)

// The network barrier keys off the same /run/dew-net-pending marker init-stage2
// sets/clears in initramfs/build.sh. If either side renames it, dew run's
// --network barrier silently degrades to a no-op — the exact race it exists to
// close (guest command runs before the DHCP lease lands). Pin both the host
// constant and the guest script so a rename fails a test in the same change.
func TestGuestNetPendingMarkerMatchesBuildScript(t *testing.T) {
	if guestNetPendingMarker != "/run/dew-net-pending" {
		t.Fatalf("guestNetPendingMarker = %q; the build.sh drift guards below assume /run/dew-net-pending", guestNetPendingMarker)
	}
	script := readBuildScript(t)
	// init-stage2 marks the lease in flight, and bring_up_network clears it.
	if !strings.Contains(script, ": > "+guestNetPendingMarker) {
		t.Errorf("init-stage2 no longer creates the lease marker (%s); dew run's --network barrier would never wait", guestNetPendingMarker)
	}
	if !strings.Contains(script, "rm -f "+guestNetPendingMarker) {
		t.Errorf("bring_up_network no longer clears the lease marker (%s); dew run's --network barrier would hang until its cap", guestNetPendingMarker)
	}
	// dew-oci-run waits on the same marker; the barrier reuses that contract.
	if !strings.Contains(script, "[ -e "+guestNetPendingMarker+" ]") {
		t.Errorf("build.sh no longer polls the lease marker (%s) the barrier mirrors", guestNetPendingMarker)
	}
}

// guestNetReadyCmd must (1) poll the marker, (2) be bounded so a genuinely dead
// lease (e.g. an affected macOS 26 VZ build) can't hang the run forever, and
// (3) report ready vs timed-out via exit code so the host can warn-and-proceed.
func TestGuestNetReadyCmd(t *testing.T) {
	cmd := guestNetReadyCmd(300)

	if !strings.Contains(cmd, guestNetPendingMarker) {
		t.Errorf("ready cmd does not reference the lease marker %s:\n%s", guestNetPendingMarker, cmd)
	}
	// Bounded loop: the cap must appear as the loop guard.
	if !strings.Contains(cmd, `"$i" -lt 300`) {
		t.Errorf("ready cmd is not bounded at the given cap:\n%s", cmd)
	}
	if !strings.Contains(cmd, "sleep 0.1") {
		t.Errorf("ready cmd does not back off between polls:\n%s", cmd)
	}
	// Exit-code contract: 0 when the marker is gone (ready), 1 when it lingers
	// past the cap (timed out). Both branches must be present.
	if !strings.Contains(cmd, "exit 1") || !strings.Contains(cmd, "exit 0") {
		t.Errorf("ready cmd lost its ready(0)/timeout(1) exit contract:\n%s", cmd)
	}

	// The cap is a real parameter, not a hardcoded 300.
	if got := guestNetReadyCmd(7); !strings.Contains(got, `"$i" -lt 7`) {
		t.Errorf("cap not plumbed through:\n%s", got)
	}
	// ~30s at the shipped cap: keep the barrier from stretching a run far past
	// the container path's matching bound if someone bumps deciseconds by accident.
	if netReadyDeciseconds != 300 {
		t.Errorf("netReadyDeciseconds = %d; expected 300 (~30s, matching dew-oci-run). Update this test deliberately if changing.", netReadyDeciseconds)
	}
}

// netLeasePending drives the warn/proceed decision. Any clean exec that came
// back non-zero (the cap-hit exit 1, or an agent-timeout kill with some other
// code) means "could not confirm the lease landed" and warrants the warning; a
// healthy exec (exit 0), a connect/exec error, or a nil result must not warn —
// we couldn't prove the lease is stuck, so we stay quiet and proceed.
func TestNetLeasePending(t *testing.T) {
	cases := []struct {
		name string
		res  *RunResult
		err  error
		want bool
	}{
		{"ready", &RunResult{ExitCode: 0}, nil, false},
		{"timed out", &RunResult{ExitCode: 1}, nil, true},
		// Any clean non-zero (e.g. an agent-timeout kill) is "not confirmed
		// ready" — the warning is intentionally broad, not pinned to exit 1.
		{"other nonzero clean", &RunResult{ExitCode: 127}, nil, true},
		{"exec error", &RunResult{ExitCode: 1}, errors.New("vsock closed"), false},
		{"nil result", nil, nil, false},
		{"nonzero but errored", &RunResult{ExitCode: 42}, errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := netLeasePending(tc.res, tc.err); got != tc.want {
				t.Errorf("netLeasePending(%v, %v) = %v, want %v", tc.res, tc.err, got, tc.want)
			}
		})
	}
}
