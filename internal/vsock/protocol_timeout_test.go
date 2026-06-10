package vsock

import (
	"net"
	"strings"
	"testing"
	"time"
)

func newPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// A peer that never replies must not hang the reader — this is the
// regression test for `dew run` blocking forever on a dead guest.
func TestReadJSONTimeout_SilentPeer(t *testing.T) {
	client, server := newPipe(t)
	defer server.Close()

	start := time.Now()
	var resp ExecResponse
	err := ReadJSONTimeout(client, &resp, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "no response within") {
		t.Errorf("error = %q, want timeout message", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~100ms", elapsed)
	}
}

func TestReadJSONTimeout_PeerReplies(t *testing.T) {
	client, server := newPipe(t)
	defer server.Close()

	go func() {
		WriteJSON(server, &ExecResponse{ExitCode: 7, Stdout: "hi"})
	}()

	var resp ExecResponse
	if err := ReadJSONTimeout(client, &resp, 2*time.Second); err != nil {
		t.Fatalf("ReadJSONTimeout: %v", err)
	}
	if resp.ExitCode != 7 || resp.Stdout != "hi" {
		t.Errorf("resp = %+v, want ExitCode=7 Stdout=hi", resp)
	}
}

// After a timeout the conn must be closed so the abandoned reader
// goroutine unblocks instead of leaking against a live conn.
func TestReadJSONTimeout_ClosesConnOnTimeout(t *testing.T) {
	client, server := newPipe(t)
	defer server.Close()

	var resp ExecResponse
	_ = ReadJSONTimeout(client, &resp, 50*time.Millisecond)

	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Error("conn still readable after timeout, want closed")
	}
}
