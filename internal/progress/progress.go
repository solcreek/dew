// Package progress provides a terminal spinner for CLI feedback.
// Inspired by Turborepo's clean output style.
package progress

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// isTTY returns true if stderr is a terminal. When false, spinners
// are suppressed (no ANSI control codes), and each Step prints a plain
// line. This makes captured/piped output clean for agents and CI.
//
// Respects NO_COLOR (https://no-color.org/) and CI=1.
func isTTY() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CI") != "" {
		return false
	}
	if os.Getenv("DEW_NO_PROGRESS") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// Spinner displays an animated spinner on stderr.
type Spinner struct {
	mu        sync.Mutex
	label     string
	steps     int
	start     time.Time // creation time, for the final summary
	stepStart time.Time // current-step start, for per-step elapsed display
	done      chan struct{}
	stopped   bool
}

// New creates a spinner. Call Step() to start each phase.
func New() *Spinner {
	return &Spinner{start: time.Now()}
}

// Step starts a new phase. Previous phase gets a ✓ line.
func (s *Spinner) Step(label string) {
	s.finish()
	s.mu.Lock()
	s.steps++
	s.label = label
	s.stepStart = time.Now()
	s.stopped = false
	s.done = make(chan struct{})
	s.mu.Unlock()

	if isTTY() {
		go s.spin()
	} else {
		// Non-TTY: just emit a plain start line, no spinner.
		fmt.Fprintf(os.Stderr, "  %s...\n", label)
	}
}

func (s *Spinner) spin() {
	i := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			frame := frames[i%len(frames)]
			fmt.Fprintf(os.Stderr, "\r  %s %s", s.label, frame)
			s.mu.Unlock()
			i++
		}
	}
}

// finish stops the current spinner and prints ✓ with step elapsed time.
func (s *Spinner) finish() {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	label := s.label
	elapsed := time.Since(s.stepStart)
	ch := s.done
	tty := isTTY()
	s.mu.Unlock()

	dur := formatStepDuration(elapsed)
	if tty {
		close(ch)
		fmt.Fprintf(os.Stderr, "\r\033[K  %s ✓ %s\n", label, dur)
	} else {
		// Non-TTY: spinner goroutine wasn't started, just print ✓.
		fmt.Fprintf(os.Stderr, "  %s ✓ %s\n", label, dur)
	}
}

// Fail stops the current spinner and prints ✗ with step elapsed time.
func (s *Spinner) Fail(reason string) {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	label := s.label
	elapsed := time.Since(s.stepStart)
	ch := s.done
	tty := isTTY()
	s.mu.Unlock()

	dur := formatStepDuration(elapsed)
	if tty {
		close(ch)
		fmt.Fprintf(os.Stderr, "\r\033[K  %s ✗ %s — %s\n", label, dur, reason)
	} else {
		fmt.Fprintf(os.Stderr, "  %s ✗ %s — %s\n", label, dur, reason)
	}
}

// formatStepDuration renders a per-step elapsed time in a short form
// that reads naturally next to the step label: 380ms / 3.2s / 1m12s.
func formatStepDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%ds", m, s)
	}
}

// Stop halts the spinner without printing anything.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	ch := s.done
	s.mu.Unlock()
	close(ch)
}

// Done prints the final summary.
func (s *Spinner) Done(url string) {
	s.finish()
	elapsed := time.Since(s.start)
	fmt.Fprintf(os.Stderr, "\n  ✓ %s\n", url)
	fmt.Fprintf(os.Stderr, "  ⚡ %d steps, %.1fs\n\n", s.steps, elapsed.Seconds())
}

// Timeout prints a timeout summary.
func (s *Spinner) Timeout(url string) {
	s.finish()
	elapsed := time.Since(s.start)
	fmt.Fprintf(os.Stderr, "\n  ? %s — may still be starting\n", url)
	fmt.Fprintf(os.Stderr, "  ⚡ %d steps, %.1fs\n\n", s.steps, elapsed.Seconds())
}
