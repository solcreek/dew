//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/solcreek/dew/internal/services"
)

func catalogNames(svcs []services.Service) map[string]services.Service {
	m := make(map[string]services.Service, len(svcs))
	for _, s := range svcs {
		m[s.Name] = s
	}
	return m
}

// Without a dew.toml, the catalog is exactly the built-in registry — the
// historical static-catalog behaviour.
func TestServiceCatalog_BuiltinsOnly(t *testing.T) {
	got := serviceCatalog(t.TempDir())
	if len(got) != len(services.Registry) {
		t.Fatalf("catalog size = %d, want %d (built-ins only)", len(got), len(services.Registry))
	}
	names := catalogNames(got)
	for n := range services.Registry {
		if _, ok := names[n]; !ok {
			t.Errorf("built-in %q missing from catalog", n)
		}
	}
	// Sorted ascending.
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("catalog not sorted: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

// dew.toml [[service]] entries join the catalog; a custom image with a
// registry name overrides the built-in. This is the 0.7.52 gap: mailpit /
// anycable were running but absent from `dew services`.
func TestServiceCatalog_IncludesDewfileServices(t *testing.T) {
	dir := t.TempDir()
	toml := `
[[service]]
name = "redis"
image = "redis:custom"
port = 6390

[[service]]
name = "mailpit"
image = "axllent/mailpit:latest"
port = 8025
`
	if err := os.WriteFile(filepath.Join(dir, "dew.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	names := catalogNames(serviceCatalog(dir))

	mp, ok := names["mailpit"]
	if !ok {
		t.Fatal("custom service mailpit missing from catalog")
	}
	if mp.Image != "axllent/mailpit:latest" || mp.Port != 8025 {
		t.Errorf("mailpit = %+v, want image/port from dew.toml", mp)
	}
	// dew.toml redis overrides the built-in (custom image + port).
	if r := names["redis"]; r.Image != "redis:custom" || r.Port != 6390 {
		t.Errorf("redis not overridden by dew.toml: %+v", r)
	}
	// Built-ins not named in dew.toml still present.
	if _, ok := names["postgres"]; !ok {
		t.Error("built-in postgres dropped when dew.toml present")
	}
}
