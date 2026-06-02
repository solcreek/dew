//go:build darwin

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"time"

	"github.com/solcreek/dew/internal/validate"
)

type fakeFileInfo struct {
	name  string
	isDir bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }

// ── Validation tests ──

func TestValidateAppName(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"excalidraw", true},
		{"uptime-kuma", true},
		{"ghost", true},
		{"my-app-123", true},
		{"", false},
		{"../etc/passwd", false},
		{"app?token=x", false},
		{"app#section", false},
		{"app%20name", false},
		{"path/traversal", false},
		{"has space", false},
		{"control\x00char", false},
	}
	for _, tt := range tests {
		err := validate.AppName(tt.input)
		if tt.ok && err != nil {
			t.Errorf("AppName(%q) should pass, got: %v", tt.input, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("AppName(%q) should fail", tt.input)
		}
	}
}

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		input string
		ok    bool
	}{
		{"1.2.3.4", true},
		{"my-server.com", true},
		{"http://localhost:9080", true},
		{"https://api.creek.dev", true},
		{"", false},
		{"../../etc", false},
		{"host name", false},
	}
	for _, tt := range tests {
		err := validate.Target(tt.input)
		if tt.ok && err != nil {
			t.Errorf("Target(%q) should pass, got: %v", tt.input, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("Target(%q) should fail", tt.input)
		}
	}
}

func TestValidatePort(t *testing.T) {
	if err := validate.Port(80); err != nil {
		t.Error(err)
	}
	if err := validate.Port(3000); err != nil {
		t.Error(err)
	}
	if err := validate.Port(65535); err != nil {
		t.Error(err)
	}
	if err := validate.Port(0); err == nil {
		t.Error("port 0 should fail")
	}
	if err := validate.Port(70000); err == nil {
		t.Error("port 70000 should fail")
	}
}

// ── Endpoint resolution ──

func TestResolveEndpoint(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.2.3.4", "https://1.2.3.4:9080"},
		{"myserver.com", "https://myserver.com:9080"},
		{"http://localhost:9080", "http://localhost:9080"},
		{"https://api.creek.dev", "https://api.creek.dev"},
		{"https://example.com/", "https://example.com"},
	}
	for _, tt := range tests {
		got := resolveEndpoint(tt.input)
		if got != tt.want {
			t.Errorf("resolveEndpoint(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── App name inference ──

func TestInferAppName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-app.tar.gz", "my-app"},
		{"/tmp/demo.tar.gz", "demo"},
		{"app.tgz", "app"},
	}
	for _, tt := range tests {
		got := inferAppName(tt.input)
		if got != tt.want {
			t.Errorf("inferAppName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── Credential store ──

func TestCredentialStore(t *testing.T) {
	// Use temp dir
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Save
	err := saveCredentials("test.host", "crk_admin_abc123")
	if err != nil {
		t.Fatal(err)
	}

	// Load
	store := loadCredentialStore()
	if store.Credentials["test.host"] != "crk_admin_abc123" {
		t.Errorf("expected crk_admin_abc123, got %q", store.Credentials["test.host"])
	}

	// Remove
	removeCredentials("test.host")
	store = loadCredentialStore()
	if _, ok := store.Credentials["test.host"]; ok {
		t.Error("credential should be removed")
	}
}

// ── Token loading ──

func TestLoadDeployToken_EnvVar(t *testing.T) {
	os.Setenv("DEW_TOKEN", "test-token-env")
	defer os.Unsetenv("DEW_TOKEN")

	token, err := loadDeployToken("any-target")
	if err != nil {
		t.Fatal(err)
	}
	if token != "test-token-env" {
		t.Errorf("got %q, want test-token-env", token)
	}
}

func TestLoadDeployToken_Missing(t *testing.T) {
	os.Unsetenv("DEW_TOKEN")
	os.Unsetenv("CREEK_TOKEN")

	_, err := loadDeployToken("nonexistent-host")
	if err == nil {
		t.Error("expected error for missing token")
	}
}

// ── Deploy (mock server) ──

func TestDeployTarball_Checksum(t *testing.T) {
	// Create a mock dew serve
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		checksum := r.Header.Get("X-Deploy-Checksum")
		if checksum == "" {
			http.Error(w, `{"error":"no checksum"}`, 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    true,
			"app":   "test",
			"bytes": len(body),
		})
	}))
	defer srv.Close()

	// Create a test tarball
	tarball := filepath.Join(t.TempDir(), "test.tar.gz")
	os.WriteFile(tarball, []byte("fake tarball content"), 0644)

	os.Setenv("DEW_TOKEN", "test-token")
	defer os.Unsetenv("DEW_TOKEN")

	err := deployTarball(srv.URL, srv.URL, "test-token", tarball, "test-app")
	if err != nil {
		t.Fatalf("deployTarball failed: %v", err)
	}
}

func TestDeployTarball_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, 401)
	}))
	defer srv.Close()

	tarball := filepath.Join(t.TempDir(), "test.tar.gz")
	os.WriteFile(tarball, []byte("fake"), 0644)

	err := deployTarball(srv.URL, srv.URL, "bad-token", tarball, "test-app")
	if err == nil {
		t.Error("expected auth error")
	}
}

// ── Build (skip set) ──

func TestBuildSkipSet_Preserves_Dist(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist\nnode_modules\n"), 0644)

	skip := buildSkipSet(dir)

	if !skip["node_modules"] {
		t.Error("should skip node_modules")
	}
	if skip["dist"] {
		t.Error("should NOT skip dist (build output)")
	}
	if !skip[".git"] {
		t.Error("should skip .git")
	}
	if !skip[".claude"] {
		t.Error("should skip .claude")
	}
}

func TestShouldSkip_Files(t *testing.T) {
	skip := map[string]bool{
		".git":         true,
		"node_modules": true,
	}
	tests := []struct {
		rel  string
		want bool
	}{
		{".", false},
		{".git", true},
		{"node_modules", true},
		{"src/App.tsx", false},
		{"data.db", true},
		{"notes.db-wal", true},
		{"app.tar.gz", true},
		{"package-lock.json", true},
		{"bun.lock", true},
	}
	for _, tt := range tests {
		info := fakeFileInfo{name: tt.rel}
		got := shouldSkip(tt.rel, info, skip)
		if got != tt.want {
			t.Errorf("shouldSkip(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

// ── Manifest fetch (mock) ──
// Removed in v0.7.20 with the apps surface itself.

// ── Token generation ──

func TestGenerateDewToken(t *testing.T) {
	t1, err := generateDewToken()
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := generateDewToken()
	if t1 == t2 {
		t.Error("tokens should be unique")
	}
	if len(t1) != len("crk_admin_")+48 {
		t.Errorf("token length = %d", len(t1))
	}
}

// ── Cloud-init security ──

func TestCloudInit_NoPlaintextToken(t *testing.T) {
	ci := generateCloudInit("crk_admin_secret123")
	if contains(ci, "crk_admin_secret123") {
		t.Error("cloud-init must NOT contain plaintext token")
	}
	h := hashToken("crk_admin_secret123")
	if !contains(ci, h) {
		t.Error("cloud-init should contain token hash")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// ── Human size formatting ──

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500B"},
		{1024, "1KB"},
		{77000, "75KB"},
		{1048576, "1.0MB"},
		{5242880, "5.0MB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
