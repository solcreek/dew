//go:build darwin

package main

import (
	"os"
	"testing"
	"time"
)

func TestBuildSkipSet(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/.gitignore", []byte("dist\n# comment\nbuild/\n"), 0644)

	skip := buildSkipSet(dir)

	expected := []string{".git", "node_modules", "__pycache__", ".venv", ".env", "dist", "build"}
	for _, name := range expected {
		if !skip[name] {
			t.Errorf("should skip %q", name)
		}
	}
}

func TestShouldSkip(t *testing.T) {
	skip := map[string]bool{
		".git":         true,
		"node_modules": true,
		".env":         true,
	}

	tests := []struct {
		rel  string
		want bool
	}{
		{".", false},
		{".git", true},
		{"node_modules", true},
		{".env", true},
		{"src/App.tsx", false},
		{"server.ts", false},
		{"app.tar.gz", true},
		{"data.db", true},
		{"notes.db-wal", true},
		{"notes.db-shm", true},
	}

	for _, tt := range tests {
		info := fakeFileInfo{name: tt.rel, isDir: false}
		got := shouldSkip(tt.rel, info, skip)
		if got != tt.want {
			t.Errorf("shouldSkip(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500B"},
		{1024, "1KB"},
		{77000, "75KB"},
		{1048576, "1.0MB"},
		{5242880, "5.0MB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestDetectGitVersion(t *testing.T) {
	v := detectGitVersion(t.TempDir())
	if v != "unknown" {
		t.Errorf("non-git dir should return 'unknown', got %q", v)
	}
}

type fakeFileInfo struct {
	name  string
	isDir bool
}

func (f fakeFileInfo) Name() string         { return f.name }
func (f fakeFileInfo) Size() int64          { return 0 }
func (f fakeFileInfo) Mode() os.FileMode    { return 0644 }
func (f fakeFileInfo) ModTime() time.Time   { return time.Time{} }
func (f fakeFileInfo) IsDir() bool          { return f.isDir }
func (f fakeFileInfo) Sys() any             { return nil }
