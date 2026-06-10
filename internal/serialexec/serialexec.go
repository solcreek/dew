// Package serialexec provides command execution inside a Dew VM via
// the serial console. Fallback when vsock is not available.
package serialexec

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const sentinel = "<<DEW_DONE>>"

// defaultRunTimeout bounds Run when the guest stops responding
// mid-command. Overridable per-Exec via the Timeout field.
const defaultRunTimeout = 30 * time.Second

// Exec wraps a serial console pipe pair for command execution.
//
// A single reader goroutine (started in New) owns the console output:
// it pumps lines into a channel, latches guest readiness, and keeps the
// console drained so the guest never blocks on a full pipe. WaitReady
// and Run select on that channel against a timer, so a guest that goes
// silent produces a timeout error instead of a hang — previously both
// blocked forever inside bufio ReadString, and concurrent callers raced
// on the shared reader.
type Exec struct {
	w io.Writer

	// Timeout bounds a single Run round-trip (command write to
	// sentinel). Zero means defaultRunTimeout.
	Timeout time.Duration

	mu sync.Mutex // serializes Run

	// capturing gates the lines channel: outside an active Run the
	// reader discards console output (boot logs, prompts) so stale
	// noise never leaks into a command's captured stdout.
	capturing atomic.Bool

	lines   chan string   // console output during Run, line at a time
	ready   chan struct{} // closed when the ready marker is seen
	readyMu sync.Mutex    // guards close(ready)
	done    chan struct{} // closed when the reader exits
	readErr error         // set before done is closed
}

// New creates an Exec from the host-side pipe ends. r reads guest
// output; w writes guest input. The reader goroutine runs until r
// errors (typically when the caller closes the console pipe).
func New(r io.Reader, w io.Writer) *Exec {
	s := &Exec{
		w:     w,
		lines: make(chan string, 256),
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.readLoop(bufio.NewReaderSize(r, 64*1024))
	return s
}

func (s *Exec) readLoop(r *bufio.Reader) {
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			if strings.Contains(line, "dew vm ready") || strings.Contains(line, "~ #") {
				s.latchReady()
			}
			if s.capturing.Load() {
				s.lines <- line
			}
		}
		if err != nil {
			s.readErr = err
			close(s.done)
			return
		}
	}
}

func (s *Exec) latchReady() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
}

// WaitReady blocks until the guest shell prompt appears on the serial
// console (looks for "dew vm ready" or "~ #"), the console errors out,
// or the timeout elapses. Readiness latches: once seen, subsequent
// calls return immediately.
func (s *Exec) WaitReady(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		return nil
	case <-s.done:
		// Reader exited; the marker may have been latched on its
		// final lines — check before reporting the error.
		select {
		case <-s.ready:
			return nil
		default:
		}
		return fmt.Errorf("serialexec: wait ready: %w", s.readErr)
	case <-timer.C:
		return fmt.Errorf("serialexec: timeout waiting for guest ready")
	}
}

// Run sends a command and returns its stdout. Uses a sentinel to
// detect end of output.
func (s *Exec) Run(cmd string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}

	// Capture console lines only between command write and sentinel;
	// drain leftovers a previous timed-out Run may have abandoned.
	s.capturing.Store(true)
	defer s.capturing.Store(false)
	for {
		select {
		case <-s.lines:
			continue
		default:
		}
		break
	}

	line := fmt.Sprintf("%s; echo '%s' $?\n", cmd, sentinel)
	if _, err := io.WriteString(s.w, line); err != nil {
		return "", -1, fmt.Errorf("serialexec: write: %w", err)
	}

	var output strings.Builder
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		var text string
		select {
		case text = <-s.lines:
		case <-s.done:
			// Reader exited; consume anything it already buffered
			// before giving up.
			select {
			case text = <-s.lines:
			default:
				return output.String(), -1, fmt.Errorf("serialexec: read: %w", s.readErr)
			}
		case <-timer.C:
			return output.String(), -1, fmt.Errorf("serialexec: timeout")
		}

		text = strings.TrimRight(text, "\r\n")

		// Skip echo of our command (shell echoes the input line)
		if strings.Contains(text, sentinel) && strings.Contains(text, cmd) {
			continue
		}

		// Sentinel line: "<<DEW_DONE>> <exitcode>"
		if strings.HasPrefix(text, sentinel) {
			exitCode := 0
			parts := strings.Fields(text)
			if len(parts) >= 2 {
				for _, c := range parts[1] {
					if c >= '0' && c <= '9' {
						exitCode = exitCode*10 + int(c-'0')
					}
				}
			}
			return strings.TrimRight(output.String(), "\n"), exitCode, nil
		}

		// Skip shell prompt lines
		trimmed := strings.TrimSpace(text)
		if trimmed == "~ #" || trimmed == "/ #" || trimmed == "" {
			continue
		}

		output.WriteString(text)
		output.WriteString("\n")
	}
}
