// Package selfupdate checks for new versions and replaces the binary.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	repo         = "solcreek/dew"
	apiURL       = "https://api.github.com/repos/" + repo + "/releases/latest"
	cacheFile    = "update-check.json"
	checkInterval = 24 * time.Hour
)

type updateCache struct {
	LastCheck string `json:"last_check"`
	Latest    string `json:"latest"`
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func ConfigDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "dew")
	os.MkdirAll(dir, 0700)
	return dir
}

// CheckBackground runs a non-blocking update check. Prints a notice
// to stderr if a newer version is available. Call from main() as:
//
//	go selfupdate.CheckBackground(currentVersion)
func CheckBackground(currentVersion string) {
	latest, err := cachedLatest()
	if err != nil {
		return
	}
	if latest != "" && latest != "v"+currentVersion && latest > "v"+currentVersion {
		fmt.Fprintf(os.Stderr, "\n  Update available: %s → v%s\n", latest, currentVersion)
		fmt.Fprintf(os.Stderr, "  Run: dew update\n\n")
	}
}

func cachedLatest() (string, error) {
	path := filepath.Join(ConfigDir(), cacheFile)

	var cache updateCache
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &cache)
		if t, err := time.Parse(time.RFC3339, cache.LastCheck); err == nil {
			if time.Since(t) < checkInterval {
				return cache.Latest, nil
			}
		}
	}

	// Fetch latest
	latest, err := fetchLatest()
	if err != nil {
		return "", err
	}

	cache = updateCache{
		LastCheck: time.Now().UTC().Format(time.RFC3339),
		Latest:   latest,
	}
	data, _ := json.Marshal(cache)
	os.WriteFile(path, data, 0600)

	return latest, nil
}

func fetchLatest() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API: %d", resp.StatusCode)
	}

	var rel releaseInfo
	json.NewDecoder(resp.Body).Decode(&rel)
	return rel.TagName, nil
}

// Update downloads the latest binary and replaces the current one.
func Update(currentVersion string) error {
	latest, err := fetchLatest()
	if err != nil {
		return fmt.Errorf("check latest: %w", err)
	}

	if latest == "" || latest == "v"+currentVersion {
		fmt.Fprintf(os.Stderr, "  Already up to date (v%s)\n", currentVersion)
		return nil
	}

	fmt.Fprintf(os.Stderr, "  Updating v%s → %s\n", currentVersion, latest)

	asset := binaryAsset()
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, latest, asset)

	fmt.Fprintf(os.Stderr, "  Downloading %s...\n", asset)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Write to temp file
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find self: %w", err)
	}
	self, _ = filepath.EvalSymlinks(self)

	tmp := self + ".new"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	f.Close()
	os.Chmod(tmp, 0755)

	// macOS: codesign
	if runtime.GOOS == "darwin" {
		exec.Command("codesign", "--force", "-s", "-", tmp).Run()
	}

	// Atomic replace
	old := self + ".old"
	os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup old: %w", err)
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Rename(old, self) // rollback
		return fmt.Errorf("replace: %w", err)
	}
	os.Remove(old)

	// Clear cache
	os.Remove(filepath.Join(ConfigDir(), cacheFile))

	fmt.Fprintf(os.Stderr, "  ✓ Updated to %s\n", latest)
	return nil
}

func binaryAsset() string {
	arch := runtime.GOARCH
	switch runtime.GOOS {
	case "darwin":
		if arch == "amd64" { arch = "amd64" }
		return "dew-darwin-" + arch
	case "linux":
		return "dew-linux-" + arch
	case "windows":
		return "dew-windows-x86_64.exe"
	default:
		return "dew-" + runtime.GOOS + "-" + arch
	}
}

// LatestVersion returns the latest release tag without caching.
func LatestVersion() (string, error) {
	v, err := fetchLatest()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(v, "v"), nil
}
