//go:build darwin

package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vm"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// readResp reads a ReverseDialResponse off conn within a short deadline so a
// hung handler fails the test instead of blocking forever.
func readResp(t *testing.T, conn net.Conn) vsockProto.ReverseDialResponse {
	t.Helper()
	type res struct {
		r   vsockProto.ReverseDialResponse
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var r vsockProto.ReverseDialResponse
		err := vsockProto.ReadJSON(conn, &r)
		ch <- res{r, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read ReverseDialResponse: %v", got.err)
		}
		return got.r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ReverseDialResponse")
		return vsockProto.ReverseDialResponse{}
	}
}

// The happy path: an authorized request for an exposed port dials the host
// loopback and proxies bytes both ways.
func TestServeReverseDial_ProxiesToLoopback(t *testing.T) {
	// A loopback echo server stands in for the macOS host process.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port

	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	go serveReverseDial(guestServer, map[int]bool{echoPort: true}, "tok", dialHostLoopback)

	if err := vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeReverseDial, Token: "tok", Port: echoPort,
	}); err != nil {
		t.Fatal(err)
	}
	if resp := readResp(t, guestClient); !resp.OK {
		t.Fatalf("response not OK: %+v", resp)
	}

	// Bytes after the response are a raw proxied stream.
	if _, err := guestClient.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	guestClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(guestClient, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want ping", buf)
	}
}

// A wrong token is rejected before any dial — the guest can't impersonate.
func TestServeReverseDial_RejectsBadToken(t *testing.T) {
	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	dialed := false
	go serveReverseDial(guestServer, map[int]bool{50051: true}, "real", func(int) (net.Conn, error) {
		dialed = true
		return nil, nil
	})

	vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeReverseDial, Token: "wrong", Port: 50051,
	})
	resp := readResp(t, guestClient)
	if resp.OK || resp.Error != "unauthorized" {
		t.Errorf("got %+v, want unauthorized", resp)
	}
	if dialed {
		t.Error("dialed despite bad token")
	}
}

// An undeclared port is refused — the allow-set is the containment boundary,
// so the guest can never make the host dial an arbitrary loopback port.
func TestServeReverseDial_RejectsUndeclaredPort(t *testing.T) {
	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	dialed := false
	go serveReverseDial(guestServer, map[int]bool{50051: true}, "tok", func(int) (net.Conn, error) {
		dialed = true
		return nil, nil
	})

	vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeReverseDial, Token: "tok", Port: 9999,
	})
	resp := readResp(t, guestClient)
	if resp.OK || resp.Error == "" {
		t.Errorf("got %+v, want a 'not exposed' error", resp)
	}
	if dialed {
		t.Error("dialed an undeclared port")
	}
}

// A dial failure (host service down) is surfaced to the guest, not hung.
func TestServeReverseDial_SurfacesDialError(t *testing.T) {
	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	go serveReverseDial(guestServer, map[int]bool{50051: true}, "tok", func(int) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})

	vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeReverseDial, Token: "tok", Port: 50051,
	})
	resp := readResp(t, guestClient)
	if resp.OK || resp.Error == "" {
		t.Errorf("got %+v, want the dial error surfaced", resp)
	}
}

// A request whose type isn't reverse_dial is rejected before any gate — fail
// closed against a stray message shape that happens to carry a token/port.
func TestServeReverseDial_RejectsBadType(t *testing.T) {
	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	go serveReverseDial(guestServer, map[int]bool{50051: true}, "tok", func(int) (net.Conn, error) {
		t.Error("dialed despite bad request type")
		return nil, nil
	})
	vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeExec, Token: "tok", Port: 50051,
	})
	if resp := readResp(t, guestClient); resp.OK || resp.Error == "" {
		t.Errorf("got %+v, want a bad-type error", resp)
	}
}

// An out-of-range port is rejected before the allow-set/token gates.
func TestServeReverseDial_RejectsOutOfRangePort(t *testing.T) {
	guestClient, guestServer := net.Pipe()
	defer guestClient.Close()
	go serveReverseDial(guestServer, map[int]bool{50051: true}, "tok", func(int) (net.Conn, error) {
		t.Error("dialed an out-of-range port")
		return nil, nil
	})
	vsockProto.WriteJSON(guestClient, &vsockProto.ReverseDialRequest{
		Type: vsockProto.TypeReverseDial, Token: "tok", Port: 70000,
	})
	if resp := readResp(t, guestClient); resp.OK || resp.Error == "" {
		t.Errorf("got %+v, want an out-of-range error", resp)
	}
}

// listenerVM is a vm.VM whose VsockListen hands out real loopback listeners so
// the reverse-forward lifecycle is testable without Virtualization.framework.
type listenerVM struct{ handed []net.Listener }

func (v *listenerVM) Start(context.Context) error                  { return nil }
func (v *listenerVM) Stop(context.Context) error                   { return nil }
func (v *listenerVM) State() vm.State                              { return vm.StateRunning }
func (v *listenerVM) WaitForState(context.Context, vm.State) error { return nil }
func (v *listenerVM) VsockConnect(uint32) (net.Conn, error)        { return nil, errors.New("no vsock") }
func (v *listenerVM) VsockListen(uint32) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	v.handed = append(v.handed, ln)
	return ln, nil
}

// A second StartHostExpose must close the prior listener — otherwise the old
// accept loop leaks and keeps serving with a stale allow-set/token.
func TestStartHostExpose_ClosesPriorListener(t *testing.T) {
	fv := &listenerVM{}
	s := &State{VM: fv, Token: "tok"}
	defer s.StopHostExpose()

	if err := s.StartHostExpose([]int{50051}); err != nil {
		t.Fatalf("first StartHostExpose: %v", err)
	}
	if err := s.StartHostExpose([]int{50052}); err != nil {
		t.Fatalf("second StartHostExpose: %v", err)
	}
	if len(fv.handed) != 2 {
		t.Fatalf("handed %d listeners, want 2", len(fv.handed))
	}
	// The first listener must be closed: Accept returns immediately with err.
	done := make(chan error, 1)
	go func() { _, err := fv.handed[0].Accept(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("prior listener still accepting; StartHostExpose leaked it")
		}
	case <-time.After(2 * time.Second):
		t.Error("prior listener was not closed (Accept still blocking)")
	}
}
