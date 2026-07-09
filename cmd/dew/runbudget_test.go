//go:build darwin

package main

import (
	"strings"
	"testing"
	"time"
)

// Without --timeout every stage keeps its own default window and the
// budget never expires — the pre-flag behavior, byte for byte.
func TestRunBudget_Disabled(t *testing.T) {
	b := newRunBudget(0)
	if got := b.window(60 * time.Second); got != 60*time.Second {
		t.Errorf("window = %s, want the stage default", got)
	}
	if b.expired() {
		t.Error("zero budget reported expired")
	}
	if got := b.guestTimeout(); got != 0 {
		t.Errorf("guestTimeout = %s, want 0 (agent default)", got)
	}
}

// A stage window never exceeds what is left of the overall budget.
func TestRunBudget_WindowShrinksToRemaining(t *testing.T) {
	b := newRunBudget(10 * time.Second)
	if got := b.window(60 * time.Second); got > 10*time.Second {
		t.Errorf("window = %s, exceeds the 10s budget", got)
	}
	if got := b.window(time.Second); got > time.Second {
		t.Errorf("window = %s, exceeds the 1s stage default", got)
	}
}

func TestRunBudget_Expiry(t *testing.T) {
	b := newRunBudget(20 * time.Millisecond)
	if b.expired() {
		t.Fatal("expired immediately")
	}
	time.Sleep(30 * time.Millisecond)
	if !b.expired() {
		t.Error("not expired past the deadline")
	}
	if got := b.window(60 * time.Second); got > 0 {
		t.Errorf("window after expiry = %s, want <= 0", got)
	}
	// guestTimeout keeps a floor so the wire value stays positive;
	// expiry is the caller's check, not the guest's.
	if got := b.guestTimeout(); got != time.Second {
		t.Errorf("guestTimeout after expiry = %s, want the 1s floor", got)
	}
}

// netReadyWindow decides whether the --network lease barrier runs and with what
// window. It must (a) run with the full netReadyWait when there's no --timeout,
// (b) shrink to the remaining budget when ample, (c) skip when too little
// remains to finish (below the floor), and (d) skip once expired. When it does
// run, the window must be ≥ netReadyMinBudget so the exec's TimeoutMs is never
// truncated to 0.
func TestRunBudget_NetReadyWindow(t *testing.T) {
	// (a) No --timeout: run with the full default window.
	if w, ok := newRunBudget(0).netReadyWindow(); !ok || w != netReadyWait {
		t.Errorf("no-timeout netReadyWindow = (%s, %v), want (%s, true)", w, ok, netReadyWait)
	}

	// (b) Ample budget: run, window capped at the remaining budget and ≥ floor.
	if w, ok := newRunBudget(10 * time.Second).netReadyWindow(); !ok || w > 10*time.Second || w < netReadyMinBudget {
		t.Errorf("ample-budget netReadyWindow = (%s, %v), want run with floor ≤ w ≤ 10s", w, ok)
	}

	// (c) Budget below the floor: skip (a sub-second wait can't finish a lease
	// that takes ~1-2s, and the run is about to time out).
	if w, ok := newRunBudget(500 * time.Millisecond).netReadyWindow(); ok {
		t.Errorf("sub-floor netReadyWindow = (%s, %v), want skip", w, ok)
	}

	// (d) Expired budget: skip (window() goes non-positive past the deadline).
	b := newRunBudget(20 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if w, ok := b.netReadyWindow(); ok {
		t.Errorf("expired netReadyWindow = (%s, %v), want skip", w, ok)
	}
}

func TestParseFlags_Timeout(t *testing.T) {
	_, _, err := parseFlags([]string{"--timeout", "90s", "--", "uname"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flagTimeout != 90*time.Second {
		t.Errorf("flagTimeout = %s, want 90s", flagTimeout)
	}

	// Reset on the next parse without the flag.
	_, _, err = parseFlags([]string{"--", "uname"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flagTimeout != 0 {
		t.Errorf("flagTimeout = %s after flagless parse, want 0", flagTimeout)
	}
}

func TestParseFlags_TimeoutInvalid(t *testing.T) {
	for _, args := range [][]string{
		{"--timeout"},                  // missing value
		{"--timeout", "banana"},        // unparseable
		{"--timeout", "-5s"},           // non-positive
		{"--timeout", "0s"},            // non-positive
	} {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v): expected error", args)
		} else if !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("parseFlags(%v): error %q doesn't mention --timeout", args, err)
		}
	}
}
