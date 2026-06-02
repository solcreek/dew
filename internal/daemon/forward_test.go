//go:build darwin

package daemon

import (
	"context"
	"net"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

// stubVM satisfies vm.VM well enough for AddForward/RemoveForward
// tests. The forward acceptance loop calls VsockConnect on each
// new TCP connection — for these tests we never connect, so a
// minimal stub works.
type stubVM struct{}

func (stubVM) Start(context.Context) error                           { return nil }
func (stubVM) Stop(context.Context) error                            { return nil }
func (stubVM) State() vm.State                                       { return vm.StateRunning }
func (stubVM) WaitForState(context.Context, vm.State) error          { return nil }
func (stubVM) VsockConnect(uint32) (net.Conn, error)                 { return nil, nil }

// Adding a forward must actually start a host TCP listener — that's
// the load-bearing observable behavior (the listener is what makes
// curl localhost:PORT work). We don't need to verify the proxy path
// here; just that the listener is bound.
func TestAddForward_StartsListener(t *testing.T) {
	s := &State{VM: stubVM{}}
	addr, err := s.AddForward(0, 8090) // host_port=0 → kernel-assigned, avoids collision
	if err == nil {
		t.Fatal("expected error when host_port=0")
	}
	_ = addr

	// Pick a free port deterministically.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	addr, err = s.AddForward(port, 8090)
	if err != nil {
		t.Fatalf("AddForward: %v", err)
	}
	if addr == "" {
		t.Errorf("expected non-empty listen addr")
	}

	// Confirm we can actually connect (= listener is bound).
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	conn.Close()

	// Cleanup so the next test can re-bind.
	s.RemoveForward(port, 8090)
}

// Idempotent re-add MUST be a no-op (same listener returned), not
// "port already in use". Without this, grove install on an app
// already forwarded would error spuriously.
func TestAddForward_IdempotentOnSamePair(t *testing.T) {
	s := &State{VM: stubVM{}}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	addr1, err := s.AddForward(port, 8090)
	if err != nil {
		t.Fatalf("first AddForward: %v", err)
	}
	addr2, err := s.AddForward(port, 8090)
	if err != nil {
		t.Fatalf("idempotent AddForward: %v", err)
	}
	if addr1 != addr2 {
		t.Errorf("idempotent add returned different addr: %q vs %q", addr1, addr2)
	}
	s.RemoveForward(port, 8090)
}

// RemoveForward closes the listener so the host port is freed.
// Tested by confirming a subsequent net.Listen on the same port
// succeeds.
func TestRemoveForward_ClosesListenerAndFreesPort(t *testing.T) {
	s := &State{VM: stubVM{}}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	if _, err := s.AddForward(port, 8090); err != nil {
		t.Fatalf("AddForward: %v", err)
	}
	if err := s.RemoveForward(port, 8090); err != nil {
		t.Fatalf("RemoveForward: %v", err)
	}
	// Port should be free immediately for re-bind.
	check, err := net.Listen("tcp", "127.0.0.1:"+itoa(port))
	if err != nil {
		t.Errorf("port not freed after remove: %v", err)
		return
	}
	check.Close()
}

// Removing a forward that was never added must NOT error — grove
// uninstall calls it defensively without first checking.
func TestRemoveForward_UnknownPairIsNoop(t *testing.T) {
	s := &State{VM: stubVM{}}
	if err := s.RemoveForward(65432, 8090); err != nil {
		t.Errorf("removing unknown forward errored: %v", err)
	}
}

func TestListForwards_ReturnsActiveEntries(t *testing.T) {
	s := &State{VM: stubVM{}}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	if _, err := s.AddForward(port, 5432); err != nil {
		t.Fatal(err)
	}
	defer s.RemoveForward(port, 5432)

	entries := s.ListForwards()
	if len(entries) != 1 {
		t.Fatalf("ListForwards len = %d, want 1", len(entries))
	}
	if entries[0].HostPort != port || entries[0].GuestPort != 5432 {
		t.Errorf("entry mismatch: %+v", entries[0])
	}
}

// Tiny strconv-free Itoa for a single test path. Avoids the import
// cycle if strconv ever gets bundled into the bootstrap.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
