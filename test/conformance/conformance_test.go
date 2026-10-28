//go:build darwin

// Package conformance boots a guest image and checks its agent over real
// vsock, through the host's own VM and protocol code (internal/vm/darwin,
// internal/vsock). The same tests run against the Go and the Rust agent, so
// they pin what the host can observe: frames, exit codes, error wording,
// timing contracts. Where the agents differ on purpose
// (docs/rust-agent-migration.md), a test says so and skips the Go run.
//
// Run it with scripts/conformance.sh, which signs the test binary (Apple's
// Virtualization framework needs the entitlement) and picks the image.
// Without DEW_CONFORMANCE_KERNEL and DEW_CONFORMANCE_INITRD every test skips.
package conformance

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/vm"
	"github.com/solcreek/dew/internal/vm/darwin"
	proto "github.com/solcreek/dew/internal/vsock"
)

const token = "conformance-token-0123456789abcdef"

// guest is the one VM every test shares; nil when no image is configured.
var guest *darwin.DarwinVM

// agent is "go" or "rust": which agent the booted image runs.
var agent string

func TestMain(m *testing.M) {
	kernel, initrd := os.Getenv("DEW_CONFORMANCE_KERNEL"), os.Getenv("DEW_CONFORMANCE_INITRD")
	if kernel == "" || initrd == "" {
		os.Exit(m.Run())
	}
	code, err := boot(m, kernel, initrd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func boot(m *testing.M, kernel, initrd string) (int, error) {
	cfg := vm.Config{
		Name: "conformance", CPUs: envUint("DEW_CONFORMANCE_CPUS", 2), MemoryMB: uint64(envUint("DEW_CONFORMANCE_MEMORY_MB", 1024)),
		Kernel: kernel, Initrd: initrd, CmdLine: "console=hvc0", VsockPort: proto.DefaultPort,
	}
	// A disk-bearing profile (node, python, standard) switch_roots onto an
	// ext4 disk it formats on first boot: give it a fresh one.
	if gb := envUint("DEW_CONFORMANCE_DISK_GB", 0); gb > 0 {
		dir, err := os.MkdirTemp("", "dew-conformance-")
		if err != nil {
			return 0, err
		}
		defer os.RemoveAll(dir)
		cfg.DiskPath, cfg.DiskGB = filepath.Join(dir, "disk.img"), gb
	}
	console, guestOut, _, err := vm.NewConsolePipe()
	if err != nil {
		return 0, err
	}
	cfg.Console = console
	serial := &lockedBuffer{}
	go io.Copy(serial, guestOut)

	d, err := darwin.New(cfg)
	if err != nil {
		return 0, err
	}
	if err := d.Start(context.Background()); err != nil {
		return 0, err
	}
	defer d.Stop(context.Background())
	if err := setToken(d); err != nil {
		return 0, fmt.Errorf("%w\nserial console:\n%s", err, serial.String())
	}
	guest = d

	// Which agent init started: a Go binary keeps its runtime's function
	// names (pclntab) even when stripped, and the Rust agent has none.
	// (Not the "Go buildinf" header: the guest's BusyBox grep misses it.)
	r, err := execOnce(proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c",
		`exe=/proc/$(pidof dew-agent | cut -d' ' -f1)/exe; test -r "$exe" || exit 2; grep -q 'runtime\.goexit' "$exe" && echo go || echo rust`}})
	switch {
	case err != nil:
		return 0, err
	case r.ExitCode == 0 && (r.Stdout == "go\n" || r.Stdout == "rust\n"):
		agent = strings.TrimSpace(r.Stdout)
	default:
		return 0, fmt.Errorf("cannot tell the agent apart: %+v", r)
	}
	fmt.Printf("conformance: agent=%s kernel=%s initrd=%s disk_gb=%d\n", agent, kernel, initrd, cfg.DiskGB)
	return m.Run(), nil
}

// lockedBuffer collects the serial console while the guest keeps writing,
// so a boot failure can print it without racing the copy.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// setToken retries until the agent is listening, as the host does at boot.
func setToken(d *darwin.DarwinVM) error {
	deadline := time.Now().Add(time.Duration(envUint("DEW_CONFORMANCE_BOOT_S", 60)) * time.Second)
	for time.Now().Before(deadline) {
		c, err := d.VsockConnect(proto.DefaultPort)
		if err == nil {
			var ack proto.ConnectResponse
			werr := proto.WriteJSON(c, &proto.SetTokenRequest{Type: proto.TypeSetToken, Token: token})
			rerr := proto.ReadJSONTimeout(c, &ack, 5*time.Second)
			c.Close()
			if werr == nil && rerr == nil {
				if !ack.OK || !ack.Confine {
					return fmt.Errorf("set_token ack = %+v, want ok and confine", ack)
				}
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("agent not ready")
}

func envUint(name string, def uint) uint {
	if v, err := strconv.ParseUint(os.Getenv(name), 10, 32); err == nil {
		return uint(v)
	}
	return def
}

func needGuest(t *testing.T) {
	t.Helper()
	if guest == nil {
		t.Skip("no image: set DEW_CONFORMANCE_KERNEL and DEW_CONFORMANCE_INITRD (scripts/conformance.sh)")
	}
}

// goDiffers skips the Go run of a case where the agents differ on purpose.
func goDiffers(t *testing.T, why string) {
	t.Helper()
	if agent == "go" {
		t.Skip("deliberate difference (docs/rust-agent-migration.md): " + why)
	}
}

func dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := guest.VsockConnect(proto.DefaultPort)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func send(t *testing.T, c net.Conn, v any) {
	t.Helper()
	if err := proto.WriteJSON(c, v); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, c net.Conn, v any, timeout time.Duration) {
	t.Helper()
	if err := proto.ReadJSONTimeout(c, v, timeout); err != nil {
		t.Fatal(err)
	}
}

func execOnce(req proto.ExecRequest) (proto.ExecResponse, error) {
	var r proto.ExecResponse
	c, err := guest.VsockConnect(proto.DefaultPort)
	if err != nil {
		return r, err
	}
	defer c.Close()
	if req.Token == "" {
		req.Token = token
	}
	if err := proto.WriteJSON(c, &req); err != nil {
		return r, err
	}
	return r, proto.ReadJSONTimeout(c, &r, 60*time.Second)
}

func run(t *testing.T, req proto.ExecRequest) proto.ExecResponse {
	t.Helper()
	r, err := execOnce(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sh(t *testing.T, script string) proto.ExecResponse {
	t.Helper()
	return run(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", script}})
}

// frame is an OutputChunk or, when ExitCode is set, the closing ExecDone.
type frame struct {
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	ExitCode *int   `json:"exit_code"`
	Error    string `json:"error"`
}

type streamed struct {
	stdout, stderr string
	exit           int
	err            string
	frames         int
}

// collect reads frames until ExecDone. TTY output is base64.
func collect(t *testing.T, c net.Conn, tty bool, timeout time.Duration) streamed {
	t.Helper()
	var s streamed
	var out, errb strings.Builder
	for {
		var f frame
		recv(t, c, &f, timeout)
		if f.ExitCode != nil {
			s.stdout, s.stderr, s.exit, s.err = out.String(), errb.String(), *f.ExitCode, f.Error
			return s
		}
		s.frames++
		data := f.Data
		if tty {
			b, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				t.Fatalf("tty frame is not base64: %q", f.Data)
			}
			data = string(b)
		}
		switch f.Stream {
		case "stdout":
			out.WriteString(data)
		case "stderr":
			errb.WriteString(data)
		default:
			t.Fatalf("frame on unknown stream %q", f.Stream)
		}
	}
}

func stream(t *testing.T, req proto.ExecRequest) (net.Conn, streamed) {
	t.Helper()
	c := dial(t)
	req.Token, req.Stream = token, true
	send(t, c, &req)
	return c, collect(t, c, req.TTY, 30*time.Second)
}

// ------------------------------------------------------------ handshake/auth

func TestHandshake_SecondSetTokenIsRefused(t *testing.T) {
	needGuest(t)
	c := dial(t)
	send(t, c, &proto.SetTokenRequest{Type: proto.TypeSetToken, Token: "attacker"})
	var r proto.ConnectResponse
	recv(t, c, &r, 5*time.Second)
	if r.OK || r.Error != "token already set" {
		t.Fatalf("second set_token = %+v", r)
	}
	// The first token still works.
	if r := sh(t, "echo ok"); r.Stdout != "ok\n" {
		t.Fatalf("exec after the refused set_token: %+v", r)
	}
}

func TestAuth_WrongTokenRunsNothing(t *testing.T) {
	needGuest(t)
	r := run(t, proto.ExecRequest{Token: "wrong", Command: "/bin/sh", Args: []string{"-c", "touch /tmp/conformance-unauth"}})
	if r.ExitCode != -1 || r.Error != "unauthorized" {
		t.Errorf("exec with a wrong token = %+v", r)
	}
	c := dial(t)
	send(t, c, &proto.ExecRequest{Token: "wrong", Command: "/bin/sh", Args: []string{"-c", "touch /tmp/conformance-unauth"}, Stream: true})
	var sr proto.ExecResponse
	recv(t, c, &sr, 5*time.Second)
	if sr.ExitCode != -1 || sr.Error != "unauthorized" {
		t.Errorf("streaming exec with a wrong token = %+v", sr)
	}
	for _, typ := range []string{proto.TypeConnect, proto.TypeSetExposes} {
		c := dial(t)
		send(t, c, map[string]any{"type": typ, "token": "wrong", "addr": "127.0.0.1:1", "ports": []int{1}})
		var cr proto.ConnectResponse
		recv(t, c, &cr, 5*time.Second)
		if cr.OK || cr.Error != "unauthorized" {
			t.Errorf("%s with a wrong token = %+v", typ, cr)
		}
	}
	if r := sh(t, "test -e /tmp/conformance-unauth && echo RAN || echo none"); r.Stdout != "none\n" {
		t.Errorf("an unauthorized exec ran: %q", r.Stdout)
	}
}

func TestPing(t *testing.T) {
	needGuest(t)
	if r := run(t, proto.ExecRequest{Command: "ping"}); r.Stdout != "pong" || r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("ping = %+v", r)
	}
}

// ------------------------------------------------------------ buffered exec

func TestExec_OutputAndExitCode(t *testing.T) {
	needGuest(t)
	r := sh(t, "echo out; echo err >&2; exit 7")
	if r.Stdout != "out\n" || r.Stderr != "err\n" || r.ExitCode != 7 || r.Error != "" {
		t.Fatalf("got %+v", r)
	}
}

// The buffered path reports stderr only for a failing command (os/exec's
// Output keeps it on the exit error). The host relies on that shape.
func TestExec_StderrOnlyOnFailure(t *testing.T) {
	needGuest(t)
	if r := sh(t, "echo out; echo err >&2"); r.Stdout != "out\n" || r.Stderr != "" || r.ExitCode != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestExec_EnvAndDir(t *testing.T) {
	needGuest(t)
	r := run(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", `echo "$DEW_CONF_X"; pwd; echo "$PATH"`},
		Env: []string{"DEW_CONF_X=hello world"}, Dir: "/tmp"})
	lines := strings.Split(r.Stdout, "\n")
	if r.ExitCode != 0 || len(lines) < 3 || lines[0] != "hello world" || lines[1] != "/tmp" || !strings.Contains(lines[2], "/usr/bin") {
		t.Fatalf("got %+v", r)
	}
}

func TestExec_SeveralRequestsOnOneConn(t *testing.T) {
	needGuest(t)
	c := dial(t)
	for i := range 3 {
		send(t, c, &proto.ExecRequest{Token: token, Command: "/bin/echo", Args: []string{strconv.Itoa(i)}})
		var r proto.ExecResponse
		recv(t, c, &r, 10*time.Second)
		if r.Stdout != fmt.Sprintf("%d\n", i) {
			t.Fatalf("request %d on a shared conn = %+v", i, r)
		}
	}
}

func TestExec_LargeOutputIsIntact(t *testing.T) {
	needGuest(t)
	const n = 4 << 20
	r := sh(t, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' a", n))
	if r.ExitCode != 0 || len(r.Stdout) != n || strings.Trim(r.Stdout, "a") != "" {
		t.Fatalf("exit=%d len=%d err=%q", r.ExitCode, len(r.Stdout), r.Error)
	}
}

func TestExec_MissingCommand(t *testing.T) {
	needGuest(t)
	r := run(t, proto.ExecRequest{Command: "dew-no-such-cmd"})
	if r.ExitCode != -1 || r.Error != `exec: "dew-no-such-cmd": executable file not found in $PATH` {
		t.Fatalf("got %+v", r)
	}
}

func TestExec_Timeout(t *testing.T) {
	needGuest(t)
	start := time.Now()
	r := run(t, proto.ExecRequest{Command: "/bin/sleep", Args: []string{"30"}, TimeoutMs: 1500})
	if r.ExitCode != -1 || r.Error != "timeout after 1.5s" {
		t.Fatalf("got %+v", r)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a 1.5s timeout took %s", took)
	}
}

// A backgrounded grandchild that keeps stdout open must not hold the exec
// past its deadline.
func TestExec_TimeoutWithGrandchildHoldingStdout(t *testing.T) {
	needGuest(t)
	goDiffers(t, "the Go agent kills only the direct child and waits for the pipe")
	start := time.Now()
	r := run(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "sleep 30 & echo started; wait"}, TimeoutMs: 1500})
	if r.ExitCode != -1 || r.Error != "timeout after 1.5s" {
		t.Fatalf("got %+v", r)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a 1.5s timeout took %s", took)
	}
}

// ------------------------------------------------------------ streaming exec

func TestStream_OutputAndExitCode(t *testing.T) {
	needGuest(t)
	_, s := stream(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 5"}})
	if s.stdout != "out\n" || s.stderr != "err\n" || s.exit != 5 || s.err != "" {
		t.Fatalf("got %+v", s)
	}
}

// Both streams written concurrently arrive whole and in order, each in its
// own frames. This is the frame interleave bug fixed in the Go agent (PR
// #44), which corrupted about one stream in six at this size; it is a race,
// so the reps make a miss unlikely, not impossible.
func TestStream_InterleaveStress(t *testing.T) {
	needGuest(t)
	const lines = 20000
	script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo out$i; echo err$i >&2; i=$((i+1)); done`, lines)
	var wantOut, wantErr strings.Builder
	for i := range lines {
		fmt.Fprintf(&wantOut, "out%d\n", i)
		fmt.Fprintf(&wantErr, "err%d\n", i)
	}
	reps := int(envUint("DEW_CONFORMANCE_INTERLEAVE_REPS", 25))
	for rep := range reps {
		_, s := stream(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", script}})
		if s.exit != 0 || s.stdout != wantOut.String() || s.stderr != wantErr.String() {
			t.Fatalf("rep %d: exit=%d stdout ok=%v stderr ok=%v (%d frames)", rep, s.exit,
				s.stdout == wantOut.String(), s.stderr == wantErr.String(), s.frames)
		}
	}
}

func TestStream_Stdin(t *testing.T) {
	needGuest(t)
	c := dial(t)
	send(t, c, &proto.ExecRequest{Token: token, Command: "/bin/cat", Stream: true, Stdin: true})
	send(t, c, &proto.InputChunk{Data: "hello\n"})
	send(t, c, &proto.InputChunk{Data: "world\n"})
	send(t, c, &proto.InputChunk{EOF: true})
	s := collect(t, c, false, 10*time.Second)
	if s.stdout != "hello\nworld\n" || s.exit != 0 {
		t.Fatalf("got %+v", s)
	}
}

func TestStream_Timeout(t *testing.T) {
	needGuest(t)
	start := time.Now()
	_, s := stream(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", "echo before; exec sleep 30"}, TimeoutMs: 1500})
	if s.stdout != "before\n" || s.exit != -1 || s.err != "timeout after 1.5s" {
		t.Fatalf("got %+v", s)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a 1.5s timeout took %s", took)
	}
}

// ------------------------------------------------------------------------ TTY

func TestTTY_TerminalAndResize(t *testing.T) {
	needGuest(t)
	c := dial(t)
	send(t, c, &proto.ExecRequest{Token: token, Command: "/bin/sh",
		Args:   []string{"-c", "test -t 0 && test -t 1 && echo IS_TTY; stty size; read line; stty size; echo got:$line"},
		Stream: true, Stdin: true, TTY: true, Rows: 24, Cols: 80})
	// Wait for the first size before resizing, so the resize can't race it.
	r := bufio.NewReader(&ttyReader{t: t, c: c})
	before := readUntil(t, r, "24 80")
	send(t, c, &proto.InputChunk{Winch: true, Rows: 40, Cols: 100})
	send(t, c, &proto.InputChunk{Data: base64.StdEncoding.EncodeToString([]byte("abc\n"))})
	after := readUntil(t, r, "got:abc")
	if !strings.Contains(before, "IS_TTY") || !strings.Contains(after, "40 100") {
		t.Fatalf("before=%q after=%q", before, after)
	}
}

// ttyReader decodes TTY frames into a byte stream (EOF at ExecDone).
type ttyReader struct {
	t   *testing.T
	c   net.Conn
	buf []byte
}

func (r *ttyReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		var f frame
		if err := proto.ReadJSONTimeout(r.c, &f, 10*time.Second); err != nil {
			return 0, err
		}
		if f.ExitCode != nil {
			return 0, io.EOF
		}
		b, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return 0, err
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func readUntil(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("waiting for %q: %v (so far %q)", want, err, got.String())
		}
		got.WriteByte(b)
	}
	return got.String()
}

// Dropping the host side of a TTY session hangs the terminal up.
func TestTTY_HostDisconnectEndsTheSession(t *testing.T) {
	needGuest(t)
	goDiffers(t, "the Go agent leaves the shell running when the host disconnects")
	c, err := guest.VsockConnect(proto.DefaultPort)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, &proto.ExecRequest{Token: token, Command: "/bin/sh",
		Args: []string{"-c", "echo $$ > /tmp/conformance-tty.pid; echo started; exec sleep 600"}, Stream: true, Stdin: true, TTY: true, Rows: 24, Cols: 80})
	readUntil(t, bufio.NewReader(&ttyReader{t: t, c: c}), "started")
	pid := strings.TrimSpace(sh(t, "cat /tmp/conformance-tty.pid").Stdout)
	if r := sh(t, "kill -0 "+pid+" && echo alive"); r.Stdout != "alive\n" {
		t.Fatalf("session %s not running before the disconnect: %+v", pid, r)
	}
	c.Close()
	for range 50 {
		if sh(t, "kill -0 "+pid+" 2>/dev/null && echo alive || echo gone").Stdout == "gone\n" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("session %s still running 5s after the host disconnected", pid)
}

// ------------------------------------------------------------ connect/expose

func TestConnect_ProxiesBothWays(t *testing.T) {
	needGuest(t)
	const port = 7781
	sh(t, fmt.Sprintf("nc -lk -p %d -e /bin/cat >/dev/null 2>&1 &", port))
	t.Cleanup(func() { sh(t, fmt.Sprintf("pkill -f 'nc -lk -p %d' || true", port)) })
	var c net.Conn
	var r proto.ConnectResponse
	for range 50 { // until the listener is up
		c = dial(t)
		send(t, c, &proto.ConnectRequest{Type: proto.TypeConnect, Token: token, Addr: fmt.Sprintf("127.0.0.1:%d", port)})
		recv(t, c, &r, 5*time.Second)
		if r.OK {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !r.OK {
		t.Fatalf("connect = %+v", r)
	}
	msg := strings.Repeat("conformance-", 10000) + "\n" // > one read's worth
	go fmt.Fprint(c, msg)
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || got != msg {
		t.Fatalf("echo through connect: %d of %d bytes, %v", len(got), len(msg), err)
	}
}

func TestConnect_RefusedPort(t *testing.T) {
	needGuest(t)
	c := dial(t)
	send(t, c, &proto.ConnectRequest{Type: proto.TypeConnect, Token: token, Addr: "127.0.0.1:7799"})
	var r proto.ConnectResponse
	recv(t, c, &r, 5*time.Second)
	if r.OK || r.Error != "dial tcp 127.0.0.1:7799: connect: connection refused" {
		t.Fatalf("got %+v", r)
	}
}

// guest → 127.0.0.2:P → the agent's reverse dial → host vsock 1025 → a
// listener on the Mac.
func TestExpose_ReverseForward(t *testing.T) {
	needGuest(t)
	mac, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mac.Close()
	port := mac.Addr().(*net.TCPAddr).Port
	fromGuest := make(chan string, 1)
	go func() {
		c, err := mac.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		fromGuest <- line
		fmt.Fprint(c, "hello-from-mac\n")
	}()

	ln, err := guest.VsockListen(proto.ReverseForwardPort)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialed := make(chan proto.ReverseDialRequest, 1)
	go func() {
		vc, err := ln.Accept()
		if err != nil {
			return
		}
		defer vc.Close()
		var req proto.ReverseDialRequest
		if proto.ReadJSON(vc, &req) != nil {
			return
		}
		dialed <- req
		tc, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port))
		if err != nil {
			proto.WriteJSON(vc, &proto.ReverseDialResponse{Error: err.Error()})
			return
		}
		defer tc.Close()
		proto.WriteJSON(vc, &proto.ReverseDialResponse{OK: true})
		go io.Copy(tc, vc)
		io.Copy(vc, tc)
	}()

	c := dial(t)
	send(t, c, &proto.SetExposesRequest{Type: proto.TypeSetExposes, Token: token, Ports: []int{port}})
	var ack proto.ConnectResponse
	recv(t, c, &ack, 5*time.Second)
	if !ack.OK {
		t.Fatalf("set_exposes = %+v", ack)
	}
	r := sh(t, fmt.Sprintf("echo hi-from-guest | nc -w 3 127.0.0.2 %d", port))
	select {
	case req := <-dialed:
		if req.Type != proto.TypeReverseDial || req.Token != token || req.Port != port {
			t.Errorf("reverse dial = %+v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reverse dial")
	}
	select {
	case line := <-fromGuest:
		if line != "hi-from-guest\n" {
			t.Errorf("mac got %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the mac")
	}
	if !strings.Contains(r.Stdout, "hello-from-mac") {
		t.Errorf("guest got %+v", r)
	}
	// Reconcile to none: the guest listener goes away.
	send(t, dial(t), &proto.SetExposesRequest{Type: proto.TypeSetExposes, Token: token, Ports: []int{}})
}

// ------------------------------------------------------------------- confine

func confined(t *testing.T, script string, c proto.Confinement) proto.ExecResponse {
	t.Helper()
	return run(t, proto.ExecRequest{Command: "/bin/sh", Args: []string{"-c", script}, Confine: &c})
}

func TestConfine_UserAndNoNewPrivs(t *testing.T) {
	needGuest(t)
	r := confined(t, "id -u; id -G; grep -E '^(NoNewPrivs|CapEff):' /proc/self/status",
		proto.Confinement{User: "65534", NoNewPrivs: true})
	if r.ExitCode != 0 || !strings.HasPrefix(r.Stdout, "65534\n65534\n") ||
		!strings.Contains(r.Stdout, "NoNewPrivs:\t1") || !strings.Contains(r.Stdout, "CapEff:\t0000000000000000") {
		t.Fatalf("got %+v", r)
	}
}

func TestConfine_ReadOnlyRootWithWritablePath(t *testing.T) {
	needGuest(t)
	r := confined(t, "touch /conformance-ro 2>/dev/null && echo root-rw || echo root-ro; touch /var/lib/conformance-rw/x && echo rw-ok",
		proto.Confinement{ReadOnlyRoot: true, ReadWritePaths: []string{"/var/lib/conformance-rw"}})
	if !strings.Contains(r.Stdout, "root-ro") || !strings.Contains(r.Stdout, "rw-ok") {
		t.Fatalf("got %+v", r)
	}
	// The remount stayed in the shim's mount namespace.
	if after := sh(t, "touch /conformance-after && rm /conformance-after && echo rw"); after.Stdout != "rw\n" {
		t.Fatalf("the agent's root is no longer writable: %+v", after)
	}
}

func TestConfine_AddressFamilyDenylist(t *testing.T) {
	needGuest(t)
	r := confined(t, "nc -z -w 1 127.0.0.1 9 2>&1 | grep -c 'not permitted'",
		proto.Confinement{AddressFamilies: []string{"AF_INET"}, AddressFamiliesDeny: true})
	if strings.TrimSpace(r.Stdout) != "1" {
		t.Fatalf("got %+v", r)
	}
}

func TestConfine_FailsClosed(t *testing.T) {
	needGuest(t)
	for _, tc := range []struct {
		c      proto.Confinement
		stderr string
	}{
		{proto.Confinement{User: "ghost-conformance"}, `dew-confine: user "ghost-conformance": user: unknown user ghost-conformance`},
		{proto.Confinement{User: "4294967295"}, "dew-confine: uid 4294967295 out of range"},
		{proto.Confinement{DropAllCaps: true, KeepCaps: []string{"cap_bogus"}}, ""},
	} {
		r := confined(t, "echo RAN", tc.c)
		spec, _ := json.Marshal(tc.c)
		if r.ExitCode != 126 || strings.Contains(r.Stdout, "RAN") || !strings.HasPrefix(r.Stderr, "dew-confine: ") ||
			(tc.stderr != "" && strings.TrimSpace(r.Stderr) != tc.stderr) {
			t.Errorf("%s: %+v", spec, r)
		}
	}
}

// A request asking for --confine over the streaming path must not run the
// command unconfined.
func TestConfine_StreamingIsRefused(t *testing.T) {
	needGuest(t)
	goDiffers(t, "the Go agent's streaming path ignores the spec")
	c := dial(t)
	send(t, c, &proto.ExecRequest{Token: token, Command: "/bin/sh", Args: []string{"-c", "touch /tmp/conformance-stream-confine"},
		Stream: true, Confine: &proto.Confinement{NoNewPrivs: true}})
	var f frame
	recv(t, c, &f, 10*time.Second)
	if f.ExitCode == nil || !strings.HasPrefix(f.Error, "confine:") {
		t.Errorf("first frame = %+v, want an ExecDone with a confine: error", f)
	}
	if r := sh(t, "test -e /tmp/conformance-stream-confine && echo RAN || echo none"); r.Stdout != "none\n" {
		t.Errorf("the command ran: %q", r.Stdout)
	}
}
