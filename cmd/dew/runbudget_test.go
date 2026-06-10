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
