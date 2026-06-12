// Package ocistage pulls OCI images on the macOS host (go-containerregistry),
// flattens them into a single rootfs tar, and writes an OCI runtime config.json
// into a stage directory that dew shares into the guest VM over virtiofs. The
// guest's dew-oci-run launcher then assembles an overlay rootfs and runs the
// container with crun — no containerd, nerdctl, runc daemon, or CNI.
//
// A content-addressed cache (keyed on the platform-specific manifest digest)
// makes repeat stages near-instant, and is fully separate from any Docker image
// store on the host.
package ocistage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Options configure a Stage call. StageDir is required; the rest default
// sensibly.
type Options struct {
	Platform  string   // "linux/arm64"; defaults to the host (= guest) arch
	CacheRoot string   // defaults to DefaultCacheDir()
	StageDir  string   // required: where rootfs.tar + config.json are written
	Name      string   // container id, used in the guest overlay path; default "dew"
	Cmd       []string // override image entrypoint+cmd when non-empty
	Env       []string // extra env appended to the image env
	Data      *Bind    // optional persistent bind mount (guest ext4 -> container)
	NoCache   bool     // bypass the content-addressed + ref caches
}

// Bundle is the result of staging: a directory holding rootfs.tar and
// config.json, ready to share into the guest.
type Bundle struct {
	Dir         string
	Name        string
	Digest      string
	Cached      bool
	RootfsBytes int64
}

// GuestRootPath is where the guest dew-oci-run launcher mounts the overlay
// merged rootfs for a container of the given name. The host writes this into
// config.json's root.path so the spec and the launcher agree.
func GuestRootPath(name string) string {
	return "/var/lib/dew/oci/" + name + "/merged"
}

// Stage pulls ref (using the cache), flattens its rootfs, and writes
// rootfs.tar + config.json into opts.StageDir. It does not download or stage
// crun or the launcher — those are baked into the initramfs.
func Stage(ctx context.Context, ref string, opts Options) (*Bundle, error) {
	if opts.StageDir == "" {
		return nil, fmt.Errorf("ocistage: StageDir is required")
	}
	if opts.Platform == "" {
		opts.Platform = "linux/" + runtime.GOARCH
	}
	if opts.CacheRoot == "" {
		opts.CacheRoot = DefaultCacheDir()
	}
	if opts.Name == "" {
		opts.Name = "dew"
	}
	if err := os.MkdirAll(opts.StageDir, 0o755); err != nil {
		return nil, err
	}

	plat, err := v1.ParsePlatform(opts.Platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", opts.Platform, err)
	}

	digest, derr := resolveDigest(ctx, ref, plat, opts.CacheRoot, opts.NoCache)
	useCache := !opts.NoCache && derr == nil
	if derr != nil {
		// Without a digest we cannot key the cache; fall back to a direct pull.
		fmt.Fprintf(os.Stderr, "dew: digest resolve failed (%v); cache bypassed\n", derr)
	}

	var itemDir string
	if useCache {
		itemDir = filepath.Join(opts.CacheRoot, plat.OS+"_"+plat.Architecture, strings.ReplaceAll(digest, ":", "-"))
	}
	cachedRootfs := filepath.Join(itemDir, "rootfs.tar")
	cachedCfg := filepath.Join(itemDir, "imgcfg.json")

	var (
		cfg      v1.Config
		n        int64
		cacheHit bool
	)

	if useCache && fileExists(cachedRootfs) && fileExists(cachedCfg) {
		cacheHit = true
		data, rerr := os.ReadFile(cachedCfg)
		if rerr != nil {
			return nil, fmt.Errorf("read cached config: %w", rerr)
		}
		if jerr := json.Unmarshal(data, &cfg); jerr != nil {
			return nil, fmt.Errorf("parse cached config: %w", jerr)
		}
		if fi, serr := os.Stat(cachedRootfs); serr == nil {
			n = fi.Size()
		}
	} else {
		img, perr := crane.Pull(ref, pullOpts(ctx, plat)...)
		if perr != nil {
			return nil, fmt.Errorf("pull %s: %w", ref, perr)
		}
		cf, cerr := img.ConfigFile()
		if cerr != nil {
			return nil, fmt.Errorf("config file: %w", cerr)
		}
		cfg = cf.Config

		dstDir := opts.StageDir
		if useCache {
			dstDir = itemDir
		}
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return nil, err
		}
		cachedRootfs = filepath.Join(dstDir, "rootfs.tar")
		if n, err = flattenTo(img, cachedRootfs); err != nil {
			return nil, fmt.Errorf("flatten rootfs: %w", err)
		}
		if useCache {
			if b, mErr := json.Marshal(cfg); mErr == nil {
				// Atomic so a concurrent reader never sees a zero-length
				// imgcfg.json (which would unmarshal to an empty config and
				// drop the image entrypoint).
				if err := writeFileAtomic(cachedCfg, b, 0o644); err != nil {
					return nil, fmt.Errorf("write cached config: %w", err)
				}
			}
		}
	}

	// Place rootfs.tar in the stage dir (hard-link from cache when possible).
	stageRootfs := filepath.Join(opts.StageDir, "rootfs.tar")
	if stageRootfs != cachedRootfs {
		if err := linkOrCopy(cachedRootfs, stageRootfs); err != nil {
			return nil, fmt.Errorf("stage rootfs: %w", err)
		}
	}

	// Write the OCI runtime spec.
	spec := ociSpec(cfg, GuestRootPath(opts.Name), opts.Cmd, opts.Env, opts.Data)
	specBytes, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(filepath.Join(opts.StageDir, "config.json"), specBytes, 0o644); err != nil {
		return nil, err
	}

	return &Bundle{
		Dir:         opts.StageDir,
		Name:        opts.Name,
		Digest:      digest,
		Cached:      cacheHit,
		RootfsBytes: n,
	}, nil
}
