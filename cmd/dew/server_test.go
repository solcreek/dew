//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/solcreek/capstan"
)

// ─── stripJSONFlag ─────────────────────────────────────────────────

func TestStripJSONFlag_NoFlag(t *testing.T) {
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = false

	out := stripJSONFlag([]string{"list", "--region", "fsn1"})
	if flagJSON {
		t.Error("flagJSON should remain false when --json absent")
	}
	if len(out) != 3 || out[0] != "list" {
		t.Errorf("expected positional args preserved, got %v", out)
	}
}

func TestStripJSONFlag_SetsFlagAndStrips(t *testing.T) {
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = false

	out := stripJSONFlag([]string{"list", "--json", "extra"})
	if !flagJSON {
		t.Error("flagJSON should be true after stripping --json")
	}
	if len(out) != 2 || out[0] != "list" || out[1] != "extra" {
		t.Errorf("--json should be removed from positionals, got %v", out)
	}
}

func TestStripJSONFlag_MultipleJSONFlagsIdempotent(t *testing.T) {
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = false

	out := stripJSONFlag([]string{"--json", "list", "--json"})
	if !flagJSON {
		t.Error("flagJSON should be true with any --json present")
	}
	if len(out) != 1 || out[0] != "list" {
		t.Errorf("all --json instances should be stripped, got %v", out)
	}
}

// ─── recordJSON / serverJSON shape contracts ───────────────────────

func TestRecordJSONShape(t *testing.T) {
	r := serverRecord{
		ID: "42", Name: "web", IP: "1.2.3.4",
		Provider: "hetzner", Region: "fsn1", Plan: "cx23",
	}
	got := recordJSON(r)
	for _, key := range []string{"id", "name", "ip", "provider", "region", "plan"} {
		if _, ok := got[key]; !ok {
			t.Errorf("recordJSON missing key %q", key)
		}
	}
	if got["name"] != "web" || got["ip"] != "1.2.3.4" {
		t.Errorf("recordJSON values wrong: %+v", got)
	}
}

func TestServerJSON_OmitsEmptyIPv6(t *testing.T) {
	srv := &capstan.Server{
		ID: "1", Name: "x", Status: capstan.StatusRunning,
		PublicIPv4: "1.2.3.4", Region: "fsn1", Plan: "cx23",
	}
	got := serverJSON(srv)
	if _, ok := got["publicIPv6"]; ok {
		t.Error("publicIPv6 should be omitted when empty")
	}
	if got["status"] != "running" {
		t.Errorf("status = %v, want running", got["status"])
	}
}

func TestServerJSON_IncludesIPv6WhenSet(t *testing.T) {
	srv := &capstan.Server{
		ID: "1", Name: "x", Status: capstan.StatusRunning,
		PublicIPv4: "1.2.3.4", PublicIPv6: "2001:db8::1",
		Region: "fsn1", Plan: "cx23",
	}
	got := serverJSON(srv)
	if got["publicIPv6"] != "2001:db8::1" {
		t.Errorf("publicIPv6 = %v, want 2001:db8::1", got["publicIPv6"])
	}
}

// ─── emitJSON contract ─────────────────────────────────────────────

func TestEmitJSON_AlwaysSetsOkTrue(t *testing.T) {
	out := captureStdout(t, func() {
		if err := emitJSON(map[string]any{"servers": []string{}}); err != nil {
			t.Fatalf("emitJSON: %v", err)
		}
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if parsed["ok"] != true {
		t.Errorf("ok = %v, want true", parsed["ok"])
	}
}

func TestEmitJSON_PreservesProvidedFields(t *testing.T) {
	out := captureStdout(t, func() {
		if err := emitJSON(map[string]any{
			"server": map[string]any{"id": "42", "name": "web"},
			"action": "start",
		}); err != nil {
			t.Fatalf("emitJSON: %v", err)
		}
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["action"] != "start" {
		t.Errorf("action = %v, want start", parsed["action"])
	}
	srv, _ := parsed["server"].(map[string]any)
	if srv == nil || srv["id"] != "42" {
		t.Errorf("server.id missing or wrong: %+v", srv)
	}
}

// ─── cmdServerList output paths ────────────────────────────────────

func TestCmdServerList_JSONEmpty(t *testing.T) {
	withTempServerStore(t, []serverRecord{})
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = true

	out := captureStdout(t, func() {
		if err := cmdServerList(nil); err != nil {
			t.Fatalf("cmdServerList: %v", err)
		}
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if parsed["ok"] != true {
		t.Error("ok != true on empty list")
	}
	srvs, _ := parsed["servers"].([]any)
	if len(srvs) != 0 {
		t.Errorf("expected empty servers, got %v", srvs)
	}
}

func TestCmdServerList_JSONWithEntries(t *testing.T) {
	withTempServerStore(t, []serverRecord{
		{ID: "1", Name: "web", IP: "1.2.3.4", Provider: "hetzner", Region: "fsn1", Plan: "cx23"},
		{ID: "2", Name: "db", IP: "5.6.7.8", Provider: "linode", Region: "eu-central", Plan: "g6-standard-1"},
	})
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = true

	out := captureStdout(t, func() {
		if err := cmdServerList(nil); err != nil {
			t.Fatalf("cmdServerList: %v", err)
		}
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	srvs, _ := parsed["servers"].([]any)
	if len(srvs) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(srvs))
	}
	first, _ := srvs[0].(map[string]any)
	if first["name"] != "web" || first["provider"] != "hetzner" {
		t.Errorf("first server wrong: %+v", first)
	}
}

func TestCmdServerList_TextMode(t *testing.T) {
	withTempServerStore(t, []serverRecord{
		{ID: "1", Name: "web", IP: "1.2.3.4", Provider: "hetzner", Region: "fsn1", Plan: "cx23"},
	})
	prev := flagJSON
	defer func() { flagJSON = prev }()
	flagJSON = false

	out := captureStdout(t, func() {
		if err := cmdServerList(nil); err != nil {
			t.Fatalf("cmdServerList: %v", err)
		}
	})
	if !bytes.Contains([]byte(out), []byte("NAME")) || !bytes.Contains([]byte(out), []byte("web")) {
		t.Errorf("text mode missing header or row: %q", out)
	}
	// Should NOT be valid JSON in text mode
	var parsed map[string]any
	if json.Unmarshal([]byte(out), &parsed) == nil {
		t.Errorf("text mode output should not be valid JSON: %q", out)
	}
}

// ─── helpers ───────────────────────────────────────────────────────

// withTempServerStore writes a fresh servers.json under a temp HOME and
// restores the previous HOME at test end. Lets us test cmdServerList /
// loadServers / saveServer without touching the developer's real registry.
func withTempServerStore(t *testing.T, recs []serverRecord) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".config", "dew")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := serverStore{Servers: recs}
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}
