package selfupdate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.4.2", "0.4.2", 0},
		{"v0.4.2", "0.4.2", 0},
		{"0.4.2", "v0.4.2", 0},
		{"0.4.3", "0.4.2", 1},
		{"0.4.2", "0.4.3", -1},
		{"0.5.0", "0.4.9", 1},
		{"0.4.10", "0.4.9", 1},    // semver, not string compare
		{"1.0.0", "0.99.99", 1},
		{"0.4.2", "0.4.2-beta", 0}, // pre-release stripped
		{"0.3.0", "0.4.0", -1},
		{"1.0.0", "0.1.0", 1},
	}
	for _, tt := range tests {
		got := CompareSemver(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"0.4.2", [3]int{0, 4, 2}},
		{"1.0.0", [3]int{1, 0, 0}},
		{"0.4.10", [3]int{0, 4, 10}},
		{"0.4", [3]int{0, 4, 0}},
		{"3", [3]int{3, 0, 0}},
		{"1.2.3-beta", [3]int{1, 2, 3}},
	}
	for _, tt := range tests {
		got := parseSemver(tt.input)
		if got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFetchLatest_MockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releaseInfo{TagName: "v0.5.0"})
	}))
	defer srv.Close()

	old := apiURL
	apiURL = srv.URL
	defer func() { apiURL = old }()

	latest, err := fetchLatest()
	if err != nil {
		t.Fatal(err)
	}
	if latest != "v0.5.0" {
		t.Errorf("got %q, want v0.5.0", latest)
	}
}

func TestFetchLatest_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	old := apiURL
	apiURL = srv.URL
	defer func() { apiURL = old }()

	_, err := fetchLatest()
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestFetchChecksums(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abc123def456  dew-darwin-arm64\n789xyz  dew-linux-amd64\n"))
	}))
	defer srv.Close()

	checksums, err := fetchChecksums(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if checksums["dew-darwin-arm64"] != "abc123def456" {
		t.Errorf("darwin checksum = %q", checksums["dew-darwin-arm64"])
	}
	if checksums["dew-linux-amd64"] != "789xyz" {
		t.Errorf("linux checksum = %q", checksums["dew-linux-amd64"])
	}
}

func TestBinaryAsset(t *testing.T) {
	asset := BinaryAsset()
	if asset == "" {
		t.Error("empty asset name")
	}
	// Should contain platform info
	if asset != "dew-darwin-arm64" && asset != "dew-darwin-amd64" &&
		asset != "dew-linux-amd64" && asset != "dew-linux-arm64" &&
		asset != "dew-windows-x86_64.exe" {
		// May be other platforms in CI, just check it's not empty
		t.Logf("asset: %s (unexpected but not fatal)", asset)
	}
}

func TestUpdateCache(t *testing.T) {
	// Setup mock server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releaseInfo{TagName: "v0.6.0"})
	}))
	defer srv.Close()

	old := apiURL
	apiURL = srv.URL
	defer func() { apiURL = old }()

	// Use temp home
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// First call — fetches from server
	latest, err := cachedLatest()
	if err != nil {
		t.Fatal(err)
	}
	if latest != "v0.6.0" {
		t.Errorf("got %q, want v0.6.0", latest)
	}

	// Verify cache written
	cachePath := filepath.Join(tmpDir, ".config", "dew", cacheFile)
	if _, err := os.Stat(cachePath); err != nil {
		t.Error("cache file not created")
	}

	// Second call — should use cache (even if server is down)
	srv.Close()
	latest2, err := cachedLatest()
	if err != nil {
		t.Fatal(err)
	}
	if latest2 != "v0.6.0" {
		t.Errorf("cached got %q, want v0.6.0", latest2)
	}
}
