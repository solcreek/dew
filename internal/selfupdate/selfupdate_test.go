package selfupdate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// captureStderr redirects os.Stderr through a pipe while fn runs and
// returns whatever was written. Used to assert "the noisy thing
// didn't print" cases.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// updateCache is the on-disk shape selfupdate writes; we mirror it here
// so the test seeds a plausible cache for cachedLatest() to read. Pin
// FetchedAt to "now" so the cache isn't considered stale.
type testCache struct {
	Latest    string    `json:"latest"`
	FetchedAt time.Time `json:"fetched_at"`
}

// Regression for 2026-06: when the binary was built without ldflags
// stamping (any `make sign` / bare `go build`), version stays "dev" and
// CompareSemver("0.7.x", "dev") returns > 0. The pre-fix CheckBackground
// then printed "Update available: vX.Y.Z (current: vdev)" on every
// invocation — even when the local dev binary was ahead of the released
// version. The user knows what they built; we shouldn't nag.
func TestCheckBackground_SilentOnDevBuild(t *testing.T) {
	// Point configDir() at a tempdir AND seed a "newer" cached latest
	// that would otherwise trigger the print.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	cfgDir := filepath.Join(tmpHome, ".config", "dew")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	cache := testCache{Latest: "v99.0.0", FetchedAt: time.Now()}
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(cfgDir, cacheFile), data, 0644); err != nil {
		t.Fatal(err)
	}

	for _, v := range []string{"dev", ""} {
		t.Run("version="+v, func(t *testing.T) {
			out := captureStderr(t, func() { CheckBackground(v) })
			if out != "" {
				t.Errorf("expected silent on version=%q, got stderr:\n%s", v, out)
			}
		})
	}

	// Sanity check the opposite direction: with a real semver that's
	// older than the cached latest, the notice MUST print. Catches a
	// fix that accidentally over-shorts the early-return.
	t.Run("real version still nags when behind", func(t *testing.T) {
		out := captureStderr(t, func() { CheckBackground("0.7.30") })
		if !strings.Contains(out, "Update available") {
			t.Errorf("expected 'Update available' notice when current < cached latest, got:\n%s", out)
		}
	})
}
