package progress

import (
	"testing"
	"time"
)

func TestSpinner_StepCount(t *testing.T) {
	s := New(3)
	if s.total != 3 {
		t.Errorf("total = %d, want 3", s.total)
	}
	if s.step != 0 {
		t.Errorf("step = %d, want 0", s.step)
	}

	s.Step("first")
	time.Sleep(50 * time.Millisecond)
	if s.step != 1 {
		t.Errorf("after Step, step = %d, want 1", s.step)
	}

	s.Step("second")
	time.Sleep(50 * time.Millisecond)
	if s.step != 2 {
		t.Errorf("after Step, step = %d, want 2", s.step)
	}

	s.Stop()
}

func TestSpinner_StopIdempotent(t *testing.T) {
	s := New(1)
	s.Step("test")
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	s.Stop() // should not panic
}

func TestSpinner_StopBeforeStart(t *testing.T) {
	s := New(1)
	s.Stop() // should not panic
}

func TestSpinner_Fail(t *testing.T) {
	s := New(1)
	s.Step("failing")
	time.Sleep(50 * time.Millisecond)
	s.Fail("timeout") // should not panic
}
