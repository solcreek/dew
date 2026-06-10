// Package vsock defines the exec protocol between the dew host and
// the guest agent over virtio-vsock.
package vsock

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	DefaultPort = 1024
)

// Message types for the vsock protocol.
const (
	TypeExec     = "exec"
	TypeConnect  = "connect"
	TypeSetToken = "set_token"
)

// SetTokenRequest is sent once after boot to inject the auth token
// via vsock instead of kernel cmdline (avoids /proc/cmdline leak).
type SetTokenRequest struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type ExecRequest struct {
	Type      string   `json:"type"`
	Token     string   `json:"token"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Env       []string `json:"env,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`
	Stream    bool     `json:"stream,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// OutputChunk is sent during streaming exec. Stream is "stdout" or "stderr".
type OutputChunk struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// ExecDone signals the end of a streaming exec.
type ExecDone struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// ConnectRequest asks the agent to dial a TCP address inside the guest
// and bidirectionally proxy data over the vsock connection.
type ConnectRequest struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	Addr  string `json:"addr"` // e.g. "127.0.0.1:3000"
}

// ConnectResponse is sent back before proxying starts.
type ConnectResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type PingResponse struct {
	Status string `json:"status"`
}

func WriteJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	// length-prefixed: 4-byte big-endian length + payload
	length := uint32(len(data))
	header := []byte{
		byte(length >> 24),
		byte(length >> 16),
		byte(length >> 8),
		byte(length),
	}
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

func ReadJSON(r io.Reader, v any) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	length := uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
	if length > 10*1024*1024 {
		return fmt.Errorf("payload too large: %d bytes", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	return json.Unmarshal(data, v)
}

// ReadJSONTimeout reads one length-prefixed JSON message, giving up
// after timeout. The vz-backed vsock conns can't be relied on to honor
// SetReadDeadline, so cancellation works by closing the conn — which
// unblocks the pending read and makes the conn unusable afterwards.
// Callers that hit the timeout must treat the connection as dead.
func ReadJSONTimeout(conn net.Conn, v any, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- ReadJSON(conn, v) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		conn.Close()
		return fmt.Errorf("vsock read: no response within %s", timeout)
	}
}
