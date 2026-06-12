package ocistage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	cachePath := filepath.Join(refsDir, refCacheFile(plat, ref))

	var rec refRecord
	haveRec := false
	if !noCache {
		if data, err := os.ReadFile(cachePath); err == nil {
			if json.Unmarshal(data, &rec) == nil && rec.Digest != "" {
				haveRec = true
				if time.Since(time.Unix(rec.Unix, 0)) < refTTL {
					return rec.Digest, nil
				}
			}
		}
	}

	d, err := crane.Digest(ref, pullOpts(ctx, plat)...)
	if err != nil {
		// Digest refresh failed (offline, auth, 404, rate-limit, …). If we have
		// a previously resolved digest — even past its TTL — reuse it so an
		// already-cached rootfs can still be staged, instead of bypassing the
		// cache and forcing a pull that will also fail. Log the actual error so
		// the cause is debuggable rather than asserting "unreachable".
		if haveRec {
			fmt.Fprintf(os.Stderr, "dew: digest refresh for %s failed (%v); using cached digest\n", ref, err)
			return rec.Digest, nil
		}
		return "", err
	}
	if !noCache {
		_ = os.MkdirAll(refsDir, 0o755)
		if b, mErr := json.Marshal(refRecord{Digest: d, Unix: time.Now().Unix()}); mErr == nil {
			_ = writeFileAtomic(cachePath, b, 0o644)
		}
	}
	return d, nil
}

// flattenTo writes the image's flattened (whiteout-applied) rootfs to path
// atomically: it streams to a sibling temp file and renames into place. Two
// concurrent cold stages of the same image (or an interrupted one) therefore
// never leave a partial rootfs.tar that a later run mistakes for a cache hit.
func flattenTo(img v1.Image, path string) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rootfs-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	rc := mutate.Extract(img)
	n, err := io.Copy(tmp, rc)
	rc.Close()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if rerr := os.Rename(tmpName, path); rerr != nil {
			err = rerr
		}
	}
	if err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	return n, nil
}

// writeFileAtomic writes data to path via a sibling temp file + rename, so a
// reader never observes a torn or zero-length file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmpName, perm)
	}
	if werr == nil {
		werr = os.Rename(tmpName, path)
	}
	if werr != nil {
		os.Remove(tmpName)
		return werr
	}
	return nil
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

// refCacheFile returns a collision-resistant filename for the tag→digest record
// of (platform, ref). A plain character substitution is lossy — e.g.
// "a/b_c:tag" and "a_b/c:tag" would map to the same name and cross-contaminate
// each other's digest — so key on a stable hash of the exact platform+ref.
func refCacheFile(plat *v1.Platform, ref string) string {
	sum := sha256.Sum256([]byte(plat.String() + "\x00" + ref))
	return hex.EncodeToString(sum[:]) + ".json"
}
