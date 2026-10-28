package vsock

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden from the Go encoder")

// goldenFrames is one value per wire shape. The Go encoder writes them to
// testdata/golden/<name>.json; the Rust dew-proto crate decodes each file into
// its mirror type and re-encodes it, so both sides must agree on field names,
// omitempty and defaults. Add a case here whenever a message type or field is
// added, then run `go test ./internal/vsock -run Golden -update`.
var goldenFrames = map[string]any{
	"set_token":                 &SetTokenRequest{Type: TypeSetToken, Token: "tok"},
	"set_exposes":               &SetExposesRequest{Type: TypeSetExposes, Token: "tok", Ports: []int{3000, 5432}},
	"set_exposes_nil_ports":     &SetExposesRequest{Type: TypeSetExposes, Token: "tok"}, // nil slice without omitempty encodes as null
	"reverse_dial_request":      &ReverseDialRequest{Type: TypeReverseDial, Token: "tok", Port: 8080},
	"reverse_dial_response_ok":  &ReverseDialResponse{OK: true},
	"reverse_dial_response_err": &ReverseDialResponse{Error: "port 22 not exposed"},
	"exec_minimal":              &ExecRequest{Token: "tok", Command: "/bin/true"},
	"exec_full": &ExecRequest{
		Type: TypeExec, Token: "tok", Command: "/bin/sh", Args: []string{"-c", "echo hi"},
		Dir: "/app", Env: []string{"A=1", "B=two words"}, TimeoutMs: 1500, Stream: true, Stdin: true,
	},
	"exec_tty": &ExecRequest{Token: "tok", Command: "/bin/sh", Stream: true, Stdin: true, TTY: true, Rows: 40, Cols: 120},
	"exec_confine": &ExecRequest{Token: "tok", Command: "/usr/bin/id", Confine: &Confinement{
		User: "nobody", Group: "nogroup", NoNewPrivs: true, DropAllCaps: true,
		KeepCaps: []string{"cap_net_bind_service"}, ReadOnlyRoot: true,
		ReadWritePaths: []string{"/var/lib/app"}, AddressFamilies: []string{"AF_UNIX", "AF_INET"},
		SystemCalls: []string{"ptrace", "mount"}, SystemCallsDeny: true,
	}},
	"exec_confine_dynamic": &ExecRequest{Token: "tok", Command: "/bin/true", Confine: &Confinement{
		DynamicUser: true, DropCaps: []string{"cap_sys_admin"}, AddressFamilies: []string{"AF_PACKET"}, AddressFamiliesDeny: true,
	}},
	"input_data":            &InputChunk{Data: "ls\n"},
	"input_eof":             &InputChunk{EOF: true},
	"input_winch":           &InputChunk{Winch: true, Rows: 50, Cols: 200},
	"exec_response_ok":      &ExecResponse{Stdout: "out\n"},
	"exec_response_fail":    &ExecResponse{ExitCode: 2, Stdout: "", Stderr: "no such file\n"},
	"exec_response_error":   &ExecResponse{ExitCode: -1, Error: "timeout after 30s"},
	"output_chunk":          &OutputChunk{Stream: "stderr", Data: "warn: \"quoted\" \\ \u00e9\n"},
	"exec_done_ok":          &ExecDone{},
	"exec_done_error":       &ExecDone{ExitCode: -1, Error: "exec: not found"},
	"connect_request":       &ConnectRequest{Type: TypeConnect, Token: "tok", Addr: "127.0.0.1:3000"},
	"connect_response_ok":   &ConnectResponse{OK: true},
	"connect_response_err":  &ConnectResponse{Error: "unauthorized"},
	"set_token_ack_confine": &ConnectResponse{OK: true, Confine: true},
	"ping_response":         &PingResponse{Status: "ok"},
}

func TestGoldenFrames(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	if *updateGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, v := range goldenFrames {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name+".json")
			if *updateGolden {
				if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden file (run with -update): %v", err)
			}
			if !bytes.Equal(got, bytes.TrimRight(want, "\n")) {
				t.Errorf("encoding drifted from %s:\n got %s\nwant %s", path, got, want)
			}
			// Decoding the golden bytes must round-trip to the same value.
			fresh := reflect.New(reflect.TypeOf(v).Elem()).Interface()
			if err := json.Unmarshal(want, fresh); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fresh, v) {
				t.Errorf("decode mismatch for %s:\n got %+v\nwant %+v", path, fresh, v)
			}
		})
	}
}

// Every golden file must have a case, so a stale fixture can't linger and
// mislead the Rust side.
func TestGoldenFrames_NoOrphans(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := filepath.Base(f[:len(f)-len(".json")])
		if _, ok := goldenFrames[name]; !ok {
			t.Errorf("%s has no entry in goldenFrames", f)
		}
	}
}
