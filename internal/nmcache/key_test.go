package nmcache

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectKey_Stable(t *testing.T) {
	dir := t.TempDir()
	k1 := ProjectKey(dir)
	k2 := ProjectKey(dir)
	if k1 != k2 {
		t.Fatalf("same dir should produce same key, got %q vs %q", k1, k2)
	}
	if len(k1) != 12 {
		t.Fatalf("key should be 12 chars, got %d (%q)", len(k1), k1)
	}
}

func TestProjectKey_DifferentDirs(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if ProjectKey(a) == ProjectKey(b) {
		t.Fatalf("different dirs should produce different keys: %q == %q", a, b)
	}
}

func TestProjectKey_NonexistentDirIsStillDeterministic(t *testing.T) {
	// Caller may pass a path that doesn't exist (e.g. user typed wrong
	// dir). We don't error — we hash the lexical form so behavior is
	// at least deterministic.
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	k1 := ProjectKey(dir)
	k2 := ProjectKey(dir)
	if k1 != k2 {
		t.Fatalf("nonexistent path should still hash deterministically: %q vs %q", k1, k2)
	}
}

func TestProjectKey_ResolveSymlinks(t *testing.T) {
	target := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	keyTarget := ProjectKey(target)
	keyLink := ProjectKey(link)
	if keyTarget != keyLink {
		t.Fatalf("symlinked path should hash to the same key as its target: %q (%q) vs %q (%q)",
			target, keyTarget, link, keyLink)
	}
}

func TestProjectKey_CaseInsensitiveOnDarwinWindows(t *testing.T) {
	// We don't actually need to be on darwin/windows to test the
	// lowercase behavior — the function inspects runtime.GOOS itself.
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("case folding only applies on darwin/windows hosts")
	}
	// Use a hard-coded path so we don't depend on filesystem semantics.
	// We test the lowercase normalization invariant: any uppercase
	// variant of a path collapses to the same key.
	lower := "/users/test/project-foo"
	upper := "/Users/Test/Project-Foo"
	if ProjectKey(lower) != ProjectKey(upper) {
		t.Fatalf("case-insensitive normalization broke: %q != %q",
			ProjectKey(lower), ProjectKey(upper))
	}
}

func TestProjectKey_IsHex(t *testing.T) {
	k := ProjectKey(t.TempDir())
	for _, c := range k {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("key contains non-hex char %q in %q", c, k)
		}
	}
}

func TestProjectKey_SafeForShellExpansion(t *testing.T) {
	// The key gets inlined into a shell command via fmt.Sprintf.
	// Guard against any future hash function returning something
	// shell-unsafe.
	k := ProjectKey(t.TempDir())
	for _, bad := range []string{"$", "`", "\\", "'", "\"", " ", ";"} {
		if strings.Contains(k, bad) {
			t.Fatalf("key %q contains shell-unsafe char %q", k, bad)
		}
	}
}
