//go:build linux

package main

import (
	"fmt"
	"net"
	"testing"
	"time"

	protocol "github.com/solcreek/dew/internal/vsock"
)

// TestExecuteStreaming_ConcurrentStdoutStderr streams heavy stdout and stderr
// at once. Both pump goroutines write frames to the same conn; if a frame is
// not written atomically they interleave and the host's decode fails partway
// (seen in a VM as a JSON decode error and a short line count).
func TestExecuteStreaming_ConcurrentStdoutStderr(t *testing.T) {
	const n = 5000
	agentConn, hostConn := net.Pipe()
	defer hostConn.Close()

	go func() {
		defer agentConn.Close()
		executeStreaming(agentConn, protocol.ExecRequest{
			Command: "/bin/sh",
			Args: []string{"-c", fmt.Sprintf(
				`i=0; while [ $i -lt %d ]; do echo out$i; echo err$i >&2; i=$((i+1)); done`, n)},
			Stream: true,
		})
	}()

	type result struct {
		stdout, stderr, exit int
		err                  error
	}
	resCh := make(chan result, 1)
	go func() {
		r := result{exit: -999}
		for r.err == nil {
			var frame struct {
				Stream   string `json:"stream"`
				ExitCode *int   `json:"exit_code"`
			}
			if err := protocol.ReadJSON(hostConn, &frame); err != nil {
				r.err = err
				break
			}
			if frame.ExitCode != nil {
				r.exit = *frame.ExitCode
				break
			}
			switch frame.Stream {
			case "stdout":
				r.stdout++
			case "stderr":
				r.stderr++
			default:
				r.err = fmt.Errorf("frame with unknown stream %q", frame.Stream)
			}
		}
		resCh <- r
	}()

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("stream corrupted after %d stdout / %d stderr frames: %v", r.stdout, r.stderr, r.err)
		}
		if r.stdout != n || r.stderr != n {
			t.Errorf("got %d stdout / %d stderr frames, want %d each", r.stdout, r.stderr, n)
		}
		if r.exit != 0 {
			t.Errorf("exit = %d, want 0", r.exit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for ExecDone")
	}
}
