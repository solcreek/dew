//go:build darwin

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// downloadAssets must fetch vmlinuz and initramfs concurrently — sequential
// downloads add a ~3-5s tail to the very first dew run. We assert
// concurrency by parking each handler until both have arrived; a
// sequential implementation will hit the 2s deadline before the second
// one starts.
func TestDownloadAssets_Parallel(t *testing.T) {
	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	bothHere := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		if n >= 2 {
			once.Do(func() { close(bothHere) })
		}
		// Wait for both to arrive, with a deadline so a sequential
		// implementation doesn't hang the test forever.
		select {
		case <-bothHere:
		case <-time.After(2 * time.Second):
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer srv.Close()

	// Swap releaseBaseURL via the env-driven helper: we can't reassign the
	// const, so we use the path-mapping httptest server which serves any
	// URL path with the same body. downloadAssets only checks status==200.
	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	dataDir := t.TempDir()
	kernel := filepath.Join(dataDir, "vmlinuz")
	initrd := filepath.Join(dataDir, "initramfs-minimal.cpio.gz")

	start := time.Now()
	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err != nil {
		t.Fatalf("downloadAssets: %v", err)
	}
	elapsed := time.Since(start)

	if got := maxConcurrent.Load(); got < 2 {
		t.Errorf("expected both requests in flight simultaneously, max concurrent = %d", got)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("expected parallel completion well under 1.5s, took %v (sequential?)", elapsed)
	}
	for _, p := range []string{kernel, initrd} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing output: %s", p)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("empty output: %s", p)
		}
	}
}

// On failure, downloadAssets must not leave .partial files behind so a
// retry starts clean.
func TestDownloadAssets_RemovesPartialOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 503)
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	dataDir := t.TempDir()
	kernel := filepath.Join(dataDir, "vmlinuz")
	initrd := filepath.Join(dataDir, "initramfs-minimal.cpio.gz")

	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err == nil {
		t.Fatal("expected error on 503")
	}
	for _, p := range []string{kernel + ".partial", initrd + ".partial"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("leftover partial: %s", p)
		}
	}
}

// Default (force=false): existing files at the destination paths are
// treated as a cache hit and skipped. The handler must be called zero
// times for kernel + initramfs that already exist. This is the
// invariant that makes `dew up` cheap on the hot path.
func TestDownloadAssets_SkipsExistingByDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("fresh"))
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	dataDir := t.TempDir()
	kernel := filepath.Join(dataDir, "vmlinuz")
	initrd := filepath.Join(dataDir, "initramfs-minimal.cpio.gz")
	if err := os.WriteFile(kernel, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err != nil {
		t.Fatalf("downloadAssets: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("expected zero HTTP hits with existing files, got %d", got)
	}
	if data, _ := os.ReadFile(kernel); string(data) != "stale" {
		t.Errorf("kernel was overwritten: %q", data)
	}
}

// When ExpectedAssetSHA has the entry and the served bytes match,
// the download installs cleanly at the destination path. This is the
// happy path on every release-build dew install.
func TestDownloadAssets_VerifiesAndInstallsOnMatchingSHA(t *testing.T) {
	payload := []byte(strings.Repeat("k", 4096))
	want := sha256Hex(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	prevSHA := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prevSHA }()
	ExpectedAssetSHA = map[string]string{
		kernelAssetName():              want,
		initrdAssetName("minimal"):     want,
	}

	dataDir := t.TempDir()
	kernel := assetCachePath(dataDir, kernelAssetName())
	initrd := assetCachePath(dataDir, initrdAssetName("minimal"))

	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err != nil {
		t.Fatalf("downloadAssets: %v", err)
	}
	for _, p := range []string{kernel, initrd} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("missing %s: %v", p, err)
			continue
		}
		if sha256Hex(got) != want {
			t.Errorf("on-disk SHA mismatch for %s", p)
		}
	}
}

// SHA mismatch surfaces as an error and leaves NO file at the
// destination — the .partial gets removed, no rename happens. This is
// the new safety net that would have caught the 2026-06 M4 Max bug
// before Apple VZ rejected the bytes with Code=1.
func TestDownloadAssets_RejectsAndCleansUpOnSHAMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("wrong bytes"))
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	prevSHA := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prevSHA }()
	ExpectedAssetSHA = map[string]string{
		kernelAssetName():          "0000000000000000000000000000000000000000000000000000000000000000",
		initrdAssetName("minimal"): "0000000000000000000000000000000000000000000000000000000000000000",
	}

	dataDir := t.TempDir()
	kernel := assetCachePath(dataDir, kernelAssetName())
	initrd := assetCachePath(dataDir, initrdAssetName("minimal"))

	err := downloadAssets(dataDir, "minimal", kernel, initrd, false)
	if err == nil {
		t.Fatal("expected SHA mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "SHA mismatch") {
		t.Errorf("error doesn't mention SHA mismatch: %v", err)
	}
	for _, p := range []string{kernel, initrd, kernel + ".partial", initrd + ".partial"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("leftover file after mismatch: %s", p)
		}
	}
}

// The actual regression scenario: a stale file from a previous dew
// version lives at the LEGACY un-suffixed path. The new dew binary
// (with ExpectedAssetSHA populated) resolves cfg.Kernel to the
// CONTENT-ADDRESSED path, sees that file is missing, and downloads
// fresh bytes. The legacy file is untouched — left for the user to
// reclaim disk on their own schedule. No SHA check is run against
// the legacy file because the new binary doesn't look at that path.
func TestResolveAssets_StaleLegacyPathIsBypassedOnUpgrade(t *testing.T) {
	payload := []byte(strings.Repeat("g", 8192))
	want := sha256Hex(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	prevSHA := ExpectedAssetSHA
	defer func() { ExpectedAssetSHA = prevSHA }()
	ExpectedAssetSHA = map[string]string{
		kernelAssetName():          want,
		initrdAssetName("minimal"): want,
	}

	dataDir := t.TempDir()

	// Pre-plant the 9MB-stale-EFI-stub-kernel at the LEGACY path
	// (~/.local/share/dew/vmlinuz). This is what an older dew install
	// would have left behind.
	legacyKernel := filepath.Join(dataDir, "vmlinuz")
	if err := os.WriteFile(legacyKernel, []byte("stale-bytes-from-0.7.30"), 0644); err != nil {
		t.Fatal(err)
	}

	// The new dew binary picks paths via assetCachePath; with
	// ExpectedAssetSHA set the path is content-addressed.
	kernel := assetCachePath(dataDir, kernelAssetName())
	initrd := assetCachePath(dataDir, initrdAssetName("minimal"))
	if err := downloadAssets(dataDir, "minimal", kernel, initrd, false); err != nil {
		t.Fatalf("downloadAssets: %v", err)
	}

	// New content-addressed kernel has correct bytes.
	got, _ := os.ReadFile(kernel)
	if sha256Hex(got) != want {
		t.Errorf("new kernel SHA mismatch")
	}
	// Legacy file untouched — exists, contains old bytes.
	legacyBytes, err := os.ReadFile(legacyKernel)
	if err != nil {
		t.Fatalf("legacy file disappeared: %v", err)
	}
	if string(legacyBytes) != "stale-bytes-from-0.7.30" {
		t.Errorf("legacy file got overwritten: %q", legacyBytes)
	}
}

// force=true: existing files are deleted before fetching so even a
// stale/corrupt cached file is replaced. The handler must be called
// for both assets, and the on-disk content must reflect the server's
// new bytes. This is what `dew assets pull --force` gives the user
// who's been told (by doctor or the debug dump) that their cached
// kernel is corrupt.
func TestDownloadAssets_ForceReplacesExisting(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("fresh"))
	}))
	defer srv.Close()

	prev := releaseBaseURLOverride
	releaseBaseURLOverride = srv.URL
	defer func() { releaseBaseURLOverride = prev }()

	dataDir := t.TempDir()
	kernel := filepath.Join(dataDir, "vmlinuz")
	initrd := filepath.Join(dataDir, "initramfs-minimal.cpio.gz")
	if err := os.WriteFile(kernel, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := downloadAssets(dataDir, "minimal", kernel, initrd, true); err != nil {
		t.Fatalf("downloadAssets force: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("expected 2 HTTP hits with force, got %d", got)
	}
	for _, p := range []string{kernel, initrd} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("missing after force: %s", p)
			continue
		}
		if string(data) != "fresh" {
			t.Errorf("%s = %q, want %q", filepath.Base(p), data, "fresh")
		}
	}
}
