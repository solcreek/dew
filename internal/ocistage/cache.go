package ocistage

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// refTTL bounds how long a resolved tag→digest mapping is trusted before we
// re-check the registry. Pulling by tag is mutable; a short TTL keeps warm
// stages fast (no manifest HEAD) without pinning a stale digest for long.
const refTTL = time.Hour

// DefaultCacheDir is dew's own content-addressed OCI cache root
// (~/Library/Caches/dew/oci on macOS). It is fully separate from any Docker
// image store on the host.
func DefaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "dew", "oci")
	}
	return filepath.Join(d, "dew", "oci")
}

// fallbackKeychain uses the host's docker credentials when they resolve, and
// silently falls back to anonymous otherwise. This supports private-registry
// logins (docker login / macOS keychain) while staying robust against a stale
// credsStore helper that isn't installed (e.g. leftover Docker Desktop config).
type fallbackKeychain struct{}

func (fallbackKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	a, err := authn.DefaultKeychain.Resolve(r)
	if err != nil {
		return authn.Anonymous, nil
	}
	return a, nil
}

func pullOpts(ctx context.Context, plat *v1.Platform) []crane.Option {
	return []crane.Option{
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(fallbackKeychain{}),
		crane.WithPlatform(plat),
	}
}

type refRecord struct {
	Digest string `json:"digest"`
	Unix   int64  `json:"unix"`
}

// resolveDigest returns the content address used as the cache key. An image
// pinned by @sha256: needs no network; a tag is resolved with one manifest
// request (platform-specific), memoized in <cacheRoot>/refs with a short TTL.
func resolveDigest(ctx context.Context, ref string, plat *v1.Platform, cacheRoot string, noCache bool) (string, error) {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:], nil
	}
	refsDir := filepath.Join(cacheRoot, "refs")
	cachePath := filepath.Join(refsDir, sanitize(plat.String()+"_"+ref)+".json")

	if !noCache {
		if data, err := os.ReadFile(cachePath); err == nil {
			var rec refRecord
			if json.Unmarshal(data, &rec) == nil && rec.Digest != "" &&
				time.Since(time.Unix(rec.Unix, 0)) < refTTL {
				return rec.Digest, nil
			}
		}
	}

	d, err := crane.Digest(ref, pullOpts(ctx, plat)...)
	if err != nil {
		return "", err
	}
	if !noCache {
		_ = os.MkdirAll(refsDir, 0o755)
		if b, mErr := json.Marshal(refRecord{Digest: d, Unix: time.Now().Unix()}); mErr == nil {
			_ = os.WriteFile(cachePath, b, 0o644)
		}
	}
	return d, nil
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

// sanitize makes a registry ref safe as a filename.
func sanitize(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_").Replace(s)
}
