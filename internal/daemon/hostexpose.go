//go:build darwin

package daemon

import (
	"fmt"
	"io"
	"net"
	"sync"

	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// hostExpose holds the host-side reverse-forward state: the vsock listener
// that accepts guest-initiated dials and the set of host ports the user
// declared with `dew up --expose-host`. Guarded so StopHostExpose can tear
// the listener down cleanly.
type hostExpose struct {
	mu       sync.Mutex
	listener net.Listener
}

// StartHostExpose begins accepting guest reverse-dials for the given host
// ports. The guest (dew-agent) dials ReverseForwardPort once per forwarded
// connection; the host validates the token and that the requested port was
// declared, then proxies to 127.0.0.1:port on macOS. This is the inverse of
// the host→guest port forward — it lets a VM service reach a host process
// bound to 127.0.0.1 (the dev default) with no NAT path and no 0.0.0.0 bind,
// and it never touches the network stack, so the macOS 26 VZ NAT regression
// can't break it. No-op for an empty port list.
func (s *State) StartHostExpose(ports []int) error {
	if len(ports) == 0 {
		return nil
	}
	ln, err := s.VM.VsockListen(vsockProto.ReverseForwardPort)
	if err != nil {
		return fmt.Errorf("host-expose vsock listen: %w", err)
	}
	allow := make(map[int]bool, len(ports))
	for _, p := range ports {
		allow[p] = true
	}

	// Close any listener from a prior StartHostExpose before replacing it, so a
	// second call (e.g. a re-up without full teardown) can't leak the old
	// listener and its accept loop serving with a stale allow-set/token.
	s.expose.mu.Lock()
	if s.expose.listener != nil {
		s.expose.listener.Close()
	}
	s.expose.listener = ln
	s.expose.mu.Unlock()

	go s.acceptReverseDials(ln, allow)
	return nil
}

// acceptReverseDials serves guest-initiated reverse-dial connections until the
// listener is closed.
func (s *State) acceptReverseDials(ln net.Listener, allow map[int]bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by StopHostExpose
		}
		go serveReverseDial(conn, allow, s.Token, dialHostLoopback)
	}
}

// dialHostLoopback dials the macOS host's own loopback — the only place a
// reverse-dial is ever allowed to land.
func dialHostLoopback(port int) (net.Conn, error) {
	return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

// serveReverseDial handles one guest-initiated reverse-dial: read the request,
// enforce the token + allow-set gate (the guest can never make the host dial
// an undeclared port or a non-loopback address), dial the host loopback, and
// proxy bytes both ways. dial is injectable for testing.
func serveReverseDial(guest net.Conn, allow map[int]bool, token string, dial func(port int) (net.Conn, error)) {
	defer guest.Close()

	var req vsockProto.ReverseDialRequest
	if err := vsockProto.ReadJSON(guest, &req); err != nil {
		return
	}
	if req.Token != token {
		vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{Error: "unauthorized"})
		return
	}
	if !allow[req.Port] {
		vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{
			Error: fmt.Sprintf("port %d not exposed", req.Port)})
		return
	}
	host, err := dial(req.Port)
	if err != nil {
		vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{Error: err.Error()})
		return
	}
	defer host.Close()
	if err := vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{OK: true}); err != nil {
		return
	}

	done := make(chan struct{})
	go func() { io.Copy(host, guest); close(done) }()
	io.Copy(guest, host)
	<-done
}

// StopHostExpose closes the reverse-forward listener (if any).
func (s *State) StopHostExpose() {
	s.expose.mu.Lock()
	defer s.expose.mu.Unlock()
	if s.expose.listener != nil {
		s.expose.listener.Close()
		s.expose.listener = nil
	}
}
