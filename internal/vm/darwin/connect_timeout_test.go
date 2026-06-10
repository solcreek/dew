//go:build darwin

package darwin

import (
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A dial that never completes — the vz behavior when the guest has no
// vsock transport — must surface as an error, not a hang.
func TestConnectWithTimeout_BlockingDial(t *testing.T) {
	start := time.Now()
	_, err := connectWithTimeout(func() (net.Conn, error) {
		select {} // block forever, like vz Connect against a mute guest
	}, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not respond") {
		t.Errorf("error = %q, want timeout message", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("returned after %s, want ~100ms", elapsed)
	}
}

func TestConnectWithTimeout_FastSuccess(t *testing.T) {
	want, peer := net.Pipe()
	defer peer.Close()
	got, err := connectWithTimeout(func() (net.Conn, error) {
		return want, nil
	}, time.Second)
	if err != nil {
		t.Fatalf("connectWithTimeout: %v", err)
	}
	if got != want {
		t.Error("returned conn is not the dialed conn")
	}
	got.Close()
}

func TestConnectWithTimeout_FastError(t *testing.T) {
	dialErr := errors.New("connection refused")
	_, err := connectWithTimeout(func() (net.Conn, error) {
		return nil, dialErr
	}, time.Second)
	if !errors.Is(err, dialErr) {
		t.Errorf("err = %v, want %v", err, dialErr)
	}
}

// A dial that fails late with a typed-nil conn (vz returns the nil
// *VirtioSocketConnection alongside its error) must not panic the
// reaper — Close on a typed-nil net.Conn dereferences nil.
func TestConnectWithTimeout_LateTypedNilNoPanic(t *testing.T) {
	_, err := connectWithTimeout(func() (net.Conn, error) {
		time.Sleep(100 * time.Millisecond)
		var c *net.TCPConn // typed nil, like vz on error
		return c, errors.New("connection reset")
	}, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	time.Sleep(300 * time.Millisecond) // reaper panic would kill the test binary
}

// A dial that completes after the timeout fired must have its conn
// closed (fd reaping), not leaked.
func TestConnectWithTimeout_LateSuccessReaped(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	release := make(chan struct{})
	var closed atomic.Bool
	cc := &closeRecorder{Conn: conn, closed: &closed}

	_, err := connectWithTimeout(func() (net.Conn, error) {
		<-release
		return cc, nil
	}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for !closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("late conn was never closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type closeRecorder struct {
	net.Conn
	closed *atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}
