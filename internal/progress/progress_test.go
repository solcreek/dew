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

func TestFormatStepDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{380 * time.Millisecond, "380ms"},
		{999 * time.Millisecond, "999ms"},
		{1 * time.Second, "1.0s"},
		{1500 * time.Millisecond, "1.5s"},
		{9999 * time.Millisecond, "10.0s"}, // boundary: rounds to 10.0, but cutoff is <10s
		{10 * time.Second, "10s"},
		{45 * time.Second, "45s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{72 * time.Second, "1m12s"},
		{125 * time.Second, "2m5s"},
	}
	for _, tc := range cases {
		got := formatStepDuration(tc.in)
		if got != tc.want {
			t.Errorf("formatStepDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
