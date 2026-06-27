//go:build darwin

package daemon

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// hostDialTimeout bounds a single reverse-dial to the host loopback so a
// stalled connect can't hang the handler goroutine. Matches the agent's
// guest-side handleConnect timeout.
const hostDialTimeout = 5 * time.Second

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
	allow := make(map[int]bool, len(ports))
	for _, p := range ports {
		allow[p] = true
	}

	// Close any listener from a prior StartHostExpose *before* binding, then
	// hold the lock across close+bind+store. ReverseForwardPort is a single
	// fixed port (1025), so a second call (e.g. a re-up without full teardown)
	// would otherwise fail to bind ("address in use") if we listened first —
	// and never reach the close/replace path, leaking the old accept loop still
	// serving a stale allow-set/token.
	s.expose.mu.Lock()
	defer s.expose.mu.Unlock()
	if s.expose.listener != nil {
		s.expose.listener.Close()
		s.expose.listener = nil
	}
	ln, err := s.VM.VsockListen(vsockProto.ReverseForwardPort)
	if err != nil {
		return fmt.Errorf("host-expose vsock listen: %w", err)
	}
	s.expose.listener = ln

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
// reverse-dial is ever allowed to land — with a bounded timeout so a dead
// host service fails fast instead of pinning the handler goroutine.
func dialHostLoopback(port int) (net.Conn, error) {
	return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), hostDialTimeout)
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
	// Fail closed: only a well-formed reverse_dial for an in-range port is
	// even considered, before the token/allow-set gates.
	if req.Type != vsockProto.TypeReverseDial {
		vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{Error: "bad request type"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		vsockProto.WriteJSON(guest, &vsockProto.ReverseDialResponse{Error: "port out of range"})
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
