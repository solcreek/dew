//go:build darwin

package main

import (
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
	if err := downloadAssets(dataDir, "minimal", kernel, initrd); err != nil {
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

	if err := downloadAssets(dataDir, "minimal", kernel, initrd); err == nil {
		t.Fatal("expected error on 503")
	}
	for _, p := range []string{kernel + ".partial", initrd + ".partial"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("leftover partial: %s", p)
		}
	}
}
