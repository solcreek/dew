//go:build darwin

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/internal/vmstate"
)

// listen binds a Unix socket and drains accepts, so a dial probe sees a
// live listener. Mirrors the helper pattern in status_test.go.
func listen(t *testing.T, sock string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

func TestCmdList_Empty(t *testing.T) {
	_ = withTempSocketPath(t)
	flagJSON = false
	out := captureStdout(t, func() { _ = cmdList(nil) })
	if !strings.Contains(out, "no VMs running") {
		t.Errorf("expected empty-list message, got: %q", out)
	}
}

// list must show the default VM and named VMs that are actually up, and
// omit crash leftovers (a stale socket file with no listener / no state).
func TestCmdList_ShowsRunningAndOmitsStale(t *testing.T) {
	defSock := withTempSocketPath(t)
	base := filepath.Dir(defSock)

	// Default VM: live socket + live state.
	listen(t, defSock)
	if err := vmstate.Write(vmstate.DirFor(base, ""), vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "start",
		Profile: "minimal", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// Named VM "alice": live socket + live state.
	listen(t, daemon.SocketPath("alice"))
	if err := vmstate.Write(vmstate.DirFor(base, "alice"), vmstate.State{
		PID: os.Getpid(), Phase: vmstate.PhaseRunning, Mode: "start",
		Profile: "node", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// "bob": stale socket file, no listener, no state → crash leftover.
	if err := os.WriteFile(daemon.SocketPath("bob"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	flagJSON = true
	defer func() { flagJSON = false }()
	out := captureStdout(t, func() { _ = cmdList(nil) })

	var env struct {
		Data struct {
			VMs []struct {
				Name    string `json:"name"`
				Running bool   `json:"running"`
				Profile string `json:"profile"`
			} `json:"vms"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("bad JSON %q: %v", out, err)
	}

	got := map[string]bool{}
	for _, v := range env.Data.VMs {
		got[v.Name] = v.Running
	}
	if running, ok := got["default"]; !ok || !running {
		t.Errorf("default VM missing or not running: %+v", env.Data.VMs)
	}
	if running, ok := got["alice"]; !ok || !running {
		t.Errorf("alice missing or not running: %+v", env.Data.VMs)
	}
	if _, ok := got["bob"]; ok {
		t.Errorf("stale bob should be omitted: %+v", env.Data.VMs)
	}
}

// Stopping a named VM must remove its empty <name>/ state dir, while the
// shared default state root is never removed.
func TestClearVMState_RemovesNamedDirNotDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	base := daemon.SocketDir()
	pid := os.Getpid()

	aliceDir := vmstate.DirFor(base, "alice")
	if err := vmstate.Write(aliceDir, vmstate.State{PID: pid, Phase: vmstate.PhaseRunning, Mode: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := vmstate.Write(vmstate.DirFor(base, ""), vmstate.State{PID: pid, Phase: vmstate.PhaseRunning, Mode: "start"}); err != nil {
		t.Fatal(err)
	}

	clearVMState("alice", pid)
	if _, err := os.Stat(aliceDir); !os.IsNotExist(err) {
		t.Errorf("named state dir %q survived stop (err=%v)", aliceDir, err)
	}

	clearVMState("", pid)
	if _, err := os.Stat(base); err != nil {
		t.Errorf("default state root must not be removed: %v", err)
	}
}
