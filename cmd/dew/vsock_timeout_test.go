//go:build darwin

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vm"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// fakeVM implements vm.VM with a pluggable VsockConnect, so the
// CLI-side deadline logic is testable without Virtualization.framework.
type fakeVM struct {
	connect func(port uint32) (net.Conn, error)
}

func (f *fakeVM) Start(ctx context.Context) error                       { return nil }
func (f *fakeVM) Stop(ctx context.Context) error                        { return nil }
func (f *fakeVM) State() vm.State                                       { return vm.StateRunning }
func (f *fakeVM) WaitForState(ctx context.Context, t vm.State) error    { return nil }
func (f *fakeVM) VsockConnect(port uint32) (net.Conn, error)            { return f.connect(port) }
func (f *fakeVM) VsockListen(uint32) (net.Listener, error)              { return nil, errors.New("no vsock listener") }

// The regression test for the `dew run` hang: a VsockConnect that
// never returns (vz against a guest with no vsock transport) must
// surface as an error at the deadline, not block forever.
func TestConnectVsockDeadline_BlockingConnect(t *testing.T) {
	v := &fakeVM{connect: func(uint32) (net.Conn, error) {
		select {} // block forever
	}}

	start := time.Now()
	_, err := connectVsockDeadline(v, 1024, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~150ms", elapsed)
	}
}

// Fast-failing connects must retry until the wall-clock deadline —
// and the deadline is total elapsed time, so even thousands of
// instant failures cannot stretch the wait (the old count-based loop
// turned "60s" into ~51 minutes when each attempt took 5s).
func TestConnectVsockDeadline_FastFailureBoundedByWallClock(t *testing.T) {
	var attempts atomic.Int64
	v := &fakeVM{connect: func(uint32) (net.Conn, error) {
		attempts.Add(1)
		return nil, errors.New("connection reset")
	}}

	start := time.Now()
	_, err := connectVsockDeadline(v, 1024, 150*time.Millisecond)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~150ms", elapsed)
	}
	if attempts.Load() < 2 {
		t.Errorf("attempts = %d, want retries before the deadline", attempts.Load())
	}
	// The last underlying error must be preserved for diagnosis.
	if err != nil && !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error %q should wrap the dial error", err)
	}
}

// A connect that succeeds after transient failures must return the
// conn — deadline conversion must not break the happy retry path.
func TestConnectVsockDeadline_EventualSuccess(t *testing.T) {
	var attempts atomic.Int64
	want, peer := net.Pipe()
	defer peer.Close()
	v := &fakeVM{connect: func(uint32) (net.Conn, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("not yet")
		}
		return want, nil
	}}

	got, err := connectVsockDeadline(v, 1024, 2*time.Second)
	if err != nil {
		t.Fatalf("connectVsockDeadline: %v", err)
	}
	if got != want {
		t.Error("returned conn is not the dialed conn")
	}
	got.Close()
}

// vz returns a typed-nil *VirtioSocketConnection alongside its error;
// passed through net.Conn that compares non-nil, and Close() on it
// panics. The reaper after a timed-out attempt must not crash the
// process on such a result (a panic here would kill this test binary).
func TestVsockConnectAttempt_TypedNilConnNoPanic(t *testing.T) {
	v := &fakeVM{connect: func(uint32) (net.Conn, error) {
		time.Sleep(100 * time.Millisecond) // outlive the attempt timeout
		var c *net.TCPConn                 // typed nil, like vz on error
		return c, errors.New("connection reset")
	}}

	_, err := vsockConnectAttempt(v, 1024, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Give the reaper goroutine time to receive the late typed-nil
	// result; if it called Close on it, the whole test process dies.
	time.Sleep(300 * time.Millisecond)
}

// execVsockExec must not hang when the agent dies after accepting the
// request: the response read is bounded by guest budget + grace.
func TestExecVsockExec_SilentAgentTimesOut(t *testing.T) {
	oldGrace := hostReadGrace
	hostReadGrace = 50 * time.Millisecond
	defer func() { hostReadGrace = oldGrace }()

	client, server := net.Pipe()
	defer server.Close()

	// Server reads the request then goes silent (agent died mid-exec).
	go func() {
		var req vsockProto.ExecRequest
		vsockProto.ReadJSON(server, &req)
	}()

	start := time.Now()
	_, err := execVsockExec(client, "tok", "uname", []string{"-a"}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s, want bounded by guest budget + grace", elapsed)
	}
}

func TestExecVsockExec_AgentReplies(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	go func() {
		var req vsockProto.ExecRequest
		if err := vsockProto.ReadJSON(server, &req); err != nil {
			return
		}
		vsockProto.WriteJSON(server, &vsockProto.ExecResponse{
			ExitCode: 0, Stdout: "Linux dew 6.18.15\n",
		})
	}()

	result, err := execVsockExec(client, "tok", "uname", []string{"-a"}, 0)
	if err != nil {
		t.Fatalf("execVsockExec: %v", err)
	}
	if result.Stdout != "Linux dew 6.18.15\n" || result.ExitCode != 0 {
		t.Errorf("result = %+v", result)
	}
}

func TestHostReadBudget(t *testing.T) {
	if got := hostReadBudget(0); got != 30*time.Second+hostReadGrace {
		t.Errorf("hostReadBudget(0) = %s, want agent default 30s + grace", got)
	}
	if got := hostReadBudget(5 * time.Minute); got != 5*time.Minute+hostReadGrace {
		t.Errorf("hostReadBudget(5m) = %s, want 5m + grace", got)
	}
}

