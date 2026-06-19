//go:build linux

package main

import (
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	protocol "github.com/solcreek/dew/internal/vsock"
)

// TestExecuteStreaming_TTY verifies that TTY mode allocates a real
// pseudo-terminal in the guest: `test -t 0` is true (isatty) and
// `stty size` reports the window size we requested. Output is base64
// in TTY mode. Linux-only; runs on the CI build host.
func TestExecuteStreaming_TTY(t *testing.T) {
	agentConn, hostConn := net.Pipe()

	go executeStreaming(agentConn, protocol.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "stty size; test -t 0 && echo IS_TTY"},
		Stream:  true, Stdin: true, TTY: true, Rows: 24, Cols: 80,
	})

	resCh := make(chan string, 1)
	go func() {
		var out []byte
		for {
			var frame struct {
				Stream   string `json:"stream"`
				Data     string `json:"data"`
				ExitCode int    `json:"exit_code"`
			}
			if err := protocol.ReadJSON(hostConn, &frame); err != nil {
				break
			}
			if frame.Stream != "" {
				b, _ := base64.StdEncoding.DecodeString(frame.Data)
				out = append(out, b...)
				continue
			}
			break // ExecDone
		}
		resCh <- string(out)
	}()

	select {
	case out := <-resCh:
		if !strings.Contains(out, "IS_TTY") {
			t.Errorf("guest does not see a tty: %q", out)
		}
		if !strings.Contains(out, "24 80") {
			t.Errorf("window size not applied (want \"24 80\"): %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — pty path likely wrong")
	}
}
