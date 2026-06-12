// Command dew-ocipoc is a proof-of-concept host-side OCI image puller for the
// "variant C" runtime: pull any OCI image on the macOS host with
// go-containerregistry, flatten its layers into a single rootfs tar, derive an
// OCI runtime config.json from the image config, and stage everything (plus a
// static crun binary and a guest run.sh) into a directory that `dew run` shares
// into the VM over virtiofs. The guest then assembles an overlay rootfs and
// runs the container with crun — no containerd, nerdctl, or runc daemon.
//
// This binary is a PoC, not production code. It exists to measure the approach
// and prove the moving parts work end-to-end.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func main() {
	var (
		stageDir = flag.String("stage", "", "directory to stage rootfs.tar, config.json, crun, run.sh into (required)")
		crunPath = flag.String("crun", "", "path to a static linux/arm64 crun binary to stage (required)")
		name     = flag.String("name", "poc", "container id")
		cmdOver  = flag.String("cmd", "", "override the image entrypoint/cmd (space-split), e.g. 'echo hi'")
		platform = flag.String("platform", "linux/arm64", "image platform os/arch to pull (guest arch)")
		cacheDir = flag.String("cache", defaultCacheDir(), "content-addressed cache root")
		noCache  = flag.Bool("no-cache", false, "bypass the cache (always pull+flatten)")
		jsonOut  = flag.Bool("json", false, "emit machine-readable timing JSON to stdout")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dew-ocipoc -stage DIR -crun PATH [-name ID] [-json] IMAGE\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *stageDir == "" || *crunPath == "" || flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	image := flag.Arg(0)

	if err := run(image, *stageDir, *crunPath, *name, *cmdOver, *platform, *cacheDir, *noCache, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "dew-ocipoc:", err)
		os.Exit(1)
	}
}

type timings struct {
	Image       string `json:"image"`
	Digest      string `json:"digest"`
	Cached      bool   `json:"cached"`
	PullMs      int64  `json:"pull_ms"`
	FlattenMs   int64  `json:"flatten_ms"`
	RootfsBytes int64  `json:"rootfs_bytes"`
}

// defaultCacheDir returns the per-user content-addressed cache root
// (~/Library/Caches/dew/oci on macOS), falling back to a temp dir.
func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "dew", "oci")
	}
	return filepath.Join(d, "dew", "oci")
}

func run(image, stageDir, crunPath, name, cmdOver, platform, cacheRoot string, noCache, jsonOut bool) error {
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return err
	}

	// Multi-arch manifest selection happens host-side. crane.Pull defaults
	// to linux/amd64, so the guest arch must be selected explicitly or the
	// rootfs binaries fail with "Exec format error" in an arm64 guest.
	plat, err := v1.ParsePlatform(platform)
	if err != nil {
		return fmt.Errorf("platform %q: %w", platform, err)
	}

	// Content-addressed cache keyed on the platform-specific manifest digest.
	// A hit skips both the registry download and the (often dominant) flatten.
	// Resolving the digest of a tag costs one cheap manifest HEAD; an image
	// pinned by @sha256: needs no network at all. The key includes os/arch so
	// arm64 and amd64 variants of the same tag never collide. The cache is
	// dew's own — fully separate from any Docker image store on the host.
	digest, derr := resolveDigest(image, plat)
	useCache := !noCache && derr == nil
	if derr != nil {
		fmt.Fprintf(os.Stderr, "dew-ocipoc: digest resolve failed (%v); cache bypassed\n", derr)
	}
	var itemDir string
	if useCache {
		itemDir = filepath.Join(cacheRoot, plat.OS+"_"+plat.Architecture, strings.ReplaceAll(digest, ":", "-"))
	}
	cachedRootfs := filepath.Join(itemDir, "rootfs.tar")
	cachedCfg := filepath.Join(itemDir, "imgcfg.json")

	var (
		cfg       v1.Config
		pullMs    int64
		flattenMs int64
		n         int64
		cacheHit  bool
	)

	if useCache && fileExists(cachedRootfs) && fileExists(cachedCfg) {
		// --- cache hit: no network, no flatten ---
		cacheHit = true
		data, err := os.ReadFile(cachedCfg)
		if err != nil {
			return fmt.Errorf("read cached config: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse cached config: %w", err)
		}
		if fi, serr := os.Stat(cachedRootfs); serr == nil {
			n = fi.Size()
		}
	} else {
		// --- cache miss (or disabled): pull + flatten ---
		// PoC: anonymous auth for public images. Production would wire the
		// macOS keychain here. The host's ~/.docker/config.json credential
		// helper is bypassed to avoid depending on a Docker Desktop install.
		t0 := time.Now()
		img, err := crane.Pull(image, crane.WithAuth(authn.Anonymous), crane.WithPlatform(plat))
		if err != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		pullMs = time.Since(t0).Milliseconds()

		cfgFile, err := img.ConfigFile()
		if err != nil {
			return fmt.Errorf("config file: %w", err)
		}
		cfg = cfgFile.Config

		// Flatten into the cache (or the stage dir if the cache is off).
		dstDir := stageDir
		if useCache {
			dstDir = itemDir
		}
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}
		cachedRootfs = filepath.Join(dstDir, "rootfs.tar")
		t1 := time.Now()
		n, err = flattenTo(img, cachedRootfs)
		if err != nil {
			return fmt.Errorf("flatten rootfs: %w", err)
		}
		flattenMs = time.Since(t1).Milliseconds()

		if useCache {
			b, _ := json.Marshal(cfg)
			if err := os.WriteFile(cachedCfg, b, 0o644); err != nil {
				return fmt.Errorf("write cached config: %w", err)
			}
		}
	}

	// --- stage: rootfs (link/copy from cache), OCI spec, crun, run.sh, archive ---
	stageRootfs := filepath.Join(stageDir, "rootfs.tar")
	if stageRootfs != cachedRootfs {
		if err := linkOrCopy(cachedRootfs, stageRootfs); err != nil {
			return fmt.Errorf("stage rootfs: %w", err)
		}
	}

	spec := ociSpec(cfg)
	if cmdOver != "" {
		spec["process"].(map[string]any)["args"] = strings.Fields(cmdOver)
	}
	specBytes, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(filepath.Join(stageDir, "config.json"), specBytes, 0o644); err != nil {
		return err
	}
	if err := copyExec(crunPath, filepath.Join(stageDir, "crun")); err != nil {
		return fmt.Errorf("stage crun: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "run.sh"), []byte(guestRunScript(name)), 0o755); err != nil {
		return err
	}

	// Also emit a docker-archive tar so the containerd/nerdctl baseline can
	// be benchmarked offline (nerdctl load -i) without guest egress. Built
	// locally as a single-layer image from the already-flattened rootfs:
	// crane.Save re-fetches remote layers and deadlocks on go-containerregistry's
	// pull limiter for multi-layer images, and this is also faster. The baseline
	// loads the same flattened content Variant C uses, so the comparison is fair.
	if err := saveArchive(stageRootfs, cfg, image, plat.OS, plat.Architecture, filepath.Join(stageDir, "image.tar")); err != nil {
		return fmt.Errorf("save docker archive: %w", err)
	}

	t := timings{Image: image, Digest: digest, Cached: cacheHit, PullMs: pullMs, FlattenMs: flattenMs, RootfsBytes: n}
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(t)
	} else {
		status := "miss"
		if cacheHit {
			status = "HIT"
		}
		fmt.Printf("staged %s -> %s\n  cache=%s pull=%dms flatten=%dms rootfs=%.1fMB\n",
			image, stageDir, status, pullMs, flattenMs, float64(n)/1e6)
	}
	return nil
}

// resolveDigest returns the content address used as the cache key. An image
// already pinned by @sha256: needs no network; a tag is resolved with one
// manifest request (platform-specific for a multi-arch index).
func resolveDigest(image string, plat *v1.Platform) (string, error) {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[i+1:], nil
	}
	return crane.Digest(image, crane.WithAuth(authn.Anonymous), crane.WithPlatform(plat))
}

// flattenTo writes the image's flattened (whiteout-applied) rootfs to path.
func flattenTo(img v1.Image, path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	rc := mutate.Extract(img)
	n, err := io.Copy(f, rc)
	rc.Close()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// linkOrCopy hard-links src to dst (instant, no extra disk) and falls back to
// a byte copy across filesystems (e.g. cache on ~, stage on /tmp).
func linkOrCopy(src, dst string) error {
	os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ociSpec builds a minimal-but-valid OCI runtime spec from the image config.
// Host networking semantics: no "network" namespace, so the container shares
// the VM's network (matches dew's existing --net=host service model). The VM
// is the isolation boundary, so we grant a broad capability set rather than
// the restrictive runc default (postgres et al. need SETUID/CHOWN to drop priv).
func ociSpec(c v1.Config) map[string]any {
	args := append([]string{}, c.Entrypoint...)
	args = append(args, c.Cmd...)
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}
	env := c.Env
	if len(env) == 0 {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	cwd := c.WorkingDir
	if cwd == "" {
		cwd = "/"
	}

	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
		"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP",
		"CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
	capSet := map[string]any{
		"bounding": caps, "effective": caps, "permitted": caps,
	}

	defaultMount := func(dst, typ, src string, opts ...string) map[string]any {
		return map[string]any{"destination": dst, "type": typ, "source": src, "options": opts}
	}

	return map[string]any{
		"ociVersion": "1.0.2-dev",
		"process": map[string]any{
			"terminal":        false,
			"user":            map[string]any{"uid": 0, "gid": 0},
			"args":            args,
			"env":             env,
			"cwd":             cwd,
			"noNewPrivileges": true,
			"capabilities":    capSet,
		},
		"root": map[string]any{
			// Absolute path to the overlay merged dir set up by run.sh.
			// On the ext4 root (after switch_root) so overlay accepts the
			// upper/work dirs — a tmpfs (/run) upperdir would be rejected.
			"path":     "/var/dew-oci/merged",
			"readonly": false,
		},
		"hostname": "dew-oci",
		"mounts": []map[string]any{
			defaultMount("/proc", "proc", "proc"),
			defaultMount("/dev", "tmpfs", "tmpfs", "nosuid", "strictatime", "mode=755", "size=65536k"),
			defaultMount("/dev/pts", "devpts", "devpts", "nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"),
			defaultMount("/dev/shm", "tmpfs", "shm", "nosuid", "noexec", "nodev", "mode=1777", "size=65536k"),
			defaultMount("/dev/mqueue", "mqueue", "mqueue", "nosuid", "noexec", "nodev"),
			defaultMount("/sys", "sysfs", "sysfs", "nosuid", "noexec", "nodev", "ro"),
		},
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				// no "network" -> share VM network (host networking)
			},
			"maskedPaths": []string{
				"/proc/kcore", "/proc/latency_stats", "/proc/timer_list",
				"/proc/timer_stats", "/proc/sched_debug", "/sys/firmware",
			},
			"readonlyPaths": []string{
				"/proc/asound", "/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
			},
		},
	}
}

// saveArchive builds a single-layer docker-archive tar from the flattened
// rootfs and the image's config, loadable by `nerdctl load -i`.
func saveArchive(rootfsPath string, cfg v1.Config, ref, osName, arch, outPath string) error {
	layer, err := tarball.LayerFromFile(rootfsPath)
	if err != nil {
		return err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return err
	}
	img, err = mutate.Config(img, cfg)
	if err != nil {
		return err
	}
	// Stamp platform into the config file, else nerdctl filters the image
	// out ("image might be filtered out") since empty.Image has no os/arch.
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	cf.OS, cf.Architecture = osName, arch
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		return err
	}
	tag, err := name.NewTag(ref)
	if err != nil {
		return err
	}
	return tarball.WriteToFile(outPath, tag, img)
}

func copyExec(src, dst string) error {
	if a, _ := filepath.Abs(src); a != "" {
		if b, _ := filepath.Abs(dst); a == b {
			return os.Chmod(dst, 0o755) // already in place
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// guestRunScript is the shell the guest runs (via `dew run -- sh /oci/run.sh`).
// It extracts the flattened rootfs onto the writable ext4 disk, stacks a tmpfs
// overlay on top, and runs the container with crun. Phase timings print to
// stdout as DEW_OCI_TIMING lines for the benchmark harness to parse.
func guestRunScript(name string) string {
	return strings.ReplaceAll(`#!/bin/sh
set -e
OCI=/oci
RUN=/var/dew-oci
# busybox date lacks %N; /proc/uptime gives centisecond resolution everywhere.
ms() { awk '{printf "%d", $1*1000}' /proc/uptime; }

rm -rf "$RUN"
mkdir -p "$RUN/lower" "$RUN/upper" "$RUN/work" "$RUN/merged" "$RUN/bundle"

T0=$(ms)
tar -xf "$OCI/rootfs.tar" -C "$RUN/lower" 2>/dev/null
T1=$(ms)
echo "DEW_OCI_TIMING extract_ms=$((T1-T0))"

mount -t overlay overlay \
  -o "lowerdir=$RUN/lower,upperdir=$RUN/upper,workdir=$RUN/work" \
  "$RUN/merged"
T2=$(ms)
echo "DEW_OCI_TIMING overlay_ms=$((T2-T1))"

cp "$OCI/config.json" "$RUN/bundle/config.json"
chmod +x "$OCI/crun" 2>/dev/null || true

T3=$(ms)
"$OCI/crun" run -b "$RUN/bundle" __NAME__
RC=$?
T4=$(ms)
echo "DEW_OCI_TIMING crun_run_ms=$((T4-T3)) exit=$RC"
exit $RC
`, "__NAME__", name)
}
