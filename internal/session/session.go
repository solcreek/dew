//go:build darwin

// Package session manages long-lived VM sessions for repeated exec.
// A session boots once, keeps the VM alive, and serves multiple exec
// requests through vsock. Wall time per exec: ~50ms (vs ~831ms cold).
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
	vsockProto "github.com/solcreek/dew/internal/vsock"
)

type Session struct {
	ID    string
	VM    *darwin.DarwinVM
	Token string

	mu     sync.Mutex
	closed bool

	hostReader *os.File
	hostWriter *os.File
	consoleR   *os.File
	consoleW   *os.File
}

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Create boots a new VM session and waits until the guest agent is ready.
func Create(cfg vm.Config) (*Session, error) {
	s := &Session{
		ID:    generateID(),
		Token: generateToken(),
	}

	if cfg.VsockPort == 0 {
		cfg.VsockPort = vsockProto.DefaultPort
	}
	cfg.CmdLine += " dew.token=" + s.Token

	console, hostReader, hostWriter, err := vm.NewConsolePipe()
	if err != nil {
		return nil, fmt.Errorf("session: console pipe: %w", err)
	}
	cfg.Console = console
	s.hostReader = hostReader.(*os.File)
	s.hostWriter = hostWriter.(*os.File)
	s.consoleR = console.In
	s.consoleW = console.Out

	d, err := darwin.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	s.VM = d

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return nil, fmt.Errorf("session: start: %w", err)
	}

	// Wait for vsock to become available
	if err := s.waitReady(ctx); err != nil {
		d.Stop(context.Background())
		return nil, fmt.Errorf("session: agent not ready: %w", err)
	}

	return s, nil
}

func (s *Session) waitReady(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := s.VM.VsockConnect(vsockProto.DefaultPort)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		// Ping to confirm agent is functional
		req := vsockProto.ExecRequest{Token: s.Token, Command: "ping"}
		if err := vsockProto.WriteJSON(conn, &req); err != nil {
			conn.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var resp vsockProto.ExecResponse
		if err := vsockProto.ReadJSON(conn, &resp); err != nil {
			conn.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		conn.Close()
		if resp.Stdout == "pong" {
			return nil
		}
	}
}

// Exec runs a command in the session's VM.
func (s *Session) Exec(cmd string, timeoutMs int) (*ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session: closed")
	}

	conn, err := s.VM.VsockConnect(vsockProto.DefaultPort)
	if err != nil {
		return nil, fmt.Errorf("session: vsock connect: %w", err)
	}
	defer conn.Close()

	req := vsockProto.ExecRequest{
		Token:     s.Token,
		Command:   "/bin/sh",
		Args:      []string{"-c", cmd},
		TimeoutMs: timeoutMs,
	}
	if err := vsockProto.WriteJSON(conn, &req); err != nil {
		return nil, fmt.Errorf("session: write: %w", err)
	}

	var resp vsockProto.ExecResponse
	if err := vsockProto.ReadJSON(conn, &resp); err != nil {
		return nil, fmt.Errorf("session: read: %w", err)
	}

	return &ExecResult{
		ExitCode: resp.ExitCode,
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		Error:    resp.Error,
	}, nil
}

// Destroy stops the VM and cleans up.
func (s *Session) Destroy() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	err := s.VM.Stop(context.Background())
	s.hostReader.Close()
	s.hostWriter.Close()
	s.consoleR.Close()
	s.consoleW.Close()
	return err
}

// StateDir returns the directory for storing session state files.
func StateDir() string {
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".local", "state", "dew", "sessions")
}

// Save writes the session ID and metadata to disk for reconnection.
func (s *Session) Save() error {
	dir := StateDir()
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, s.ID)
	return os.WriteFile(path, []byte(s.ID+"\n"), 0600)
}

// Remove deletes the session state file.
func (s *Session) Remove() error {
	return os.Remove(filepath.Join(StateDir(), s.ID))
}

// net.Conn wrapper for io.ReadCloser/io.WriteCloser
func connToFile(rc interface{}) *os.File {
	if f, ok := rc.(*os.File); ok {
		return f
	}
	return nil
}
