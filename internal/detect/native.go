package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Known npm packages that ship a `binding.gyp` and frequently fall
// back to from-source compilation when no prebuilt is available for
// linux-musl. The minimum guest toolchain we need to satisfy them is
// build-base (gcc/g++/make/musl-dev) + python3 (for node-gyp). Keep
// this list tight — false positives just mean an extra ~30 s of apk
// install for a project that didn't strictly need it; false negatives
// fall back to the stderr-scan retry path which is more expensive
// (a failed npm install before the build tools land).
var knownNativeNodePackages = []string{
	"bcrypt",
	"better-sqlite3",
	"canvas",
	"node-gyp",
	"node-pty",
	"node-sass",
	"sass-embedded",
	"sqlite3",
	"sharp",
	"usb",
}

// gypErrorPattern matches the strings that consistently appear in the
// stderr of an npm install that failed because the toolchain wasn't
// present. Used by the reactive-fallback path in cmdUp.
var gypErrorPattern = regexp.MustCompile(
	`(?i)gyp ERR!|node-gyp rebuild|g\+\+: not found|cc1: error:|python(?:3)?: (?:command )?not found|prebuild-install (?:warn|err)|make: \*\*\* .*Error`,
)

// NeedsNativeBuildTools reports whether the project's package-lock.json
// or package.json references a package known to compile native code at
// install time. Returns the matched package names for use in user-
// facing messages.
//
// Best-effort: we only catch what's named in the lock; if a project
// pulls in a native dep transitively under an alias we won't see it.
// The reactive retry path (ScanInstallStderrForNativeBuild) covers
// what we miss here.
func NeedsNativeBuildTools(dir string) (needs bool, matched []string) {
	seen := map[string]bool{}
	scanLockfile(dir, seen)
	scanPackageJSON(dir, seen)
	for _, name := range knownNativeNodePackages {
		if seen[name] {
			matched = append(matched, name)
		}
	}
	return len(matched) > 0, matched
}

// ScanInstallStderrForNativeBuild reports whether the captured stderr
// looks like a missing-build-toolchain failure rather than e.g. a
// network error or a peer-dep conflict.
func ScanInstallStderrForNativeBuild(stderr string) bool {
	return gypErrorPattern.MatchString(stderr)
}

// scanLockfile reads package-lock.json and notes any top-level package
// name from the lockfile whose name appears in knownNativeNodePackages.
// Only the lockfile is consulted (not transitive metadata) since the
// goal is to catch deps the user has actually pinned; the reactive
// retry catches the rest.
func scanLockfile(dir string, seen map[string]bool) {
	data, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		return
	}
	// We only need package names, so a partial schema is enough:
	//   { "packages": { "node_modules/sharp": {...}, "node_modules/foo/node_modules/bcrypt": {...} } }
	var lock struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return
	}
	for path := range lock.Packages {
		if path == "" {
			continue
		}
		// path is like "node_modules/foo" or "node_modules/foo/node_modules/bar".
		// Take the segment after the final "node_modules/".
		idx := strings.LastIndex(path, "node_modules/")
		if idx < 0 {
			continue
		}
		name := path[idx+len("node_modules/"):]
		if name != "" {
			seen[name] = true
		}
	}
}

// scanPackageJSON covers the case where a project has dependencies
// declared but no lockfile yet (e.g. immediately after `npm create
// vite@latest` before `npm install` runs).
func scanPackageJSON(dir string, seen map[string]bool) {
	p := readPkgJSON(dir)
	for name := range p.Dependencies {
		seen[name] = true
	}
	for name := range p.DevDependencies {
		seen[name] = true
	}
}
