package serialexec

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
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

// fakeGuest wires an Exec to a scripted guest shell: every command
// line the host writes is answered with banner-echo + the scripted
// response, like a real serial console. Output arrives only AFTER the
// command is written, which is what the capture window expects.
func fakeGuest(t *testing.T, respond func(cmdLine string) string) (*Exec, *bytes.Buffer) {
	t.Helper()
	consoleR, consoleW := io.Pipe() // guest output → host
	cmdR, cmdW := io.Pipe()         // host commands → guest
	t.Cleanup(func() { consoleR.Close(); cmdW.Close() })

	var sent bytes.Buffer
	s := New(consoleR, io.MultiWriter(cmdW, &sent))

	go func() {
		// Boot banner before any command — must never leak into
		// captured output.
		io.WriteString(consoleW, "kernel: booting\n  dew vm ready\n~ # \n")
		br := bufio.NewReader(cmdR)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			io.WriteString(consoleW, line) // shell echoes the input
			io.WriteString(consoleW, respond(line))
		}
	}()
	return s, &sent
}

func TestRun(t *testing.T) {
	s, sent := fakeGuest(t, func(string) string {
		return fmt.Sprintf("hello.txt\nworld.txt\n%s 0\n", sentinel)
	})

	if err := s.WaitReady(2 * time.Second); err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(sent.String(), "ls -la") {
		t.Errorf("sent command missing: %q", sent.String())
	}
	if !strings.Contains(sent.String(), sentinel) {
		t.Errorf("sent command missing sentinel: %q", sent.String())
	}
}

// Console output from before the command (boot logs, prompts) must not
// leak into the captured stdout — regression test for the capture
// window: a guest that booted noisily used to have its entire kernel
// log prepended to the first command's output.
func TestRun_BootNoiseExcluded(t *testing.T) {
	s, _ := fakeGuest(t, func(string) string {
		return fmt.Sprintf("clean-output\n%s 0\n", sentinel)
	})
	if err := s.WaitReady(2 * time.Second); err != nil {
		t.Fatal(err)
	}

	out, _, err := s.Run("uname -a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "booting") || strings.Contains(out, "dew vm ready") {
		t.Errorf("boot noise leaked into output: %q", out)
	}
	if !strings.Contains(out, "clean-output") {
		t.Errorf("output missing command result: %q", out)
	}
}

func TestRun_NonZeroExit(t *testing.T) {
	s, _ := fakeGuest(t, func(string) string {
		return fmt.Sprintf("cat: can't open\n%s 1\n", sentinel)
	})

	if err := s.WaitReady(2 * time.Second); err != nil {
		t.Fatal(err)
	}
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

// A guest that produces no output at all must not hang WaitReady —
// this is the regression test for `dew run` blocking forever on the
// serial fallback path. io.Pipe (unlike strings.Reader) never EOFs,
// matching a live-but-silent console.
func TestWaitReady_SilentGuestTimesOut(t *testing.T) {
	r, _ := io.Pipe()
	defer r.Close()
	s := New(r, &bytes.Buffer{})

	start := time.Now()
	err := s.WaitReady(100 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~100ms", elapsed)
	}
}

// Same for Run: a command whose output never arrives must time out.
func TestRun_SilentGuestTimesOut(t *testing.T) {
	r, _ := io.Pipe()
	defer r.Close()
	s := New(r, &bytes.Buffer{})
	s.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, exitCode, err := s.Run("uname -a")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~100ms", elapsed)
	}
}

// Readiness latches: a second WaitReady returns immediately even
// though the marker line was already consumed.
func TestWaitReady_Latches(t *testing.T) {
	input := "booting...\n  dew vm ready\n"
	s := New(strings.NewReader(input), &bytes.Buffer{})
	if err := s.WaitReady(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitReady(10 * time.Millisecond); err != nil {
		t.Fatalf("second WaitReady should hit the latch: %v", err)
	}
}

// Concurrent WaitReady callers must be memory-safe (run with -race).
// Previously both raced on a shared bufio.Reader.
func TestWaitReady_ConcurrentCallers(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	s := New(pr, &bytes.Buffer{})

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.WaitReady(2 * time.Second)
		}(i)
	}
	go func() {
		pw.Write([]byte("noise\nmore noise\n  dew vm ready\n"))
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
}

func TestRun_EmptyOutput(t *testing.T) {
	s, _ := fakeGuest(t, func(string) string {
		return fmt.Sprintf("%s 0\n", sentinel)
	})

	if err := s.WaitReady(2 * time.Second); err != nil {
		t.Fatal(err)
	}
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
