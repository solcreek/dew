//go:build darwin

package main

import (
	"strings"
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
	if calls != 5 {
		t.Errorf("probe called %d times, want 5", calls)
	}
}

func TestWaitGuestReady_SucceedsImmediately(t *testing.T) {
	calls := 0
	ok := waitGuestReady(func() bool { calls++; return true }, 30, time.Hour)
	if !ok || calls != 1 {
		t.Errorf("ready=%v calls=%d, want true/1", ok, calls)
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
