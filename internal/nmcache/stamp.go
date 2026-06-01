package nmcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// schemaVersion is bumped when the on-disk stamp format changes in
// an incompatible way. Old CLIs see a higher version and treat the
// cache as a miss (forces rebuild, safe direction).
const schemaVersion = 1

// Stamp is the cache validity record. It is written inside the VM
// after a successful install and consulted on subsequent boots.
type Stamp struct {
	SchemaVersion int    `json:"schema_version"`
	Lockfile      string `json:"lockfile"`        // basename: package-lock.json | yarn.lock | pnpm-lock.yaml | bun.lockb | bun.lock
	LockfileHash  string `json:"lockfile_sha256"` // hex sha256 of the lockfile bytes
}

// Marshal returns the canonical JSON encoding of the stamp.
// Whitespace and field order are stable across Go versions so that
// the VM-side string compare is meaningful.
func (s Stamp) Marshal() string {
	b, _ := json.Marshal(s)
	return string(b)
}

// supportedLockfiles is the priority-ordered list of lockfile names
// dew recognizes. The first one found in the project root wins —
// matching the resolution order of the underlying package managers
// (e.g. pnpm prefers its own lockfile if multiple are present).
var supportedLockfiles = []string{
	"pnpm-lock.yaml",
	"bun.lock",
	"bun.lockb",
	"yarn.lock",
	"package-lock.json",
}

// ErrNoLockfile is returned by ComputeStamp when the project has no
// supported lockfile. The caller should disable the cache for this
// project (fall back to tmpfs node_modules) — without a lockfile we
// can't tell when dependencies change, so caching is unsafe.
var ErrNoLockfile = errors.New("no supported lockfile")

// ComputeStamp inspects projectDir for a lockfile and returns the
// stamp the cache should hold after a successful install of that
// lockfile.
func ComputeStamp(projectDir string) (Stamp, error) {
	for _, name := range supportedLockfiles {
		path := filepath.Join(projectDir, name)
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Stamp{}, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return Stamp{}, err
		}
		f.Close()
		return Stamp{
			SchemaVersion: schemaVersion,
			Lockfile:      name,
			LockfileHash:  hex.EncodeToString(h.Sum(nil)),
		}, nil
	}
	return Stamp{}, ErrNoLockfile
}
