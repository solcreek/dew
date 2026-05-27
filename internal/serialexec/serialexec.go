// Package serialexec provides command execution inside a Dew VM via
// the serial console. Fallback when vsock is not available.
package serialexec

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const sentinel = "<<DEW_DONE>>"

// Exec wraps a serial console pipe pair for command execution.
type Exec struct {
	w  io.Writer
	r  *bufio.Reader
	mu sync.Mutex
}

// New creates an Exec from the host-side pipe ends. r reads guest
// output; w writes guest input.
func New(r io.Reader, w io.Writer) *Exec {
	return &Exec{
		w: w,
		r: bufio.NewReaderSize(r, 64*1024),
	}
}

// WaitReady blocks until the guest shell prompt appears on the serial
// console (looks for "dew vm ready" or "~ #" within timeout).
func (s *Exec) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("serialexec: wait ready: %w", err)
		}
		if strings.Contains(line, "dew vm ready") || strings.Contains(line, "~ #") {
			return nil
		}
	}
	return fmt.Errorf("serialexec: timeout waiting for guest ready")
}

// Run sends a command and returns its stdout. Uses a sentinel to
// detect end of output.
func (s *Exec) Run(cmd string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	line := fmt.Sprintf("%s; echo '%s' $?\n", cmd, sentinel)
	if _, err := io.WriteString(s.w, line); err != nil {
		return "", -1, fmt.Errorf("serialexec: write: %w", err)
	}

	var output strings.Builder
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		text, err := s.r.ReadString('\n')
		if err != nil {
			return output.String(), -1, fmt.Errorf("serialexec: read: %w", err)
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

	return output.String(), -1, fmt.Errorf("serialexec: timeout")
}
