//go:build darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A release build must fetch assets from its OWN version tag, not from
// /latest/download. This is the contract that keeps a pinned dew
// version reproducible: when a newer release ships a rebuilt initramfs,
// the old pinned binary keeps pulling the exact bytes its embedded SHA
// was computed from instead of silently bricking on a SHA mismatch.
func TestReleaseAssetBaseURL_PinsToVersionTag(t *testing.T) {
	prev := version
	defer func() { version = prev }()

	cases := []struct {
		ver  string
		want string
	}{
		{"0.8.2", releaseRepoURL + "/download/v0.8.2"},
		{"v0.8.2", releaseRepoURL + "/download/v0.8.2"}, // no double-v
		{"1.0.0-rc.1", releaseRepoURL + "/download/v1.0.0-rc.1"},
	}
	for _, c := range cases {
		version = c.ver
		if got := releaseAssetBaseURL(); got != c.want {
			t.Errorf("version=%q: got %q, want %q", c.ver, got, c.want)
		}
	}
}

// Dev / local builds have no tagged release and no embedded SHAs, so
// they fall back to /latest/download — `make build` keeps working
// without a release pipeline.
func TestReleaseAssetBaseURL_DevFallsBackToLatest(t *testing.T) {
	prev := version
	defer func() { version = prev }()

	for _, v := range []string{"dev", ""} {
		version = v
		want := releaseRepoURL + "/latest/download"
		if got := releaseAssetBaseURL(); got != want {
			t.Errorf("version=%q: got %q, want %q", v, got, want)
		}
	}
}

func TestInstallSource(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/opt/homebrew/Cellar/dew/0.8.2/bin/dew", "Homebrew"},
		{"/usr/local/Homebrew/bin/dew", "Homebrew"}, // Intel, capital H
		{"/usr/local/Cellar/dew/0.8.2/bin/dew", "Homebrew"},
		{"/Users/x/.npm/_npx/abc123/node_modules/.bin/dew", "npx/npm"},
		{"/Users/x/.cache/npm/dew", "npx/npm"},
		{"/usr/local/bin/dew", ""},
	}
	for _, c := range cases {
		if got := installSource(c.path); got != c.want {
			t.Errorf("installSource(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// A SHA mismatch must produce an actionable error: it names the
// running binary's version and tells the user to upgrade (newer
// releases pin their own immutable assets). Without this the reporter
// couldn't even tell which of two installed dews was failing.
func TestFetchAsset_SHAMismatchErrorIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("wrong bytes"))
	}))
	defer srv.Close()

	prevVer := version
	defer func() { version = prevVer }()
	version = "0.8.1"

	dataDir := t.TempDir()
	res := fetchAsset(srv.URL+"/x", filepath.Join(dataDir, "k"), "kernel", "minimal",
		"0000000000000000000000000000000000000000000000000000000000000000")
	if res.err == nil {
		t.Fatal("expected error")
	}
	msg := res.err.Error()
	for _, want := range []string{"SHA mismatch", "0.8.1", "upgrade"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}

// A non-404 HTTP status (rate-limit, server error, auth) must not be
// reported as "Asset not found" / "may have been removed" — that
// misdirects the user to upgrade when the real fix is to retry.
func TestFetchAsset_Non404StatusIsNotReportedAsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", 429)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	res := fetchAsset(srv.URL+"/x", filepath.Join(dataDir, "k"), "kernel", "minimal", "")
	if res.err == nil {
		t.Fatal("expected error on HTTP 429")
	}
	msg := res.err.Error()
	if !strings.Contains(msg, "429") {
		t.Errorf("error should name the status 429:\n%s", msg)
	}
	if strings.Contains(msg, "not found") || strings.Contains(msg, "removed") {
		t.Errorf("429 must not be described as missing/removed:\n%s", msg)
	}
}

// 404 keeps the "may have been removed" + upgrade hint — that IS the
// pinned-asset-gone case.
func TestFetchAsset_404ReportsMissingWithUpgradeHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 404)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	res := fetchAsset(srv.URL+"/x", filepath.Join(dataDir, "k"), "kernel", "minimal", "")
	if res.err == nil {
		t.Fatal("expected error on HTTP 404")
	}
	msg := res.err.Error()
	if !strings.Contains(msg, "404") || !strings.Contains(msg, "removed") {
		t.Errorf("404 should mention removal:\n%s", msg)
	}
}

// End-to-end: a release-build binary (version set) must hit the
// versioned tag path on the server, NOT /latest/download. This is the
// regression guard for the brick bug — it asserts the request URL the
// download layer actually produces.
func TestDownloadAssets_RequestsVersionedTagPath(t *testing.T) {
	// downloadAssets fetches kernel + initramfs concurrently, so the
	// handler runs on multiple goroutines — guard the shared slice.
	var mu sync.Mutex
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	prevOverride := releaseBaseURLOverride
	defer func() { releaseBaseURLOverride = prevOverride }()
	prevVer := version
	defer func() { version = prevVer }()
	version = "0.8.2"

	// Build the override the same way production composes base: the test
	// server stands in for github.com, and the path must carry the
	// version tag. We compute it from releaseAssetBaseURL by swapping
	// only the host so the path assertion is meaningful.
	base := releaseAssetBaseURL()
	suffix := strings.TrimPrefix(base, releaseRepoURL) // "/download/v0.8.2"
	releaseBaseURLOverride = srv.URL + "/releases" + suffix

	dataDir := t.TempDir()
	kernel := filepath.Join(dataDir, "vmlinuz")
	initrd := filepath.Join(dataDir, "initramfs-minimal.cpio.gz")
	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err != nil {
		t.Fatalf("downloadAssets: %v", err)
	}

	for _, p := range gotPaths {
		if !strings.Contains(p, "/download/v0.8.2/") {
			t.Errorf("request path %q does not carry the version tag /download/v0.8.2/", p)
		}
		if strings.Contains(p, "/latest/") {
			t.Errorf("request path %q still aims at /latest — pin defeated", p)
		}
	}
	if len(gotPaths) == 0 {
		t.Fatal("no requests reached the server")
	}
}
