//go:build darwin

package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitGuestReady_SucceedsOnceProbePasses(t *testing.T) {
	calls := 0
	ok := waitGuestReady(func() bool {
		calls++
		return calls >= 3
	}, 10, time.Millisecond)
	if !ok {
		t.Fatal("expected ready=true")
	}
	if calls != 3 {
		t.Errorf("probe called %d times, want 3", calls)
	}
}

func TestWaitGuestReady_TimesOut(t *testing.T) {
	calls := 0
	ok := waitGuestReady(func() bool { calls++; return false }, 5, time.Millisecond)
	if ok {
		t.Fatal("expected ready=false on timeout")
	}
	// Instant probes normally exhaust all 5 attempts, but the wall-clock
	// deadline (attempts*interval) may legitimately stop a hair early under
	// scheduler jitter — so bound the count rather than pin it exactly.
	if calls < 1 || calls > 5 {
		t.Errorf("probe called %d times, want 1..5", calls)
	}
}

func TestWaitGuestReady_WallClockDeadlineCapsSlowProbes(t *testing.T) {
	// Each probe blocks far longer than the interval and never succeeds.
	// Counting attempts alone would run all of them (~attempts*(probe+interval));
	// the wall-clock deadline (attempts*interval) must stop it much sooner.
	const attempts = 50
	const interval = 2 * time.Millisecond // deadline ≈ 100ms
	var calls int32
	probe := func() bool {
		atomic.AddInt32(&calls, 1)
		time.Sleep(10 * time.Millisecond) // each probe dwarfs the interval
		return false
	}
	start := time.Now()
	ok := waitGuestReady(probe, attempts, interval)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("expected ready=false for a never-ready probe")
	}
	// Uncapped this runs ~50*(10+2)=600ms; the deadline caps it near 100ms plus
	// at most one in-flight probe. 300ms sits comfortably between the two.
	if elapsed > 300*time.Millisecond {
		t.Errorf("elapsed %v — wall-clock deadline did not cap a slow probe", elapsed)
	}
	if got := atomic.LoadInt32(&calls); int(got) >= attempts {
		t.Errorf("probe ran %d times (== attempts); the deadline should stop it early", got)
	}
}

func TestWaitGuestReady_SucceedsImmediately(t *testing.T) {
	calls := 0
	ok := waitGuestReady(func() bool { calls++; return true }, 30, time.Hour)
	if !ok || calls != 1 {
		t.Errorf("ready=%v calls=%d, want true/1", ok, calls)
	}
}

// The shared readiness budget must stay fine-grained enough to detect a
// fast-binding service promptly, yet long enough overall to tolerate a slow
// cold start. Guards against silently reverting to a coarse interval or
// shrinking the total window.
func TestReadyProbeBudget(t *testing.T) {
	if readyProbeInterval > 250*time.Millisecond {
		t.Errorf("readyProbeInterval = %v, want ≤ 250ms so a ~180ms bind is caught quickly", readyProbeInterval)
	}
	total := time.Duration(readyProbeAttempts) * readyProbeInterval
	if total < 20*time.Second {
		t.Errorf("readiness budget = %v (%d × %v), want ≥ 20s for a slow first start",
			total, readyProbeAttempts, readyProbeInterval)
	}
}

func TestAppendServiceDiag_CapturesLogs(t *testing.T) {
	exec := func(cmd string) (*RunResult, error) {
		if strings.Contains(cmd, "tail") {
			return &RunResult{Stdout: "FATAL: data dir not empty\n"}, nil
		}
		return &RunResult{}, nil
	}
	ev := map[string]interface{}{"name": "postgres"}
	appendServiceDiag(ev, exec, "postgres")
	if ev["logs"] != "FATAL: data dir not empty" {
		t.Errorf("logs = %v, want the FATAL line", ev["logs"])
	}
}

func TestAppendServiceDiag_ToleratesFailure(t *testing.T) {
	exec := func(string) (*RunResult, error) { return nil, errFakeExec }
	ev := map[string]interface{}{}
	appendServiceDiag(ev, exec, "redis")
	if _, ok := ev["logs"]; ok {
		t.Error("logs should be absent when collection fails")
	}
}

var errFakeExec = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "no VM" }

// bringUpStaged must preserve svcs order in its outcomes regardless of which
// service finishes first, and map launch/readiness results correctly.
func TestBringUpStaged_OrderedOutcomes(t *testing.T) {
	svcs := []stagedService{
		{name: "redis", port: 6379},
		{name: "mailpit", port: 8025},  // launch fails
		{name: "anycable", port: 8080}, // launches but never ready
	}
	launch := func(s stagedService) error {
		if s.name == "mailpit" {
			return errors.New("image not found")
		}
		return nil
	}
	probe := func(s stagedService) bool { return s.name == "redis" }
	diag := func(name string) string { return name + "-logs" }

	got := bringUpStaged(svcs, launch, probe, diag)
	if len(got) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(got))
	}
	// redis: launched + ready, no failure.
	if !got[0].launched || !got[0].ready || got[0].failReason != "" {
		t.Errorf("redis outcome = %+v, want launched+ready", got[0])
	}
	// mailpit: launch failed, reason surfaced, diag captured.
	if got[1].launched || got[1].failReason != "image not found" || got[1].failLogs != "mailpit-logs" {
		t.Errorf("mailpit outcome = %+v, want launch failure with diag", got[1])
	}
	// anycable: launched but not ready, readiness reason + diag.
	if !got[2].launched || got[2].ready || got[2].failLogs != "anycable-logs" {
		t.Errorf("anycable outcome = %+v, want launched-but-not-ready", got[2])
	}
	if !strings.Contains(got[2].failReason, "accepting connections") {
		t.Errorf("anycable failReason = %q, want a readiness-timeout message", got[2].failReason)
	}
}

// The services must actually run concurrently: N probes that each block for D
// should finish in ≈D, not N×D.
func TestBringUpStaged_RunsConcurrently(t *testing.T) {
	const n = 4
	const block = 120 * time.Millisecond
	svcs := make([]stagedService, n)
	for i := range svcs {
		svcs[i] = stagedService{name: string(rune('a' + i)), port: 1000 + i}
	}
	var concurrent int32
	var peak int32
	probe := func(stagedService) bool {
		c := atomic.AddInt32(&concurrent, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
				break
			}
		}
		time.Sleep(block)
		atomic.AddInt32(&concurrent, -1)
		return true
	}
	start := time.Now()
	got := bringUpStaged(svcs, func(stagedService) error { return nil }, probe, func(string) string { return "" })
	elapsed := time.Since(start)

	// Serial execution takes n*block; true concurrency ≈ block. Assert
	// comfortably below the serial time (n-1)*block rather than at the
	// midpoint, so a loaded runner's scheduler jitter doesn't flake an
	// actually-concurrent run. peak (below) is the strong overlap signal.
	if elapsed >= block*time.Duration(n-1) {
		t.Errorf("elapsed %v for %d services blocking %v each — looks serial, not concurrent", elapsed, n, block)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want ≥ 2 (services should overlap)", peak)
	}
	for i, o := range got {
		if !o.launched || !o.ready {
			t.Errorf("outcome[%d] = %+v, want launched+ready", i, o)
		}
	}
}
