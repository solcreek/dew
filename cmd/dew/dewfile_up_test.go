//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/detect"
	"github.com/solcreek/dew/internal/dewfile"
	"github.com/solcreek/dew/internal/services"
)

func TestSplitNonEmpty(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , , b ", []string{"a", "b"}},
		{"a,,", []string{"a"}},
	}
	for _, c := range cases {
		got := splitNonEmpty(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitNonEmpty(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPickProfile(t *testing.T) {
	// An explicit --profile wins over the detected/dew.toml profile.
	if got := pickProfile("standard", "node"); got != "standard" {
		t.Errorf("user profile = %q, want standard", got)
	}
	// No --profile → fall back to the resolved (detected/dew.toml) profile.
	if got := pickProfile("", "node"); got != "node" {
		t.Errorf("fallback = %q, want node", got)
	}
	if got := pickProfile("", ""); got != "" {
		t.Errorf("both empty = %q, want empty", got)
	}
}

func TestMergeNames(t *testing.T) {
	// dew.toml names lead; a --with name duplicating one is dropped (no
	// "redis, redis").
	got := mergeNames([]string{"redis", "mailpit"}, []string{"redis", "postgres"})
	want := []string{"redis", "mailpit", "postgres"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("mergeNames = %v, want %v", got, want)
	}
	// Empty inputs.
	if got := mergeNames(nil, nil); len(got) != 0 {
		t.Errorf("mergeNames(nil,nil) = %v, want empty", got)
	}
	if got := mergeNames(nil, []string{"a", "a"}); strings.Join(got, "|") != "a" {
		t.Errorf("mergeNames dedup within second = %v, want [a]", got)
	}
}

func TestApplyDewfileOverrides(t *testing.T) {
	// Non-empty dew.toml fields override detection; empty fields are left.
	proj := &detect.Project{
		Profile: "node", InstallCmd: "npm install", DevCmd: "npm run dev", Port: 5173,
	}
	df := &dewfile.File{
		Project: dewfile.Project{Profile: "standard"},
		Dev:     dewfile.Dev{Command: "pnpm dev", Port: 3000},
		// Install left empty → must inherit detection.
	}
	applyDewfileOverrides(df, proj)
	if proj.Profile != "standard" {
		t.Errorf("Profile = %q, want standard", proj.Profile)
	}
	if proj.DevCmd != "pnpm dev" {
		t.Errorf("DevCmd = %q, want pnpm dev", proj.DevCmd)
	}
	if proj.Port != 3000 {
		t.Errorf("Port = %d, want 3000", proj.Port)
	}
	if proj.InstallCmd != "npm install" {
		t.Errorf("InstallCmd = %q, want inherited npm install", proj.InstallCmd)
	}
}

func TestResolveServiceNames(t *testing.T) {
	svcs, failures := resolveServiceNames([]string{"redis", "postgres"})
	if len(svcs) != 2 || len(failures) != 0 {
		t.Fatalf("known names: svcs=%d failures=%d", len(svcs), len(failures))
	}
	// Whitespace-only entries are skipped, unknown names fail.
	svcs, failures = resolveServiceNames([]string{"redis", " ", "nope"})
	if len(svcs) != 1 {
		t.Errorf("svcs = %d, want 1", len(svcs))
	}
	if len(failures) != 1 || !strings.Contains(failures[0].reason, "unknown service") {
		t.Errorf("failures = %+v, want one unknown service", failures)
	}
}

func TestCombineServices(t *testing.T) {
	// dew.toml redis (custom image) must win over the registry redis, and a
	// new --with name (postgres) is appended.
	toml := []services.Service{{Name: "redis", Image: "redis:custom", Port: 6379}}
	svcs, failures := combineServices(toml, []string{"redis", "postgres"})
	if len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}
	if len(svcs) != 2 {
		t.Fatalf("svcs = %d, want 2 (deduped redis + postgres)", len(svcs))
	}
	if svcs[0].Name != "redis" || svcs[0].Image != "redis:custom" {
		t.Errorf("svcs[0] = %+v, want dew.toml redis (redis:custom)", svcs[0])
	}
	if svcs[1].Name != "postgres" {
		t.Errorf("svcs[1].Name = %q, want postgres", svcs[1].Name)
	}

	// Unknown --with name surfaces as a failure but doesn't drop the rest.
	svcs, failures = combineServices(nil, []string{"redis", "bogus"})
	if len(svcs) != 1 || len(failures) != 1 {
		t.Errorf("svcs=%d failures=%d, want 1/1", len(svcs), len(failures))
	}

	// dew.toml only, no --with.
	svcs, failures = combineServices(toml, nil)
	if len(svcs) != 1 || len(failures) != 0 {
		t.Errorf("toml-only: svcs=%d failures=%d, want 1/0", len(svcs), len(failures))
	}
}

func TestRunUpInit(t *testing.T) {
	// Empty dir: writes a valid, mostly-commented template that Loads clean.
	dir := t.TempDir()
	if err := runUpInit(dir); err != nil {
		t.Fatalf("runUpInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dewfile.Filename)); err != nil {
		t.Fatalf("dew.toml not written: %v", err)
	}
	if _, err := dewfile.Load(dir); err != nil {
		t.Fatalf("Load(generated): %v", err)
	}

	// Refuses to clobber an existing dew.toml.
	if err := runUpInit(dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second runUpInit err = %v, want already-exists", err)
	}
}

// A detected Node project must have its profile pinned into the starter file.
func TestRunUpInit_PinsDetectedProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runUpInit(dir); err != nil {
		t.Fatalf("runUpInit: %v", err)
	}
	f, err := dewfile.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Project.Profile != "node" {
		t.Errorf("pinned profile = %q, want node", f.Project.Profile)
	}
	if len(f.Services) != 0 {
		t.Errorf("starter has %d active services, want 0 (commented)", len(f.Services))
	}
}
