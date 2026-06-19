//go:build darwin

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vm"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// fakeVM hands handleExec a pre-wired vsock conn (one end of a pipe);
// the test drives the other end as a stand-in guest agent.
type fakeVM struct{ guest net.Conn }

func (f *fakeVM) Start(context.Context) error                  { return nil }
func (f *fakeVM) Stop(context.Context) error                   { return nil }
func (f *fakeVM) State() vm.State                              { var s vm.State; return s }
func (f *fakeVM) WaitForState(context.Context, vm.State) error { return nil }
func (f *fakeVM) VsockConnect(uint32) (net.Conn, error)        { return f.guest, nil }

func TestBuildVsockExec_ForwardsStdinFlag(t *testing.T) {
	got := buildVsockExec(ExecRequest{Argv: []string{"/bin/sh"}, Stream: true, Stdin: true}, "tok")
	if !got.Stdin || !got.Stream {
		t.Errorf("Stdin/Stream not propagated: %+v", got)
	}
	got = buildVsockExec(ExecRequest{Command: "echo hi"}, "tok")
	if got.Stdin {
		t.Errorf("Stdin should default false: %+v", got)
	}
}

// TestHandleExec_StreamingStdin exercises the full daemon relay: client
// stdin frames go unix→vsock, and guest output frames go vsock→unix,
// concurrently, on one connection. A fake guest echoes stdin back as
// stdout, then ExecDone on EOF.
func TestHandleExec_StreamingStdin(t *testing.T) {
	guestDaemon, guestAgent := net.Pipe() // daemon ↔ (fake) guest agent
	cliConn, daemonConn := net.Pipe()     // CLI ↔ daemon (unix conn stand-in)

	s := &State{VM: &fakeVM{guest: guestDaemon}, Token: "tok", VsockPort: 1024}

	// Fake guest agent: consume the exec request, echo each stdin chunk
	// back as stdout, finish with ExecDone when stdin closes.
	go func() {
		var req vsockProto.ExecRequest
		if err := vsockProto.ReadJSON(guestAgent, &req); err != nil {
			return
		}
		for {
			var in vsockProto.InputChunk
			if err := vsockProto.ReadJSON(guestAgent, &in); err != nil {
				return
			}
			if in.Data != "" {
				_ = vsockProto.WriteJSON(guestAgent, &vsockProto.OutputChunk{Stream: "stdout", Data: "echo:" + in.Data})
			}
			if in.EOF {
				_ = vsockProto.WriteJSON(guestAgent, &vsockProto.ExecDone{ExitCode: 0})
				return
			}
		}
	}()

	// Daemon under test.
	go s.handleExec(daemonConn, json.NewDecoder(daemonConn), ExecRequest{
		Argv: []string{"/bin/sh"}, Stream: true, Stdin: true,
	})

	// CLI: push stdin frames.
	go func() {
		enc := json.NewEncoder(cliConn)
		_ = enc.Encode(vsockProto.InputChunk{Data: "hello\n"})
		_ = enc.Encode(vsockProto.InputChunk{EOF: true})
	}()

	// CLI: collect relayed output until ExecDone, with a watchdog.
	type result struct {
		stdout string
		exit   int
	}
	resCh := make(chan result, 1)
	go func() {
		r := bufio.NewReader(cliConn)
		var out strings.Builder
		exit := -999
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var chunk vsockProto.OutputChunk
				if json.Unmarshal(line, &chunk) == nil && chunk.Stream != "" {
					out.WriteString(chunk.Data)
				} else {
					var done vsockProto.ExecDone
					_ = json.Unmarshal(line, &done)
					exit = done.ExitCode
					break
				}
			}
			if err != nil {
				break
			}
		}
		resCh <- result{out.String(), exit}
	}()

	select {
	case res := <-resCh:
		if !strings.Contains(res.stdout, "echo:hello") {
			t.Errorf("stdin not relayed to guest / output not relayed back: stdout=%q", res.stdout)
		}
		if res.exit != 0 {
			t.Errorf("exit = %d, want 0", res.exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — relay likely deadlocked")
	}
}
