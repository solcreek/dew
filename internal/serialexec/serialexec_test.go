package serialexec

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWaitReady(t *testing.T) {
	input := "booting...\nkernel: init\n  dew vm ready\n~ # "
	s := New(strings.NewReader(input), &bytes.Buffer{})
	if err := s.WaitReady(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReady_Timeout(t *testing.T) {
	input := "booting...\nkernel: init\n"
	s := New(strings.NewReader(input), &bytes.Buffer{})
	err := s.WaitReady(50 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRun(t *testing.T) {
	// Simulate what the guest shell would produce:
	// 1. Echo of command (should be skipped)
	// 2. Command output
	// 3. Sentinel with exit code
	guestOutput := fmt.Sprintf(
		"ls -la; echo '%s' $?\nhello.txt\nworld.txt\n%s 0\n",
		sentinel, sentinel,
	)

	var cmdBuf bytes.Buffer
	s := New(strings.NewReader(guestOutput), &cmdBuf)

	out, exitCode, err := s.Run("ls -la")
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Errorf("output missing hello.txt: %q", out)
	}
	if !strings.Contains(out, "world.txt") {
		t.Errorf("output missing world.txt: %q", out)
	}

	// Verify the command was sent
	sent := cmdBuf.String()
	if !strings.Contains(sent, "ls -la") {
		t.Errorf("sent command missing: %q", sent)
	}
	if !strings.Contains(sent, sentinel) {
		t.Errorf("sent command missing sentinel: %q", sent)
	}
}

func TestRun_NonZeroExit(t *testing.T) {
	guestOutput := fmt.Sprintf(
		"cat /nonexistent; echo '%s' $?\ncat: can't open\n%s 1\n",
		sentinel, sentinel,
	)

	s := New(strings.NewReader(guestOutput), &bytes.Buffer{})
	out, exitCode, err := s.Run("cat /nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
	if !strings.Contains(out, "can't open") {
		t.Errorf("output = %q, want error message", out)
	}
}

func TestRun_EmptyOutput(t *testing.T) {
	guestOutput := fmt.Sprintf(
		"true; echo '%s' $?\n%s 0\n",
		sentinel, sentinel,
	)

	s := New(strings.NewReader(guestOutput), &bytes.Buffer{})
	out, exitCode, err := s.Run("true")
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
}
