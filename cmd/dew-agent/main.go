//go:build linux

// dew-agent runs inside the VM guest, listens on vsock and executes
// commands sent by the host.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	"github.com/solcreek/dew/internal/agentauth"
	"github.com/solcreek/dew/internal/guestenv"
	protocol "github.com/solcreek/dew/internal/vsock"
)

var authToken string
var tokenSet bool

// syncInterval bounds how much unsynced guest data an abrupt VM stop can lose.
// 10s is a light touch given the Cached+Fsync disk attachment.
const syncInterval = 10 * time.Second

// isAuthorized wraps the package-level state for the dispatch loop.
// The pure logic lives in internal/agentauth so it can be tested on
// any host OS (this package is //go:build linux only).
func isAuthorized(givenToken string) bool {
	return agentauth.IsAuthorized(givenToken, authToken, tokenSet)
}

var execUser string

func main() {
	// Pin the agent's own PATH so exec.LookPath resolves guest binaries
	// deterministically. Go resolves a command's argv[0] against THIS
	// process's PATH when exec.Command is constructed — setting cmd.Env
	// afterward does NOT re-resolve it — so the agent's PATH order, not
	// ExecEnv's child PATH, decides which binary wins. `--confine` shells out
	// to a bare `setpriv`, and the BusyBox applet at /bin/setpriv (which lacks
	// --bounding-set) shadows the util-linux /usr/bin/setpriv baked into the
	// standard profile whenever the inherited PATH lists /bin before /usr/bin
	// — making the capability drop fail to launch. DefaultPath orders /usr/bin
	// before /bin, so the util-linux setpriv is selected.
	if err := os.Setenv("PATH", guestenv.DefaultPath); err != nil {
		// Unreachable for a constant valid key (Setenv only errors on an
		// empty key or one containing '='/NUL), but don't silently ignore it:
		// a stale PATH would reintroduce the setpriv shadowing.
		log.Printf("dew-agent: pin PATH failed: %v", err)
	}

	// If re-exec'd as the --confine shim, set up the namespace/filesystem and
	// exec the target; never returns in that case. Must run after the PATH pin
	// so the shim resolves the target binary the same way.
	maybeRunConfineShim()

	// Token is now injected via vsock handshake, not env/cmdline
	execUser = os.Getenv("DEW_EXEC_USER")

	// Periodically flush filesystem buffers. The host attaches the disk
	// Cached+Fsync, so non-fsync'd guest writes sit in the host page cache
	// until something fsyncs; an abrupt VM stop (or `dew down`) would lose
	// them. A periodic sync bounds that window for every stop path — no
	// host-side handshake needed. syscall.Sync() is the guest sync(2): it
	// flushes the guest's dirty pages to the virtio block device, and the
	// host's Cached+Fsync attachment turns that into an fsync() on the backing
	// image file (not the heavier F_FULLFSYNC), so the cost is small.
	//
	// Sleep between syncs rather than a Ticker: a Ticker keeps a pending tick
	// queued, so if one Sync() runs longer than the interval the next fires
	// immediately — degrading to back-to-back syncs under heavy write. A sleep
	// guarantees a fixed pause between sync completions regardless of duration.
	go func() {
		for {
			time.Sleep(syncInterval)
			syscall.Sync()
		}
	}()

	port := uint32(protocol.DefaultPort)
	if p := os.Getenv("DEW_VSOCK_PORT"); p != "" {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			log.Fatalf("invalid DEW_VSOCK_PORT: %v", err)
		}
		port = uint32(n)
	}

	listener, err := vsock.Listen(port, nil)
	if err != nil {
		log.Fatalf("vsock listen on port %d: %v", port, err)
	}
	defer listener.Close()

	// No in-VM capability stripping. The VM is the isolation boundary
	// and the agent's children run arbitrary user code (npm install,
	// mount --bind, ping, etc.); restricting their caps here only blocks
	// legitimate work — earlier cuts dropped CAP_NET_RAW and SIGBUS'd
	// raw-socket tools, then later CAP_SYS_ADMIN was needed for the
	// bind-mount install path.

	fmt.Fprintf(os.Stderr, "dew-agent: listening on vsock port %d\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn)
	}
}

type envelope struct {
	Type string `json:"type"`
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		// Read raw bytes to determine message type
		header := make([]byte, 4)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
		if length > 10*1024*1024 {
			return
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(conn, data); err != nil {
			return
		}

		var env envelope
		json.Unmarshal(data, &env)

		switch env.Type {
		case protocol.TypeSetToken:
			var req protocol.SetTokenRequest
			json.Unmarshal(data, &req)
			if !tokenSet {
				authToken = req.Token
				tokenSet = true
				protocol.WriteJSON(conn, &protocol.ConnectResponse{OK: true})
			} else {
				protocol.WriteJSON(conn, &protocol.ConnectResponse{Error: "token already set"})
			}
			return

		case protocol.TypeSetExposes:
			var req protocol.SetExposesRequest
			// Fail closed on a malformed frame before mutating listener state:
			// a corrupt SetExposes must not reconcile (and possibly tear down)
			// the guest's expose listeners.
			if err := json.Unmarshal(data, &req); err != nil {
				protocol.WriteJSON(conn, &protocol.ConnectResponse{Error: "bad set_exposes request"})
				return
			}
			if !isAuthorized(req.Token) {
				protocol.WriteJSON(conn, &protocol.ConnectResponse{Error: "unauthorized"})
				return
			}
			startExposeForwarders(req.Ports)
			protocol.WriteJSON(conn, &protocol.ConnectResponse{OK: true})
			return

		case protocol.TypeConnect:
			var req protocol.ConnectRequest
			json.Unmarshal(data, &req)
			// Fail-closed on missing token. Previously the check was
			// `tokenSet && req.Token != authToken`, which let any
			// Connect arriving BEFORE the host sent SetTokenRequest
			// bypass auth entirely (race window during boot).
			if !isAuthorized(req.Token) {
				protocol.WriteJSON(conn, &protocol.ConnectResponse{Error: "unauthorized"})
				return
			}
			handleConnect(conn, req.Addr)
			return

		default:
			var req protocol.ExecRequest
			json.Unmarshal(data, &req)
			// Same hardening as the Connect path above.
			if !isAuthorized(req.Token) {
				protocol.WriteJSON(conn, &protocol.ExecResponse{ExitCode: -1, Error: "unauthorized"})
				return
			}
			if req.Stream {
				executeStreaming(conn, req)
				// A streaming exec owns the connection for its lifetime
				// (the host opens a fresh vsock conn per exec). Return so
				// the read loop can't race the stdin reader goroutine for
				// the next frame; defer conn.Close() unblocks it.
				return
			} else {
				resp := executeCommand(req)
				if err := protocol.WriteJSON(conn, &resp); err != nil {
					return
				}
			}
		}
	}
}

func executeCommand(req protocol.ExecRequest) protocol.ExecResponse {
	if req.Command == "ping" {
		return protocol.ExecResponse{Stdout: "pong"}
	}

	timeout := 30 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if req.Confine.Set() {
		// Confined exec re-execs through the shim (mount-ns read-only fs); the
		// shim sets Dir/Env and applies the spec, then execs the target. uid/caps
		// still ride the host-built setpriv prefix in req.Command/Args.
		cmd = confineExecCmd(ctx, req)
	} else {
		cmd = exec.CommandContext(ctx, req.Command, req.Args...)
		if req.Dir != "" {
			cmd.Dir = req.Dir
		}
		// Always build the env (even with no request overrides) so a PATH is
		// guaranteed; otherwise bare names like `ss` resolve only by luck of
		// the agent's boot environment.
		cmd.Env = guestenv.ExecEnv(os.Environ(), req.Env)
		setExecUser(cmd)
	}

	stdout, err := cmd.Output()
	resp := protocol.ExecResponse{
		Stdout: string(stdout),
	}

	if ctx.Err() == context.DeadlineExceeded {
		resp.ExitCode = -1
		resp.Error = fmt.Sprintf("timeout after %s", timeout)
		return resp
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		resp.ExitCode = exitErr.ExitCode()
		resp.Stderr = string(exitErr.Stderr)
	} else if err != nil {
		resp.ExitCode = -1
		resp.Error = err.Error()
	}

	return resp
}

func executeStreaming(conn net.Conn, req protocol.ExecRequest) {
	// An interactive (stdin-attached) session runs until stdin closes or
	// the process exits, so it must not be bounded by the exec timeout
	// that batch commands use.
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		timeout time.Duration
	)
	if req.Stdin || req.TTY {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		timeout = 30 * time.Second
		if req.TimeoutMs > 0 {
			timeout = time.Duration(req.TimeoutMs) * time.Millisecond
		}
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	setExecUser(cmd)
	cmd.Env = guestenv.ExecEnv(os.Environ(), req.Env)

	// TTY mode runs the command on a pseudo-terminal: one merged output
	// stream, isatty true, job control. Bytes are base64 on the wire.
	if req.TTY {
		runPTY(conn, cmd, req)
		return
	}

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	// When the host opted into stdin, wire a pipe and feed it from
	// InputChunk frames arriving on the same conn (vsock is full-duplex,
	// so reading stdin here while writing output below is safe).
	var stdinPipe io.WriteCloser
	if req.Stdin {
		stdinPipe, _ = cmd.StdinPipe()
	}

	if err := cmd.Start(); err != nil {
		protocol.WriteJSON(conn, &protocol.ExecDone{ExitCode: -1, Error: err.Error()})
		return
	}

	if stdinPipe != nil {
		go func() {
			defer stdinPipe.Close()
			for {
				var in protocol.InputChunk
				if err := protocol.ReadJSON(conn, &in); err != nil {
					return // conn closed/errored → EOF the process stdin
				}
				if in.Data != "" {
					if _, err := io.WriteString(stdinPipe, in.Data); err != nil {
						return
					}
				}
				if in.EOF {
					return
				}
			}
		}()
	}

	var wg sync.WaitGroup
	streamPipe := func(pipe io.Reader, stream string) {
		defer wg.Done()
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			chunk := protocol.OutputChunk{Stream: stream, Data: scanner.Text() + "\n"}
			if err := protocol.WriteJSON(conn, &chunk); err != nil {
				return
			}
		}
	}

	wg.Add(2)
	go streamPipe(stdoutPipe, "stdout")
	go streamPipe(stderrPipe, "stderr")
	wg.Wait()

	exitCode := 0
	errMsg := ""
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			errMsg = err.Error()
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		exitCode = -1
		errMsg = fmt.Sprintf("timeout after %s", timeout)
	}

	protocol.WriteJSON(conn, &protocol.ExecDone{ExitCode: exitCode, Error: errMsg})
}

// runPTY runs cmd attached to a pseudo-terminal. Output is one merged
// raw stream; input carries stdin bytes and window resizes. Terminal
// bytes are binary, so InputChunk/OutputChunk Data is base64 in TTY mode.
func runPTY(conn net.Conn, cmd *exec.Cmd, req protocol.ExecRequest) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		protocol.WriteJSON(conn, &protocol.ExecDone{ExitCode: -1, Error: err.Error()})
		return
	}
	defer ptmx.Close()
	if req.Rows > 0 || req.Cols > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
	}

	// host → pty: stdin bytes (base64) and window-size changes.
	go func() {
		for {
			var in protocol.InputChunk
			if err := protocol.ReadJSON(conn, &in); err != nil {
				return
			}
			if in.Winch {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: in.Rows, Cols: in.Cols})
				continue
			}
			if in.Data != "" {
				if b, derr := base64.StdEncoding.DecodeString(in.Data); derr == nil {
					ptmx.Write(b)
				}
			}
			if in.EOF {
				return
			}
		}
	}()

	// pty → host: raw bytes, base64-framed.
	buf := make([]byte, 32*1024)
	for {
		n, rerr := ptmx.Read(buf)
		if n > 0 {
			chunk := protocol.OutputChunk{Stream: "stdout", Data: base64.StdEncoding.EncodeToString(buf[:n])}
			if werr := protocol.WriteJSON(conn, &chunk); werr != nil {
				break
			}
		}
		if rerr != nil {
			break // pty EOF: the shell exited
		}
	}

	exitCode := 0
	errMsg := ""
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
			errMsg = err.Error()
		}
	}
	protocol.WriteJSON(conn, &protocol.ExecDone{ExitCode: exitCode, Error: errMsg})
}

func handleConnect(vsockConn net.Conn, addr string) {
	tcpConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		protocol.WriteJSON(vsockConn, &protocol.ConnectResponse{Error: err.Error()})
		return
	}
	defer tcpConn.Close()

	protocol.WriteJSON(vsockConn, &protocol.ConnectResponse{OK: true})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(tcpConn, vsockConn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(vsockConn, tcpConn)
	}()
	wg.Wait()
}

// exposeListenIP is the guest loopback alias the reverse host-forward listens
// on. host.lo.internal resolves here (set in init-stage2 / dew-oci-run), so a
// guest dev server or a --net=host container reaches a macOS host service at
// host.lo.internal:<port> without colliding with 127.0.0.1 services in the
// same VM.
const exposeListenIP = "127.0.0.2"

// reverseDialRespTimeout bounds how long the guest waits for the host's
// ReverseDialResponse handshake. A stalled host listener or wedged vsock
// transport must not block this goroutine (and its TCP conn) forever.
const reverseDialRespTimeout = 5 * time.Second

var (
	exposeMu        sync.Mutex
	exposeListeners = map[int]net.Listener{}
)

// startExposeForwarders reconciles the guest's reverse-forward listeners to
// `ports` as the desired full set: it opens a 127.0.0.2:<port> listener for
// each newly declared port (tunnelling accepted connections to the host over
// ReverseForwardPort, where dew dials the macOS loopback) and closes any
// previously started listener no longer in the set. Treating SetExposes as the
// source of truth means a re-send with a smaller set reclaims the dropped
// ports instead of leaving them dangling on 127.0.0.2.
func startExposeForwarders(ports []int) {
	desired := make(map[int]bool, len(ports))
	for _, p := range ports {
		if p >= 1 && p <= 65535 {
			desired[p] = true
		}
	}

	exposeMu.Lock()
	defer exposeMu.Unlock()

	// Drop listeners no longer desired (closing the listener unblocks its
	// acceptExposeConns loop).
	for port, ln := range exposeListeners {
		if !desired[port] {
			ln.Close()
			delete(exposeListeners, port)
		}
	}
	// Start listeners newly desired.
	for port := range desired {
		if _, running := exposeListeners[port]; running {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", exposeListenIP, port))
		if err != nil {
			log.Printf("expose: listen %s:%d: %v", exposeListenIP, port, err)
			continue
		}
		exposeListeners[port] = ln
		go acceptExposeConns(ln, port)
	}
}

func acceptExposeConns(ln net.Listener, port int) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleExposeConn(c, port)
	}
}

// handleExposeConn tunnels one guest-side connection to the host: open a vsock
// stream to the host's ReverseForwardPort, send a ReverseDialRequest for this
// port, and on OK proxy bytes both ways. The host enforces token + allow-set.
func handleExposeConn(tcpConn net.Conn, port int) {
	defer tcpConn.Close()
	vsockConn, err := vsock.Dial(vsock.Host, protocol.ReverseForwardPort, nil)
	if err != nil {
		return
	}
	defer vsockConn.Close()

	if err := protocol.WriteJSON(vsockConn, &protocol.ReverseDialRequest{
		Type: protocol.TypeReverseDial, Token: authToken, Port: port,
	}); err != nil {
		return
	}
	var resp protocol.ReverseDialResponse
	if err := protocol.ReadJSONTimeout(vsockConn, &resp, reverseDialRespTimeout); err != nil || !resp.OK {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(vsockConn, tcpConn) }()
	go func() { defer wg.Done(); io.Copy(tcpConn, vsockConn) }()
	wg.Wait()
}

func setExecUser(cmd *exec.Cmd) {
	if execUser == "" {
		return
	}
	u, err := user.Lookup(execUser)
	if err != nil {
		return
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
}
