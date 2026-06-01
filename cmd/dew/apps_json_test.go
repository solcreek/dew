//go:build darwin

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `dew apps --json` used to silently ignore the flag and print
// the human catalog. Now it emits a single JSON object with
// per-app manifest data — enough for an agent to filter by
// runtime, port, or image without scraping the human table.
//
// We stand up a mock registry server so the test doesn't depend
// on the live one.
func TestCmdInstallList_JSON(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry.json":
			json.NewEncoder(w).Encode(map[string][]string{"apps": {"alpha", "beta"}})
		case "/apps/alpha/manifest.json":
			json.NewEncoder(w).Encode(appManifest{
				Name: "alpha", Version: "1.0.0", Description: "an app",
				Runtime: "container", Type: "service", Port: 8080,
				DockerImage: "ghcr.io/x/alpha:1.0.0", Tags: []string{"demo"},
			})
		case "/apps/beta/manifest.json":
			json.NewEncoder(w).Encode(appManifest{
				Name: "beta", Version: "0.1.0", Description: "another app",
				Runtime: "node", Type: "service", Port: 3000,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	origBase := registryBase
	registryBase = mock.URL
	t.Cleanup(func() { registryBase = origBase })

	flagJSON = true
	t.Cleanup(func() { flagJSON = false })

	out := captureStdoutString(t, func() {
		if err := cmdInstallList(); err != nil {
			t.Fatalf("cmdInstallList: %v", err)
		}
	})

	// Output must be one JSON object — agents pipe through jq.
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("not a single JSON object:\n%s\nerr: %v", out, err)
	}

	if ok, _ := got["ok"].(bool); !ok {
		t.Errorf("ok field missing or false:\n%s", out)
	}
	data, _ := got["data"].(map[string]any)
	apps, _ := data["apps"].([]any)
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %d", len(apps))
	}

	// Each entry must carry the fields the agent report specifically
	// asked for: name, version, port, runtime, image.
	first, _ := apps[0].(map[string]any)
	for _, key := range []string{"name", "version", "port", "runtime"} {
		if _, ok := first[key]; !ok {
			t.Errorf("app entry missing %q field:\n%+v", key, first)
		}
	}
}

// An entry whose manifest fetch fails must surface as an Error
// field, not poison the whole response. Agents see partial data
// + which apps failed, not an opaque 500.
func TestCmdInstallList_JSON_ManifestErrorIsPerEntry(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry.json":
			json.NewEncoder(w).Encode(map[string][]string{"apps": {"works", "broken"}})
		case "/apps/works/manifest.json":
			json.NewEncoder(w).Encode(appManifest{Name: "works", Version: "1"})
		case "/apps/broken/manifest.json":
			http.Error(w, "boom", 500)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	origBase := registryBase
	registryBase = mock.URL
	t.Cleanup(func() { registryBase = origBase })

	flagJSON = true
	t.Cleanup(func() { flagJSON = false })

	out := captureStdoutString(t, func() {
		_ = cmdInstallList()
	})
	var resp map[string]any
	json.Unmarshal([]byte(strings.TrimSpace(out)), &resp)
	data := resp["data"].(map[string]any)
	apps := data["apps"].([]any)
	if len(apps) != 2 {
		t.Fatalf("expected partial response: 2 entries, got %d", len(apps))
	}
}
