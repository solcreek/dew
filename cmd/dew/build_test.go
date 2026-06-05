//go:build darwin

package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/solcreek/dew/internal/detect"
)

// Regression: symlinks must round-trip with a non-empty Linkname so
// GNU tar can recreate them. The previous tar.FileInfoHeader(info, "")
// call shipped tarballs that BSD tar (mac) silently downgraded to a
// 0-byte regular file at extract time while GNU tar (every Linux
// deploy target) fatally rejected with "Cannot create symlink to ''".
// The bug surfaced as an opaque "exit status 2" in the dew client
// because the actual tar stderr only made it to the server's journal.
//
// We exercise createTarball directly and re-read the result with Go's
// archive/tar package — strict, not BSD-tolerant — so a future
// regression can't escape macOS dev again.
func TestCreateTarball_SymlinkLinknamePreserved(t *testing.T) {
	src := t.TempDir()

	// Plant a regular file + a symlink pointing at it. Mirrors the
	// field repro (CLAUDE.md -> AGENTS.md in a project root).
	target := filepath.Join(src, "AGENTS.md")
	if err := os.WriteFile(target, []byte("agents body"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "CLAUDE.md")
	if err := os.Symlink("AGENTS.md", link); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "build.tar.gz")
	manifest := &buildManifest{App: "test", Type: "static"}
	proj := &detect.Project{}
	if _, _, err := createTarball(src, "", out, manifest, proj); err != nil {
		t.Fatalf("createTarball: %v", err)
	}

	// Strict read-back via Go's archive/tar — exactly the path GNU
	// tar on Linux walks at extract time.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	sawSymlink := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if h.Name == "app/CLAUDE.md" {
			sawSymlink = true
			if h.Typeflag != tar.TypeSymlink {
				t.Errorf("CLAUDE.md typeflag = %d, want TypeSymlink (%d)", h.Typeflag, tar.TypeSymlink)
			}
			if h.Linkname != "AGENTS.md" {
				t.Errorf("CLAUDE.md Linkname = %q, want %q — this is the regression that breaks GNU tar at extract", h.Linkname, "AGENTS.md")
			}
		}
	}
	if !sawSymlink {
		t.Fatal("expected app/CLAUDE.md in tarball, never saw it")
	}
}

// type=static must ship ONLY the build output dir, not the project
// source tree. Pre-2026-06-05, createTarball walked projectDir
// unconditionally — every static deploy shipped the entire repo
// (package.json, tsconfig.json, src/, vercel.json...) alongside dist/.
// That also pulled in repo-root symlinks (CLAUDE.md → AGENTS.md) that
// have no business in a static bundle and triggered the GNU-tar
// symlink bug.
func TestCreateTarball_StaticShipsOnlyOutputDir(t *testing.T) {
	src := t.TempDir()
	// Source files we DON'T want in the tarball.
	for _, f := range []string{"package.json", "src/index.ts", "vercel.json", "CLAUDE.md"} {
		full := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("source"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Build output we DO want.
	dist := filepath.Join(src, "dist")
	if err := os.MkdirAll(dist, 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "assets/app.js"} {
		full := filepath.Join(dist, f)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("built"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "static.tar.gz")
	if _, _, err := createTarball(src, "dist", out, &buildManifest{App: "site", Type: "static"}, &detect.Project{}); err != nil {
		t.Fatalf("createTarball: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	defer gz.Close()
	tr := tar.NewReader(gz)

	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		seen[h.Name] = true
	}

	// Wanted: dist contents, rooted at app/.
	for _, want := range []string{"app/index.html", "app/assets/app.js"} {
		if !seen[want] {
			t.Errorf("missing %q in tarball", want)
		}
	}
	// Not wanted: source tree.
	for _, leak := range []string{"app/package.json", "app/src/index.ts", "app/vercel.json", "app/CLAUDE.md"} {
		if seen[leak] {
			t.Errorf("source leaked into static tarball: %q", leak)
		}
	}
}

// publishCanonicalTarball must leave a usable .dew/build.tar.gz
// pointer at the project root regardless of where the primary output
// is written. The file is what a follow-up `dew deploy` (without
// --tarball) auto-detects, so a missing or corrupt copy here is the
// "no tarball found" / retry-after-failure friction the field report
// flagged.
func TestPublishCanonicalTarball_ReusableAcrossRetries(t *testing.T) {
	proj := t.TempDir()
	// Primary build output lives outside the project dir (matches
	// `dew build -o /tmp/foo.tar.gz`) — the canonical pointer must
	// still appear inside the project.
	out := filepath.Join(t.TempDir(), "foo.tar.gz")
	if err := os.WriteFile(out, []byte("first build"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := publishCanonicalTarball(proj, out); err != nil {
		t.Fatalf("publish: %v", err)
	}
	canonical := filepath.Join(proj, ".dew", "build.tar.gz")
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if string(got) != "first build" {
		t.Errorf("canonical bytes = %q, want %q", got, "first build")
	}

	// Second build overwrites the pointer cleanly — no leftover from
	// the first build, no permission/exists error from re-creating.
	if err := os.WriteFile(out, []byte("second build"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publishCanonicalTarball(proj, out); err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	got, err = os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read canonical 2: %v", err)
	}
	if string(got) != "second build" {
		t.Errorf("re-publish bytes = %q, want %q", got, "second build")
	}
}
