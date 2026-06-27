//go:build darwin

package main

import "testing"

// When a dew.toml `ports` entry remaps a second host port onto a service's
// primary container port, the guest port carries two forwards. forward-list
// order is unspecified, so pickPrimaryHostPort must deterministically report
// the primary's host port (host==guest) regardless of input order.
func TestPickPrimaryHostPort_PrefersPrimaryRegardlessOfOrder(t *testing.T) {
	// 8025 is mailpit's primary (host==guest); 1080 is the extra remap.
	orders := [][]int{
		{8025, 1080},
		{1080, 8025},
	}
	for _, hps := range orders {
		got := pickPrimaryHostPort(map[int][]int{8025: hps})
		if got[8025] != 8025 {
			t.Errorf("input order %v: host port for guest 8025 = %d, want 8025 (the primary)", hps, got[8025])
		}
	}
}

// With no host==guest forward (primary fell back to a busy-port replacement),
// the choice must still be deterministic — the smallest host port — not
// dependent on forward-list ordering.
func TestPickPrimaryHostPort_DeterministicWithoutPrimaryMapping(t *testing.T) {
	a := pickPrimaryHostPort(map[int][]int{8025: {1090, 1080}})
	b := pickPrimaryHostPort(map[int][]int{8025: {1080, 1090}})
	if a[8025] != 1080 || b[8025] != 1080 {
		t.Errorf("non-primary forwards not reduced deterministically: got %d and %d, want 1080 both", a[8025], b[8025])
	}
}

// A single forward per guest port is reported as-is, including a busy-port
// fallback where the host port differs from the guest port.
func TestPickPrimaryHostPort_SingleForward(t *testing.T) {
	got := pickPrimaryHostPort(map[int][]int{6379: {6379}, 5432: {15432}})
	if got[6379] != 6379 {
		t.Errorf("guest 6379 = %d, want 6379", got[6379])
	}
	if got[5432] != 15432 {
		t.Errorf("guest 5432 (busy-port fallback) = %d, want 15432", got[5432])
	}
}
