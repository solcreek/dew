//go:build darwin

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/solcreek/dew/internal/detect"
	"github.com/solcreek/dew/internal/progress"
)

type buildManifest struct {
	App         string `json:"app"`
	Version     string `json:"version"`
	Runtime     string `json:"runtime"`
	Type        string `json:"type"`
	Entry       string `json:"entry"`
	Port        int    `json:"port"`
	Checksum    string `json:"checksum,omitempty"`
	BuiltAt     string `json:"built_at"`
	BuiltBy     string `json:"built_by"`
}

func cmdBuild(args []string) error {
	dir := "."
	var outPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "--output":
			i++
			if i < len(args) {
				outPath = args[i]
			}
		default:
			if !strings.HasPrefix(args[i], "-") {
				dir = args[i]
			}
		}
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	sp := progress.New()

	sp.Step("Detecting project")
	proj, err := detect.Detect(abs)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}
	if proj.Runtime == "" {
		sp.Fail("no project detected")
		return fmt.Errorf("no supported project detected in %s", abs)
	}

	appName := filepath.Base(abs)
	gitVersion := detectGitVersion(abs)

	if proj.BuildCmd != "" {
		sp.Step(fmt.Sprintf("Building (%s)", proj.BuildCmd))
		cmd := exec.Command("sh", "-c", proj.BuildCmd)
		cmd.Dir = abs
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			sp.Fail("build failed")
			return fmt.Errorf("build command failed: %w", err)
		}
	}

	isStatic := detectStaticSite(abs, proj)
	if isStatic {
		proj.Port = 0
	}

	sp.Step("Packaging tarball")

	appType := "server"
	if isStatic {
		appType = "static"
	}

	manifest := buildManifest{
		App:     appName,
		Version: gitVersion,
		Runtime: proj.Runtime,
		Type:    appType,
		Entry:   proj.Entry,
		Port:    proj.Port,
		BuiltAt: time.Now().UTC().Format(time.RFC3339),
		BuiltBy: "dew/" + version,
	}

	if outPath == "" {
		outPath = appName + ".tar.gz"
	}

	size, checksum, err := createTarball(abs, outPath, &manifest, proj)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}
	manifest.Checksum = checksum

	sp.Done(outPath)
	fmt.Fprintf(os.Stderr, "  %s (%s, sha256:%s)\n\n", outPath, humanSize(size), checksum[:12])

	if flagJSON {
		manifest.Checksum = checksum
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(manifest)
	}

	return nil
}

func createTarball(projectDir, outPath string, manifest *buildManifest, proj *detect.Project) (int64, string, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)
	gw := gzip.NewWriter(mw)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	skip := buildSkipSet(projectDir)

	err = filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return err
		}

		if shouldSkip(rel, info, skip) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.Join("app", rel)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		return 0, "", err
	}

	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	header := &tar.Header{
		Name:    "manifest.json",
		Size:    int64(len(manifestJSON)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(header); err != nil {
		return 0, "", err
	}
	if _, err := tw.Write(manifestJSON); err != nil {
		return 0, "", err
	}

	tw.Close()
	gw.Close()

	stat, _ := f.Stat()
	checksum := hex.EncodeToString(hash.Sum(nil))
	return stat.Size(), checksum, nil
}

var buildOutputDirs = map[string]bool{
	"dist":        true,
	"build":       true,
	".next":       true,
	".nuxt":       true,
	".astro":      true,
	".svelte-kit": true,
	".output":     true,
}

func buildSkipSet(dir string) map[string]bool {
	skip := map[string]bool{
		".git":         true,
		".dew":         true,
		".claude":      true,
		".cursor":      true,
		".agents":      true,
		"node_modules": true,
		"__pycache__":  true,
		".venv":        true,
		"venv":         true,
		".env":         true,
		".env.local":   true,
	}

	gitignore := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(gitignore)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				line = strings.TrimSuffix(line, "/")
				if buildOutputDirs[line] {
					continue
				}
				skip[line] = true
			}
		}
	}

	return skip
}

func shouldSkip(rel string, info os.FileInfo, skip map[string]bool) bool {
	if rel == "." {
		return false
	}
	base := filepath.Base(rel)
	if skip[base] {
		return true
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 && skip[parts[0]] {
		return true
	}
	if strings.HasSuffix(base, ".tar.gz") {
		return true
	}
	if strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".db-shm") || strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-journal") {
		return true
	}
	lockFiles := map[string]bool{
		"package-lock.json": true,
		"pnpm-lock.yaml":    true,
		"yarn.lock":         true,
		"bun.lockb":         true,
		"bun.lock":          true,
	}
	if lockFiles[base] {
		return true
	}
	return false
}

func detectStaticSite(dir string, proj *detect.Project) bool {
	if proj.Entry != "" {
		return false
	}
	for _, d := range []string{"dist", "build", ".next/standalone"} {
		if info, err := os.Stat(filepath.Join(dir, d)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func detectGitVersion(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func humanSize(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
