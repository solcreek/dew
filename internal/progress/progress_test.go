package progress

import (
	"testing"
	"time"
)

func TestSpinner_Steps(t *testing.T) {
	s := New()
	s.Step("first")
	time.Sleep(50 * time.Millisecond)
	s.Step("second")
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	if s.steps != 2 {
		t.Errorf("steps = %d, want 2", s.steps)
	}
}

func TestSpinner_StopIdempotent(t *testing.T) {
	s := New()
	s.Step("test")
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	s.Stop()
}

func TestSpinner_StopBeforeStart(t *testing.T) {
	s := New()
	s.Stop()
}

func TestSpinner_Fail(t *testing.T) {
	s := New()
	s.Step("failing")
	time.Sleep(50 * time.Millisecond)
	s.Fail("timeout")
}
