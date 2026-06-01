//go:build darwin

// Package daemon provides a Unix socket API for cross-process
// access to a running Dew VM. Enables `dew exec` from any terminal
// to run commands in a VM started by `dew start`.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/solcreek/dew/internal/vm"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

// State holds the daemon's runtime state.
type State struct {
	VM        vm.VM
	Token     string
	VsockPort uint32
	SocketPath string

	listener net.Listener
	mu       sync.Mutex
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
	Command   string   `json:"command,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Stream    bool     `json:"stream,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`
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
		var resp vsockProto.ExecResponse
		if err := vsockProto.ReadJSON(vsockConn, &resp); err != nil {
			json.NewEncoder(conn).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(resp)
	}
}
