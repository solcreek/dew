//go:build darwin

package vsock

import (
	"fmt"
	"net"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// Client connects to the dew-agent inside a running VM via vsock.
type Client struct {
	conn net.Conn
}

// Connect establishes a vsock connection to the guest agent.
func Connect(machine *vz.VirtualMachine, port uint32) (*Client, error) {
	devices := machine.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("dew: VM has no vsock devices")
	}

	conn, err := devices[0].Connect(port)
	if err != nil {
		return nil, fmt.Errorf("dew: vsock connect port %d: %w", port, err)
	}

	return &Client{conn: conn}, nil
}

// Exec sends a command to the guest agent and returns the response.
func (c *Client) Exec(req ExecRequest) (*ExecResponse, error) {
	c.conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := WriteJSON(c.conn, &req); err != nil {
		return nil, fmt.Errorf("dew: exec write: %w", err)
	}

	var resp ExecResponse
	if err := ReadJSON(c.conn, &resp); err != nil {
		return nil, fmt.Errorf("dew: exec read: %w", err)
	}

	return &resp, nil
}

// Ping checks if the guest agent is responsive.
func (c *Client) Ping() error {
	resp, err := c.Exec(ExecRequest{Command: "ping"})
	if err != nil {
		return err
	}
	if resp.Stdout != "pong" {
		return fmt.Errorf("dew: unexpected ping response: %q", resp.Stdout)
	}
	return nil
}

// Close releases the vsock connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
