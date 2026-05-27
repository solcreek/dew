package vsock

import (
	"bytes"
	"testing"
)

func TestWriteReadJSON_ExecRequest(t *testing.T) {
	req := ExecRequest{
		Command: "ls",
		Args:    []string{"-la", "/tmp"},
		Dir:     "/home",
		Env:     []string{"FOO=bar"},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}

	var got ExecRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Command != req.Command {
		t.Errorf("Command = %q, want %q", got.Command, req.Command)
	}
	if len(got.Args) != 2 || got.Args[0] != "-la" {
		t.Errorf("Args = %v, want %v", got.Args, req.Args)
	}
	if got.Dir != "/home" {
		t.Errorf("Dir = %q, want %q", got.Dir, "/home")
	}
	if len(got.Env) != 1 || got.Env[0] != "FOO=bar" {
		t.Errorf("Env = %v, want %v", got.Env, req.Env)
	}
}

func TestWriteReadJSON_ExecResponse(t *testing.T) {
	resp := ExecResponse{
		ExitCode: 0,
		Stdout:   "hello world\n",
		Stderr:   "",
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &resp); err != nil {
		t.Fatal(err)
	}

	var got ExecResponse
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", got.ExitCode)
	}
	if got.Stdout != "hello world\n" {
		t.Errorf("Stdout = %q, want %q", got.Stdout, "hello world\n")
	}
}

func TestWriteReadJSON_ErrorResponse(t *testing.T) {
	resp := ExecResponse{
		ExitCode: 127,
		Error:    "command not found",
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &resp); err != nil {
		t.Fatal(err)
	}

	var got ExecResponse
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", got.ExitCode)
	}
	if got.Error != "command not found" {
		t.Errorf("Error = %q, want %q", got.Error, "command not found")
	}
}

func TestReadJSON_PayloadTooLarge(t *testing.T) {
	header := []byte{0x01, 0x00, 0x00, 0x00} // 16MB
	buf := bytes.NewReader(header)
	var got ExecRequest
	err := ReadJSON(buf, &got)
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestReadJSON_TruncatedHeader(t *testing.T) {
	buf := bytes.NewReader([]byte{0x00, 0x00})
	var got ExecRequest
	err := ReadJSON(buf, &got)
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestWriteReadJSON_MultipleMessages(t *testing.T) {
	var buf bytes.Buffer

	req1 := ExecRequest{Command: "echo", Args: []string{"first"}}
	req2 := ExecRequest{Command: "echo", Args: []string{"second"}}

	if err := WriteJSON(&buf, &req1); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&buf, &req2); err != nil {
		t.Fatal(err)
	}

	var got1, got2 ExecRequest
	if err := ReadJSON(&buf, &got1); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(&buf, &got2); err != nil {
		t.Fatal(err)
	}
	if got1.Args[0] != "first" {
		t.Errorf("first message args = %v", got1.Args)
	}
	if got2.Args[0] != "second" {
		t.Errorf("second message args = %v", got2.Args)
	}
}

func TestPingResponse(t *testing.T) {
	resp := PingResponse{Status: "ok"}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &resp); err != nil {
		t.Fatal(err)
	}
	var got PingResponse
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want %q", got.Status, "ok")
	}
}

func TestStreamingProtocol(t *testing.T) {
	var buf bytes.Buffer

	// Simulate streaming: 2 chunks + done
	WriteJSON(&buf, &OutputChunk{Stream: "stdout", Data: "line 1\n"})
	WriteJSON(&buf, &OutputChunk{Stream: "stderr", Data: "warn\n"})
	WriteJSON(&buf, &ExecDone{ExitCode: 0})

	var chunk1 OutputChunk
	if err := ReadJSON(&buf, &chunk1); err != nil {
		t.Fatal(err)
	}
	if chunk1.Stream != "stdout" || chunk1.Data != "line 1\n" {
		t.Errorf("chunk1 = %+v", chunk1)
	}

	var chunk2 OutputChunk
	if err := ReadJSON(&buf, &chunk2); err != nil {
		t.Fatal(err)
	}
	if chunk2.Stream != "stderr" || chunk2.Data != "warn\n" {
		t.Errorf("chunk2 = %+v", chunk2)
	}

	var done ExecDone
	if err := ReadJSON(&buf, &done); err != nil {
		t.Fatal(err)
	}
	if done.ExitCode != 0 {
		t.Errorf("done.ExitCode = %d", done.ExitCode)
	}
}

func TestExecRequestStream(t *testing.T) {
	req := ExecRequest{Token: "abc", Command: "ls", Stream: true}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got ExecRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Stream {
		t.Error("Stream should be true")
	}
}

func TestConnectRequest(t *testing.T) {
	req := ConnectRequest{Type: TypeConnect, Token: "tok123", Addr: "127.0.0.1:5432"}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got ConnectRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeConnect {
		t.Errorf("Type = %q, want %q", got.Type, TypeConnect)
	}
	if got.Token != "tok123" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.Addr != "127.0.0.1:5432" {
		t.Errorf("Addr = %q", got.Addr)
	}
}

func TestConnectResponse(t *testing.T) {
	tests := []struct {
		name string
		resp ConnectResponse
	}{
		{"success", ConnectResponse{OK: true}},
		{"error", ConnectResponse{OK: false, Error: "connection refused"}},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, &tt.resp); err != nil {
			t.Fatal(err)
		}
		var got ConnectResponse
		if err := ReadJSON(&buf, &got); err != nil {
			t.Fatal(err)
		}
		if got.OK != tt.resp.OK {
			t.Errorf("%s: OK = %v, want %v", tt.name, got.OK, tt.resp.OK)
		}
		if got.Error != tt.resp.Error {
			t.Errorf("%s: Error = %q, want %q", tt.name, got.Error, tt.resp.Error)
		}
	}
}

func TestExecRequestWithTimeout(t *testing.T) {
	req := ExecRequest{Token: "t", Command: "sleep", Args: []string{"10"}, TimeoutMs: 5000}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got ExecRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.TimeoutMs != 5000 {
		t.Errorf("TimeoutMs = %d, want 5000", got.TimeoutMs)
	}
}

func TestExecRequestWithToken(t *testing.T) {
	req := ExecRequest{Token: "secret-token-abc", Command: "ls"}
	var buf bytes.Buffer
	WriteJSON(&buf, &req)
	var got ExecRequest
	ReadJSON(&buf, &got)
	if got.Token != "secret-token-abc" {
		t.Errorf("Token = %q", got.Token)
	}
}

func TestSetTokenRequest(t *testing.T) {
	req := SetTokenRequest{Type: TypeSetToken, Token: "my-secret-token"}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got SetTokenRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != TypeSetToken {
		t.Errorf("Type = %q, want %q", got.Type, TypeSetToken)
	}
	if got.Token != "my-secret-token" {
		t.Errorf("Token = %q", got.Token)
	}
}
