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
	// ReverseForwardPort is the host-side vsock listener port the guest
	// dials (CID=2) to reach a macOS host service exposed via `dew up
	// --expose-host`. It is the mirror of DefaultPort: DefaultPort is the
	// guest listening for host-initiated exec/connect; ReverseForwardPort is
	// the host listening for guest-initiated reverse dials. See
	// ReverseDialRequest.
	ReverseForwardPort = 1025
)

// Message types for the vsock protocol.
const (
	TypeExec        = "exec"
	TypeConnect     = "connect"
	TypeSetToken    = "set_token"
	TypeSetExposes  = "set_exposes"
	TypeReverseDial = "reverse_dial"
)

// SetExposesRequest is sent once after boot (host→guest, on DefaultPort, like
// SetTokenRequest) to tell the agent which host ports the user exposed. The
// agent then listens on 127.0.0.2:<port> inside the guest for each and proxies
// accepted connections back to the host over ReverseForwardPort.
type SetExposesRequest struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	Ports []int  `json:"ports"`
}

// ReverseDialRequest is sent guest→host on ReverseForwardPort, once per
// forwarded connection, asking the host to dial 127.0.0.1:Port on macOS and
// bidirectionally proxy. The host validates Token and that Port is in the
// user-declared expose set before dialing — the guest can never make the host
// dial an arbitrary address. This is how a VM service reaches a host process
// bound to 127.0.0.1 (the dev default) without the NAT path or a 0.0.0.0 bind.
type ReverseDialRequest struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	Port  int    `json:"port"`
}

// ReverseDialResponse is sent host→guest before proxying starts.
type ReverseDialResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

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
	// Stdin opts the streaming exec into receiving stdin: after the
	// request, the host sends InputChunk frames the guest feeds to the
	// process's stdin. Interactive sessions (a shell) also disable the
	// exec timeout, since they run until stdin closes or the process
	// exits. Only meaningful together with Stream.
	Stdin bool `json:"stdin,omitempty"`
	// TTY allocates a pseudo-terminal in the guest and runs the command
	// attached to it (a single merged output stream, isatty true, job
	// control, line editing). Implies Stdin. Rows/Cols are the initial
	// window size; resizes arrive as InputChunk{Winch:true}. In TTY mode
	// InputChunk.Data and OutputChunk.Data are base64 (terminal bytes are
	// binary and not safe to carry raw in a JSON string).
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// Confine, when set, is a privilege/isolation spec the host derives from a
	// systemd unit (--confine). The agent's re-exec shim enforces all of it
	// natively before exec: the read-only filesystem (mount namespace) and the
	// uid/caps/no_new_privs drop (prctl/capset/setresuid). omitempty so the
	// common unconfined path is unchanged and an older agent simply ignores it.
	Confine *Confinement `json:"confine,omitempty"`
}

// Confinement is the privilege/isolation spec the host derives from a systemd
// unit for `dew run --confine`.
//
// Enforcement status (this is a phased feature):
//   - ReadOnlyRoot/ReadWritePaths — APPLIED by the agent's re-exec shim, in a
//     mount namespace before exec.
//   - User/Group/DynamicUser/NoNewPrivs/DropAllCaps/KeepCaps/DropCaps — APPLIED
//     natively by the shim (prctl/capset/setresuid), replacing the former
//     host-built setpriv prefix.
//   - RestrictAddressFamilies= — APPLIED by the shim as a socket(2)/socketpair(2)
//     BPF seccomp filter. SystemCallFilter= is still design-only (see
//     docs/confine-enforcement.md §5).
type Confinement struct {
	// Privilege drop — applied natively by the agent shim.
	User        string   `json:"user,omitempty"`         // uid or username; "" = unchanged
	Group       string   `json:"group,omitempty"`        // gid or group name
	DynamicUser bool     `json:"dynamic_user,omitempty"` // no User= but DynamicUser=yes → fixed unprivileged uid
	NoNewPrivs  bool     `json:"no_new_privs,omitempty"`
	DropAllCaps bool     `json:"drop_all_caps,omitempty"` // empty/positive bounding set → drop all but KeepCaps
	KeepCaps    []string `json:"keep_caps,omitempty"`     // libcap names (lowercase) kept when DropAllCaps
	DropCaps    []string `json:"drop_caps,omitempty"`     // libcap names dropped from the inherited set otherwise
	// Filesystem (ProtectSystem=strict + ReadWritePaths=) — applied by the agent.
	// A missing ReadWritePaths entry is created as a directory; a writable file
	// exception must already exist on the rootfs to be bound as a file.
	ReadOnlyRoot   bool     `json:"read_only_root,omitempty"`
	ReadWritePaths []string `json:"read_write_paths,omitempty"`
	// Seccomp (RestrictAddressFamilies=) — applied by the agent shim as a BPF
	// filter on socket(2)/socketpair(2). AddressFamilies holds AF_* names;
	// AddressFamiliesDeny is the systemd `~` (denylist) form.
	AddressFamilies     []string `json:"address_families,omitempty"`
	AddressFamiliesDeny bool     `json:"address_families_deny,omitempty"`
}

// Set reports whether the spec constrains anything (so the agent can skip the
// re-exec shim entirely when it doesn't).
func (c *Confinement) Set() bool {
	if c == nil {
		return false
	}
	return c.User != "" || c.Group != "" || c.DynamicUser || c.NoNewPrivs ||
		c.DropAllCaps || len(c.KeepCaps) > 0 || len(c.DropCaps) > 0 ||
		c.ReadOnlyRoot || len(c.ReadWritePaths) > 0 || len(c.AddressFamilies) > 0
}

// InputChunk carries stdin from host to guest during a streaming exec.
// EOF (empty Data) signals the host closed stdin. Winop (Winch=true)
// carries a terminal resize (Rows/Cols) instead of data. In TTY mode
// Data is base64-encoded.
type InputChunk struct {
	Type  string `json:"type,omitempty"`
	Data  string `json:"data,omitempty"`
	EOF   bool   `json:"eof,omitempty"`
	Winch bool   `json:"winch,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
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
	// Confine is the SetToken handshake's positive acknowledgement that this
	// agent applies the ExecRequest.Confine spec natively (read-only fs +
	// uid/caps/no_new_privs drop). An older agent omits the field, so it
	// decodes to false and the host fails closed rather than silently running
	// a --confine command unconfined. omitempty: irrelevant on other replies.
	Confine bool `json:"confine,omitempty"`
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
