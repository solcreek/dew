// Package nmcache implements the per-project node_modules cache.
//
// The cache lives on the existing persistent node-profile disk inside the
// VM, at /var/cache/dew/nm/{key}/node_modules. The host computes {key}
// from the absolute project path, picks a "want" stamp from the
// lockfile, and bind-mounts that directory into /app/node_modules.
// On cache hit the package-manager install step is skipped; on miss
// the install runs and the stamp is committed atomically.
//
// The contract is documented in cache.sh, which is the executable
// part of the design; key.go and stamp.go are the host-side inputs.
package nmcache

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
)

// ProjectKey returns a stable 12-char hex identifier for a project
// directory. Used as the cache subdirectory name inside the VM.
//
// Symlinks are resolved so that ~/work/proj and a symlink to it
// produce the same key. On case-insensitive host filesystems
// (macOS APFS default, NTFS) the path is lower-cased so /Users/Foo
// and /Users/foo collapse to one cache entry.
func ProjectKey(dir string) string {
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// Project may not exist yet; fall back to lexical Abs so the
		// key is still deterministic.
		abs, err = filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
	}
	abs = filepath.ToSlash(filepath.Clean(abs))
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	h := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(h[:])[:12]
}
