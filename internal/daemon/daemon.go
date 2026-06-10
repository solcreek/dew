//go:build darwin

// Package daemon provides a Unix socket API for cross-process
// access to a running Dew VM. Enables `dew exec` from any terminal
// to run commands in a VM started by `dew start`.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/solcreek/dew/internal/vm"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// State holds the daemon's runtime state.
type State struct {
	VM         vm.VM
	Token      string
	VsockPort  uint32
	SocketPath string

	listener net.Listener
	mu       sync.Mutex

	// Port forwards live as long as the daemon does. The daemon
	// owns them so a separate CLI invocation (`dew forward add ...`)
	// can register a new one against the same VM without restarting.
	forwardMu sync.Mutex
	forwards  map[string]*ForwardEntry
}

// ForwardEntry records a live host→guest TCP forward. Keyed by
// "host:guest" in State.forwards so duplicate adds are idempotent
// and forward-remove can find the listener.
//
// We bind BOTH 127.0.0.1 (IPv4) and ::1 (IPv6) per forward because
// `localhost` on macOS resolves to both — curl/Safari/browsers
// flip between legs unpredictably, and an IPv4-only forward sends
// every IPv6 request to whatever else happens to be on that port
// (or returns refused). Keeping both listeners in the same entry
// means RemoveForward tears them down atomically.
type ForwardEntry struct {
	HostPort  int
	GuestPort int
	listener  net.Listener // IPv4 loopback (127.0.0.1)
	listener6 net.Listener // IPv6 loopback ([::1]) — nil if bind failed
}

// ExecRequest is received from CLI clients over the Unix socket.
//
// Two execution modes:
//   - Command set (legacy / shell mode): treated as a string passed
//     to /bin/sh -c. Use when the user wrote a single shell string
//     with metacharacters: dew exec "echo a; echo b".
//   - Argv set: treated as direct argv; argv[0] is the program,
//     argv[1:] are its arguments. No shell wrap. Use when the user
//     structured their input as argv: dew exec sh -c 'echo a; echo b'.
//
// When both are set, Argv wins. Older clients that only know about
// Command still work without changes.
type ExecRequest struct {
	// Kind dispatches inside the daemon. Empty string keeps the
	// pre-v0.7.22 wire format compatible (treated as "exec"); newer
	// clients can request "forward-add", "forward-remove", or
	// "forward-list" to manage runtime port forwards without
	// restarting the VM.
	Kind string `json:"kind,omitempty"`

	// exec mode
	Command   string   `json:"command,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Stream    bool     `json:"stream,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`

	// forward mode (kind = "forward-*")
	HostPort  int `json:"host_port,omitempty"`
	GuestPort int `json:"guest_port,omitempty"`
}

// SocketDir returns the directory for daemon sockets.
func SocketDir() string {
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".local", "state", "dew")
}

// SocketPath returns the socket path for a given VM name.
func SocketPath(name string) string {
	if name == "" {
		name = "default"
	}
	return filepath.Join(SocketDir(), name+".sock")
}

// buildVsockExec translates a daemon.ExecRequest into the
// vsockProto.ExecRequest the guest agent expects. Pure function so it
// can be unit-tested without a VM or a socket.
//
// Argv mode (req.Argv non-empty): direct argv exec. Argv[0] is the
// program, Argv[1:] are its args. No shell wrap.
//
// Shell mode (req.Command set, Argv empty): /bin/sh -c <Command>.
// Legacy path for clients that send a single shell string.
func buildVsockExec(req ExecRequest, token string) vsockProto.ExecRequest {
	if len(req.Argv) > 0 {
		return vsockProto.ExecRequest{
			Token:     token,
			Command:   req.Argv[0],
			Args:      req.Argv[1:],
			Stream:    req.Stream,
			TimeoutMs: req.TimeoutMs,
		}
	}
	return vsockProto.ExecRequest{
		Token:     token,
		Command:   "/bin/sh",
		Args:      []string{"-c", req.Command},
		Stream:    req.Stream,
		TimeoutMs: req.TimeoutMs,
	}
}

// Start begins listening on a Unix socket, proxying exec requests
// to the VM via vsock.
func (s *State) Start() error {
	os.MkdirAll(filepath.Dir(s.SocketPath), 0700)
	os.Remove(s.SocketPath)

	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", s.SocketPath, err)
	}
	s.listener = ln

	go s.serve()
	return nil
}

// Stop closes the Unix socket and cleans up.
func (s *State) Stop() {
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.SocketPath)
}

func (s *State) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleClient(conn)
	}
}

func (s *State) handleClient(conn net.Conn) {
	defer conn.Close()

	var req ExecRequest
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&req); err != nil {
		json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
		return
	}

	switch req.Kind {
	case "", "exec":
		s.handleExec(conn, req)
	case "forward-add":
		s.handleForwardAdd(conn, req)
	case "forward-remove":
		s.handleForwardRemove(conn, req)
	case "forward-list":
		s.handleForwardList(conn)
	default:
		json.NewEncoder(conn).Encode(map[string]string{"error": "unknown kind: " + req.Kind})
	}
}

// AddForward spawns a host TCP listener that proxies into the
// guest via vsock. Idempotent on (host,guest) pairs — re-adding the
// same forward is a no-op rather than an error so callers (initial
// dew start AND runtime forward-add) can share one entry point.
//
// Returns the resolved listening address (host:port) so the caller
// can confirm what was actually bound.
func (s *State) AddForward(hostPort, guestPort int) (string, error) {
	if hostPort == 0 || guestPort == 0 {
		return "", fmt.Errorf("forward: host_port and guest_port required")
	}
	key := fmt.Sprintf("%d:%d", hostPort, guestPort)

	s.forwardMu.Lock()
	if s.forwards == nil {
		s.forwards = map[string]*ForwardEntry{}
	}
	if existing, ok := s.forwards[key]; ok {
		s.forwardMu.Unlock()
		return existing.listener.Addr().String(), nil
	}
	s.forwardMu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return "", fmt.Errorf("forward listen %d: %w", hostPort, err)
	}
	// IPv6 leg is best-effort. Some sandboxed environments have ::1
	// disabled — we still want the IPv4 path to work in that case.
	// On macOS dev machines ::1 binds fine and the dual-stack pairing
	// kills the "localhost flips to wrong process" bug class.
	ln6, err6 := net.Listen("tcp", fmt.Sprintf("[::1]:%d", hostPort))
	if err6 != nil {
		fmt.Fprintf(os.Stderr, "dew: forward IPv6 leg unavailable for :%d (%v); IPv4 only\n", hostPort, err6)
		ln6 = nil
	}

	entry := &ForwardEntry{HostPort: hostPort, GuestPort: guestPort, listener: ln, listener6: ln6}
	s.forwardMu.Lock()
	s.forwards[key] = entry
	s.forwardMu.Unlock()

	acceptLoop := func(l net.Listener) {
		for {
			tcpConn, err := l.Accept()
			if err != nil {
				return
			}
			go s.proxyToGuest(tcpConn, guestPort)
		}
	}
	go acceptLoop(ln)
	if ln6 != nil {
		go acceptLoop(ln6)
	}
	return ln.Addr().String(), nil
}

// RemoveForward closes the host listener for the given pair. The
// forward goroutine exits on the resulting Accept error. Returns
// nil if no such forward existed — callers (grove uninstall, etc.)
// can call defensively without first checking.
func (s *State) RemoveForward(hostPort, guestPort int) error {
	key := fmt.Sprintf("%d:%d", hostPort, guestPort)
	s.forwardMu.Lock()
	entry, ok := s.forwards[key]
	if ok {
		delete(s.forwards, key)
	}
	s.forwardMu.Unlock()
	if !ok {
		return nil
	}
	err := entry.listener.Close()
	if entry.listener6 != nil {
		// Always close both; surface only the first error.
		if e6 := entry.listener6.Close(); e6 != nil && err == nil {
			err = e6
		}
	}
	return err
}

// ListForwards returns a snapshot of active forwards. Order is not
// guaranteed; callers wanting deterministic output sort by
// HostPort themselves.
func (s *State) ListForwards() []ForwardEntry {
	s.forwardMu.Lock()
	defer s.forwardMu.Unlock()
	out := make([]ForwardEntry, 0, len(s.forwards))
	for _, e := range s.forwards {
		out = append(out, ForwardEntry{HostPort: e.HostPort, GuestPort: e.GuestPort})
	}
	return out
}

// proxyToGuest is the per-connection bridge: read from the host
// TCP conn, push over vsock to the guest agent's TCP-proxy port,
// copy bytes both ways. Mirrors the original cmd/dew implementation
// that lived next to startPortForwards.
func (s *State) proxyToGuest(tcpConn net.Conn, guestPort int) {
	defer tcpConn.Close()
	vsockConn, err := s.VM.VsockConnect(s.VsockPort)
	if err != nil {
		return
	}
	defer vsockConn.Close()

	req := vsockProto.ConnectRequest{
		Type:  vsockProto.TypeConnect,
		Token: s.Token,
		Addr:  fmt.Sprintf("127.0.0.1:%d", guestPort),
	}
	if err := vsockProto.WriteJSON(vsockConn, &req); err != nil {
		return
	}
	var resp vsockProto.ConnectResponse
	if err := vsockProto.ReadJSON(vsockConn, &resp); err != nil {
		return
	}
	if !resp.OK {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(vsockConn, tcpConn); close(done) }()
	io.Copy(tcpConn, vsockConn)
	<-done
}

func (s *State) handleForwardAdd(conn net.Conn, req ExecRequest) {
	addr, err := s.AddForward(req.HostPort, req.GuestPort)
	if err != nil {
		json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(conn).Encode(map[string]any{
		"ok":         true,
		"host_port":  req.HostPort,
		"guest_port": req.GuestPort,
		"addr":       addr,
	})
}

func (s *State) handleForwardRemove(conn net.Conn, req ExecRequest) {
	if err := s.RemoveForward(req.HostPort, req.GuestPort); err != nil {
		json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(conn).Encode(map[string]any{"ok": true})
}

func (s *State) handleForwardList(conn net.Conn) {
	json.NewEncoder(conn).Encode(map[string]any{
		"ok":       true,
		"forwards": s.ListForwards(),
	})
}

func (s *State) handleExec(conn net.Conn, req ExecRequest) {
	// Connect to guest agent via vsock
	vsockConn, err := s.VM.VsockConnect(s.VsockPort)
	if err != nil {
		json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer vsockConn.Close()

	// Forward exec request to guest. Argv mode skips the shell wrap;
	// shell mode wraps Command in /bin/sh -c. See ExecRequest doc.
	execReq := buildVsockExec(req, s.Token)
	if err := vsockProto.WriteJSON(vsockConn, &execReq); err != nil {
		json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.Stream {
		// Relay streaming chunks to client
		for {
			header := make([]byte, 4)
			if _, err := vsockConn.Read(header); err != nil {
				return
			}
			length := uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
			data := make([]byte, length)
			if _, err := vsockConn.Read(data); err != nil {
				return
			}
			conn.Write(data)
			conn.Write([]byte("\n"))

			var check struct{ Stream string `json:"stream"` }
			json.Unmarshal(data, &check)
			if check.Stream == "" {
				return
			}
		}
	} else {
		// The agent enforces the exec timeout guest-side and always
		// replies; the host bound (guest budget + grace) only fires
		// when the agent or transport died mid-exec, so `dew exec`
		// reports an error instead of hanging its caller.
		guestBudget := 30 * time.Second
		if req.TimeoutMs > 0 {
			guestBudget = time.Duration(req.TimeoutMs) * time.Millisecond
		}
		var resp vsockProto.ExecResponse
		if err := vsockProto.ReadJSONTimeout(vsockConn, &resp, guestBudget+15*time.Second); err != nil {
			json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(resp)
	}
}
