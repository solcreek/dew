// Package selfupdate checks for new versions and replaces the binary.
package selfupdate

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	repo          = "solcreek/dew"
	defaultAPIURL = "https://api.github.com/repos/" + repo + "/releases/latest"
	cacheFile     = "update-check.json"
	checkInterval = 24 * time.Hour
)

// Overridable for testing.
var apiURL = defaultAPIURL

type updateCache struct {
	LastCheck string `json:"last_check"`
	Latest    string `json:"latest"`
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "dew")
	os.MkdirAll(dir, 0700)
	return dir
}

// CompareSemver returns:
//
//	-1 if a < b
//	 0 if a == b
//	 1 if a > b
func CompareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	ap := parseSemver(a)
	bp := parseSemver(b)
	for i := 0; i < 3; i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var parts [3]int
	segs := strings.SplitN(v, ".", 3)
	for i, s := range segs {
		if i >= 3 {
			break
		}
		// Strip pre-release suffix (e.g., "1-beta")
		s = strings.SplitN(s, "-", 2)[0]
		parts[i], _ = strconv.Atoi(s)
	}
	return parts
}

// CheckBackground runs a non-blocking update check. Prints a notice
// to stderr if a newer version is available.
func CheckBackground(currentVersion string) {
	latest, err := cachedLatest()
	if err != nil || latest == "" {
		return
	}
	if CompareSemver(latest, currentVersion) > 0 {
		fmt.Fprintf(os.Stderr, "\n  Update available: %s (current: v%s)\n", latest, currentVersion)
		fmt.Fprintf(os.Stderr, "  Run: dew update\n\n")
	}
}

func cachedLatest() (string, error) {
	path := filepath.Join(configDir(), cacheFile)

	var cache updateCache
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &cache)
		if t, err := time.Parse(time.RFC3339, cache.LastCheck); err == nil {
			if time.Since(t) < checkInterval {
				return cache.Latest, nil
			}
		}
	}

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

	if latest == "" || CompareSemver(latest, currentVersion) <= 0 {
		fmt.Fprintf(os.Stderr, "  Already up to date (v%s)\n", currentVersion)
		return nil
	}

	fmt.Fprintf(os.Stderr, "  Updating v%s → %s\n", currentVersion, latest)

	asset := BinaryAsset()
	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, latest)

	// Download checksums
	checksums, err := fetchChecksums(baseURL + "/checksums.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not verify checksum (%v)\n", err)
	}

	// Download binary
	binaryURL := baseURL + "/" + asset
	fmt.Fprintf(os.Stderr, "  Downloading %s...\n", asset)

	resp, err := http.Get(binaryURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

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

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	f.Close()

	// Verify checksum
	got := hex.EncodeToString(hash.Sum(nil))
	if expected, ok := checksums[asset]; ok {
		if got != expected {
			os.Remove(tmp)
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expected[:12], got[:12])
		}
		fmt.Fprintf(os.Stderr, "  Checksum verified ✓\n")
	}

	os.Chmod(tmp, 0755)

	// macOS: codesign with virtualization entitlement
	if runtime.GOOS == "darwin" {
		entitlements := filepath.Join(configDir(), "entitlements.plist")
		os.WriteFile(entitlements, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>com.apple.security.virtualization</key><true/>
</dict></plist>`), 0644)
		exec.Command("codesign", "--entitlements", entitlements, "--force", "-s", "-", tmp).Run()
	}

	// Atomic replace with rollback
	old := self + ".old"
	os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup old: %w", err)
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Rename(old, self)
		return fmt.Errorf("replace: %w", err)
	}
	os.Remove(old)

	// Clear cache
	os.Remove(filepath.Join(configDir(), cacheFile))

	fmt.Fprintf(os.Stderr, "  ✓ Updated to %s\n", latest)
	return nil
}

func fetchChecksums(url string) (map[string]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	checksums := make(map[string]string)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		// Format: <sha256>  <filename>
		parts := strings.Fields(scanner.Text())
		if len(parts) >= 2 {
			checksums[parts[1]] = parts[0]
		}
	}
	return checksums, nil
}

// BinaryAsset returns the expected release asset name for the current platform.
func BinaryAsset() string {
	arch := runtime.GOARCH
	switch runtime.GOOS {
	case "darwin":
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
