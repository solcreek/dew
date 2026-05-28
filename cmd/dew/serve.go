//go:build darwin

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type serveState struct {
	mu   sync.RWMutex
	apps map[string]*appRecord
}

type appRecord struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Version   string `json:"version"`
	Status    string `json:"status"`
	Port      int    `json:"port,omitempty"`
	DeployDir string `json:"deploy_dir"`
	DeployedAt string `json:"deployed_at"`
}

var state = &serveState{apps: make(map[string]*appRecord)}

func cmdServe(args []string) error {
	port := "9080"
	dataDir := "/var/dew"
	tokenFile := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			i++
			if i < len(args) {
				port = args[i]
			}
		case "--data-dir":
			i++
			if i < len(args) {
				dataDir = args[i]
			}
		case "--token-file":
			i++
			if i < len(args) {
				tokenFile = args[i]
			}
		}
	}

	if tokenFile == "" {
		tokenFile = filepath.Join(dataDir, "token")
	}

	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("cannot read token from %s: %w\nRun: dew serve init", tokenFile, err)
	}
	serverToken := strings.TrimSpace(string(token))

	for _, dir := range []string{
		filepath.Join(dataDir, "apps"),
		filepath.Join(dataDir, "data"),
		filepath.Join(dataDir, "deploys"),
	} {
		os.MkdirAll(dir, 0755)
	}

	mux := http.NewServeMux()

	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") || strings.TrimPrefix(h, "Bearer ") != serverToken {
				http.Error(w, `{"error":"unauthorized"}`, 401)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /v1/system/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"version":"%s"}`, version)
	})

	mux.HandleFunc("GET /v1/apps", auth(func(w http.ResponseWriter, r *http.Request) {
		state.mu.RLock()
		defer state.mu.RUnlock()
		apps := make([]*appRecord, 0, len(state.apps))
		for _, a := range state.apps {
			apps = append(apps, a)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apps)
	}))

	mux.HandleFunc("POST /v1/apps/{app}/deploy", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		contentType := r.Header.Get("Content-Type")

		if contentType == "application/json" {
			handleImageDeploy(w, r, appName, dataDir)
			return
		}

		handleTarballDeploy(w, r, appName, dataDir)
	}))

	mux.HandleFunc("GET /v1/apps/{app}/health", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		state.mu.RLock()
		app, ok := state.apps[appName]
		state.mu.RUnlock()
		if !ok {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(app)
	}))

	mux.HandleFunc("DELETE /v1/apps/{app}", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		state.mu.Lock()
		delete(state.apps, appName)
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"app":"%s","action":"deleted"}`, appName)
	}))

	addr := ":" + port
	fmt.Fprintf(os.Stderr, "  💧 dew serve listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "  Health: http://localhost%s/v1/system/health\n\n", addr)

	return http.ListenAndServe(addr, mux)
}

func handleTarballDeploy(w http.ResponseWriter, r *http.Request, appName, dataDir string) {
	checksumHeader := r.Header.Get("X-Deploy-Checksum")

	deployID := fmt.Sprintf("deploy_%d", time.Now().Unix())
	deployDir := filepath.Join(dataDir, "deploys", appName, deployID)
	os.MkdirAll(deployDir, 0755)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)

	sendEvent := func(phase, status, extra string) {
		fmt.Fprintf(w, "event: step\ndata: {\"phase\":%q,\"status\":%q%s}\n\n", phase, status, extra)
		if flusher != nil {
			flusher.Flush()
		}
	}

	tarPath := filepath.Join(deployDir, "app.tar.gz")
	f, err := os.Create(tarPath)
	if err != nil {
		sendEvent("receive", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
		return
	}

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), r.Body)
	f.Close()
	if err != nil {
		sendEvent("receive", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
		return
	}
	sendEvent("receive", "done", fmt.Sprintf(",\"bytes\":%d", written))

	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if checksumHeader != "" && checksumHeader != checksum {
		sendEvent("verify", "fail", ",\"error\":\"checksum mismatch\"")
		return
	}
	sendEvent("verify", "done", fmt.Sprintf(",\"checksum\":%q", checksum))

	appDir := filepath.Join(dataDir, "apps", appName)
	os.MkdirAll(appDir, 0755)

	extractCmd := fmt.Sprintf("tar xzf %s -C %s", tarPath, appDir)
	if err := runShell(extractCmd); err != nil {
		sendEvent("extract", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
		return
	}
	sendEvent("extract", "done", fmt.Sprintf(",\"path\":%q", appDir))

	var manifest buildManifest
	manifestPath := filepath.Join(appDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		json.Unmarshal(data, &manifest)
	}

	state.mu.Lock()
	state.apps[appName] = &appRecord{
		Name:       appName,
		Type:       manifest.Type,
		Version:    manifest.Version,
		Status:     "deployed",
		Port:       manifest.Port,
		DeployDir:  deployDir,
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
	}
	state.mu.Unlock()

	sendEvent("start", "done", fmt.Sprintf(",\"app\":%q", appName))

	fmt.Fprintf(w, "event: done\ndata: {\"ok\":true,\"app\":%q,\"version\":%q,\"deploy_id\":%q}\n\n",
		appName, manifest.Version, deployID)
	if flusher != nil {
		flusher.Flush()
	}
}

func handleImageDeploy(w http.ResponseWriter, r *http.Request, appName, dataDir string) {
	var body struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Image == "" {
		http.Error(w, `{"error":"image field required"}`, 400)
		return
	}

	state.mu.Lock()
	state.apps[appName] = &appRecord{
		Name:       appName,
		Type:       "image",
		Version:    body.Image,
		Status:     "deployed",
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
	}
	state.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"app":"%s","image":"%s"}`, appName, body.Image)
}

func runShell(cmd string) error {
	return runCmd("sh", "-c", cmd)
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	return c.Run()
}
