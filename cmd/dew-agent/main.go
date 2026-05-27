//go:build linux

// dew-agent runs inside the VM guest, listens on vsock and executes
// commands sent by the host.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/mdlayher/vsock"
	protocol "github.com/solcreek/dew/internal/vsock"
)

var authToken string

func main() {
	authToken = os.Getenv("DEW_TOKEN")

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

func handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		var req protocol.ExecRequest
		if err := protocol.ReadJSON(conn, &req); err != nil {
			return
		}

		if authToken != "" && req.Token != authToken {
			resp := protocol.ExecResponse{ExitCode: -1, Error: "unauthorized"}
			protocol.WriteJSON(conn, &resp)
			return
		}

		resp := executeCommand(req)
		if err := protocol.WriteJSON(conn, &resp); err != nil {
			return
		}
	}
}

func executeCommand(req protocol.ExecRequest) protocol.ExecResponse {
	if req.Command == "ping" {
		return protocol.ExecResponse{Stdout: "pong"}
	}

	cmd := exec.Command(req.Command, req.Args...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}

	stdout, err := cmd.Output()
	resp := protocol.ExecResponse{
		Stdout: string(stdout),
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
