// Package progress provides a terminal spinner with elapsed time
// and step counter for CLI feedback.
package progress

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner displays an animated spinner with elapsed time on stderr.
type Spinner struct {
	mu      sync.Mutex
	step    int
	total   int
	label   string
	start   time.Time
	done    chan struct{}
	stopped bool
}

// New creates a spinner with the given total step count.
func New(totalSteps int) *Spinner {
	return &Spinner{total: totalSteps}
}

// Step starts a new step with the given label. Stops the previous
// spinner if running.
func (s *Spinner) Step(label string) {
	s.Stop()
	s.mu.Lock()
	s.step++
	s.label = label
	s.start = time.Now()
	s.stopped = false
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.spin()
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
			elapsed := time.Since(s.start)
			frame := frames[i%len(frames)]
			fmt.Fprintf(os.Stderr, "\r  [%d/%d] %s %s %.1fs",
				s.step, s.total, s.label, frame, elapsed.Seconds())
			s.mu.Unlock()
			i++
		}
	}
}

// Stop halts the current spinner and prints the final elapsed time.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	elapsed := time.Since(s.start)
	step := s.step
	total := s.total
	label := s.label
	ch := s.done
	s.mu.Unlock()

	close(ch)
	fmt.Fprintf(os.Stderr, "\r  [%d/%d] %s ✓ %.1fs\n", step, total, label, elapsed.Seconds())
}

// Fail halts the current spinner and prints a failure indicator.
func (s *Spinner) Fail(reason string) {
	s.mu.Lock()
	if s.stopped || s.done == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	elapsed := time.Since(s.start)
	step := s.step
	total := s.total
	label := s.label
	ch := s.done
	s.mu.Unlock()

	close(ch)
	fmt.Fprintf(os.Stderr, "\r  [%d/%d] %s ✗ %.1fs — %s\n", step, total, label, elapsed.Seconds(), reason)
}

// Done prints the final summary line.
func (s *Spinner) Done(url string, totalElapsed time.Duration) {
	s.Stop()
	fmt.Fprintf(os.Stderr, "\n  ✓ %s (%.1fs)\n\n", url, totalElapsed.Seconds())
}

// Timeout prints a timeout summary.
func (s *Spinner) Timeout(url string, totalElapsed time.Duration) {
	s.Stop()
	fmt.Fprintf(os.Stderr, "\n  ? %s — may still be starting (%.1fs)\n\n", url, totalElapsed.Seconds())
}
