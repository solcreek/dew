//go:build darwin

package daemon

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

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
