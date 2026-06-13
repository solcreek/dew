//go:build linux

// dew-agent runs inside the VM guest, listens on vsock and executes
// commands sent by the host.
package main

import (
	"bufio"
	"context"
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

	"github.com/mdlayher/vsock"
	"github.com/solcreek/dew/internal/agentauth"
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
	// Token is now injected via vsock handshake, not env/cmdline
	execUser = os.Getenv("DEW_EXEC_USER")

	// Periodically flush filesystem buffers. The host attaches the disk
	// Cached+Fsync, so non-fsync'd guest writes sit in the host page cache
	// until something fsyncs; an abrupt VM stop (or `dew down`) would lose
	// them. A periodic sync bounds that window to the interval for every stop
	// path — no host-side handshake needed. sync() here is a regular fsync to
	// the image file (not F_FULLFSYNC), so the cost is small.
	go func() {
		t := time.NewTicker(syncInterval)
		defer t.Stop()
		for range t.C {
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

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}
	setExecUser(cmd)

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
	timeout := 30 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	setExecUser(cmd)
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		protocol.WriteJSON(conn, &protocol.ExecDone{ExitCode: -1, Error: err.Error()})
		return
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
