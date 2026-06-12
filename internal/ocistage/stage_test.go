package ocistage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TestStage_CacheHitOffline drives Stage end-to-end with no network: a
// digest-pinned ref (so resolveDigest needs no manifest request) plus a
// pre-seeded content-addressed cache entry.
func TestStage_CacheHitOffline(t *testing.T) {
	cacheRoot := t.TempDir()
	stageDir := t.TempDir()

	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	ref := "docker.io/library/redis@" + digest

	// Seed the cache as Stage would have on a miss.
	itemDir := filepath.Join(cacheRoot, "linux_arm64", "sha256-0000000000000000000000000000000000000000000000000000000000000001")
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(itemDir, "rootfs.tar"), []byte("ROOTFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := v1.Config{Entrypoint: []string{"redis-server"}, Env: []string{"PATH=/usr/bin"}}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(itemDir, "imgcfg.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := Stage(context.Background(), ref, Options{
		Platform:  "linux/arm64",
		CacheRoot: cacheRoot,
		StageDir:  stageDir,
		Name:      "redis",
		Cmd:       []string{"redis-server", "--version"},
		Data:      &Bind{Source: "/var/lib/dew/services/redis/data", Destination: "/data"},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !b.Cached {
		t.Fatal("expected cache hit")
	}
	if b.Digest != digest {
		t.Fatalf("digest = %q, want %q", b.Digest, digest)
	}

	// rootfs.tar staged from cache.
	if got, _ := os.ReadFile(filepath.Join(stageDir, "rootfs.tar")); string(got) != "ROOTFS" {
		t.Fatalf("staged rootfs = %q", got)
	}

	// config.json reflects the override + bind + cached image config.
	data, err := os.ReadFile(filepath.Join(stageDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	args := spec["process"].(map[string]any)["args"].([]any)
	if len(args) != 2 || args[0] != "redis-server" || args[1] != "--version" {
		t.Fatalf("config args = %v", args)
	}
	root := spec["root"].(map[string]any)["path"]
	if root != "/var/lib/dew/oci/redis/merged" {
		t.Fatalf("root.path = %v", root)
	}
}

// TestStage_Integration exercises the real pull+flatten+cache path against
// Docker Hub. Skipped under -short (and offline CI).
func TestStage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("network integration test; skipped under -short")
	}
	cacheRoot := t.TempDir()
	stageDir := t.TempDir()
	b, err := Stage(context.Background(), "docker.io/library/alpine:3.21", Options{
		Platform:  "linux/arm64",
		CacheRoot: cacheRoot,
		StageDir:  stageDir,
		Name:      "alpine",
		Cmd:       []string{"cat", "/etc/os-release"},
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if b.RootfsBytes == 0 {
		t.Fatal("expected non-empty rootfs")
	}
	if !fileExists(filepath.Join(stageDir, "rootfs.tar")) || !fileExists(filepath.Join(stageDir, "config.json")) {
		t.Fatal("stage dir missing rootfs.tar/config.json")
	}
	// Second stage should hit the cache.
	b2, err := Stage(context.Background(), "docker.io/library/alpine:3.21", Options{
		Platform: "linux/arm64", CacheRoot: cacheRoot, StageDir: t.TempDir(), Name: "alpine",
	})
	if err != nil {
		t.Fatalf("Stage(warm): %v", err)
	}
	if !b2.Cached {
		t.Fatal("second stage should be a cache hit")
	}
}

func TestResolveDigest_PinnedNeedsNoNetwork(t *testing.T) {
	const digest = "sha256:abc123"
	got, err := resolveDigest(context.Background(), "repo@"+digest, &v1.Platform{OS: "linux", Architecture: "arm64"}, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got != digest {
		t.Fatalf("got %q, want %q", got, digest)
	}
}

func TestRefCacheRoundTrip(t *testing.T) {
	cacheRoot := t.TempDir()
	plat := &v1.Platform{OS: "linux", Architecture: "arm64"}
	refsDir := filepath.Join(cacheRoot, "refs")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a fresh ref record; resolveDigest should return it without network.
	rec := refRecord{Digest: "sha256:cached", Unix: time.Now().Unix()}
	b, _ := json.Marshal(rec)
	path := filepath.Join(refsDir, sanitize(plat.String()+"_docker.io/library/redis:7")+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDigest(context.Background(), "docker.io/library/redis:7", plat, cacheRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:cached" {
		t.Fatalf("got %q, want sha256:cached (from ref cache)", got)
	}
}
