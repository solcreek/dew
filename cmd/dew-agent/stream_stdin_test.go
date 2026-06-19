//go:build linux

package main

import (
	"net"
	"strings"
	"testing"
	"time"

	protocol "github.com/solcreek/dew/internal/vsock"
)

// TestExecuteStreaming_Stdin verifies the guest agent feeds InputChunk
// frames into the process's stdin and streams its stdout back. `cat`
// echoes stdin to stdout, so sending "hi\n" then EOF must come back as
// an OutputChunk and then an ExecDone. Linux-only: it runs a real
// process and is exercised on the CI build host, not on the macOS dev
// machine (which can't build the guest agent anyway).
func TestExecuteStreaming_Stdin(t *testing.T) {
	agentConn, hostConn := net.Pipe()

	go executeStreaming(agentConn, protocol.ExecRequest{
		Command: "cat", Stream: true, Stdin: true,
	})

	// Host: push stdin, then EOF.
	go func() {
		_ = protocol.WriteJSON(hostConn, &protocol.InputChunk{Data: "hi\n"})
		_ = protocol.WriteJSON(hostConn, &protocol.InputChunk{EOF: true})
	}()

	// Host: read frames until ExecDone.
	type result struct {
		stdout string
		exit   int
	}
	resCh := make(chan result, 1)
	go func() {
		var out strings.Builder
		exit := -999
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
				out.WriteString(frame.Data)
				continue
			}
			exit = frame.ExitCode // ExecDone
			break
		}
		resCh <- result{out.String(), exit}
	}()

	select {
	case res := <-resCh:
		if !strings.Contains(res.stdout, "hi") {
			t.Errorf("stdin not echoed via guest stdout: stdout=%q", res.stdout)
		}
		if res.exit != 0 {
			t.Errorf("exit = %d, want 0", res.exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — agent stdin wiring likely wrong")
	}
}
