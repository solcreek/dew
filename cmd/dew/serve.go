//go:build darwin

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type serveState struct {
	mu       sync.RWMutex
	apps     map[string]*appRecord
	nextPort int
	dataDir  string
}

type appRecord struct {
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Version    string       `json:"version"`
	Status     string       `json:"status"`
	Port       int          `json:"port,omitempty"`
	URL        string       `json:"url,omitempty"`
	DeployDir  string       `json:"deploy_dir"`
	DeployedAt string       `json:"deployed_at"`
	cmd        *exec.Cmd
	stop       func()
}

var state = &serveState{
	apps:     make(map[string]*appRecord),
	nextPort: 10000,
}

func cmdServe(args []string) error {
	port := "9080"
	tokenFile := ""
	state.dataDir = "/var/dew"

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
				state.dataDir = args[i]
			}
		case "--token-file":
			i++
			if i < len(args) {
				tokenFile = args[i]
			}
		}
	}

	if tokenFile == "" {
		tokenFile = filepath.Join(state.dataDir, "token")
	}

	var verifyToken func(string) bool

	// Try hash file first (secure: VPS cloud-init only stores hash)
	hashFile := filepath.Join(state.dataDir, "token-hash")
	if data, err := os.ReadFile(hashFile); err == nil {
		storedHash := strings.TrimSpace(string(data))
		verifyToken = func(t string) bool {
			h := sha256.Sum256([]byte(t))
			return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(storedHash)) == 1
		}
	} else if data, err := os.ReadFile(tokenFile); err == nil {
		// Fallback: plaintext token file (local dev)
		serverToken := strings.TrimSpace(string(data))
		verifyToken = func(t string) bool { return t == serverToken }
	} else {
		return fmt.Errorf("no token found at %s or %s\nRun: dew serve init", hashFile, tokenFile)
	}

	for _, dir := range []string{
		filepath.Join(state.dataDir, "apps"),
		filepath.Join(state.dataDir, "data"),
		filepath.Join(state.dataDir, "deploys"),
	} {
		os.MkdirAll(dir, 0755)
	}

	mux := http.NewServeMux()

	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") || !verifyToken(strings.TrimPrefix(h, "Bearer ")) {
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
		if r.Header.Get("Content-Type") == "application/json" {
			handleImageDeploy(w, r, appName)
			return
		}
		handleTarballDeploy(w, r, appName)
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
		stopApp(appName)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"app":"%s","action":"deleted"}`, appName)
	}))

	mux.HandleFunc("POST /v1/apps/{app}/rollback", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		w.Header().Set("Content-Type", "application/json")
		// TODO: restore previous deploy from deploy history
		// For now: restart current version
		state.mu.RLock()
		app, ok := state.apps[appName]
		state.mu.RUnlock()
		if !ok {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"app":"%s","version":"%s","action":"rollback"}`, appName, app.Version)
	}))

	// Proxy requests to apps by X-App header or subdomain
	mux.HandleFunc("GET /proxy/{app}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		state.mu.RLock()
		app, ok := state.apps[appName]
		state.mu.RUnlock()
		if !ok || app.Port == 0 {
			http.Error(w, "app not found", 404)
			return
		}
		target, _ := url.Parse(fmt.Sprintf("http://localhost:%d", app.Port))
		proxy := httputil.NewSingleHostReverseProxy(target)
		r.URL.Path = "/" + r.PathValue("path")
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + port
	fmt.Fprintf(os.Stderr, "  💧 dew serve listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "  Health: http://localhost%s/v1/system/health\n\n", addr)

	return http.ListenAndServe(addr, mux)
}

func allocatePort() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	p := state.nextPort
	state.nextPort++
	return p
}

func stopApp(name string) {
	state.mu.Lock()
	app, ok := state.apps[name]
	if ok {
		if app.stop != nil {
			app.stop()
		}
		delete(state.apps, name)
	}
	state.mu.Unlock()
}

func handleTarballDeploy(w http.ResponseWriter, r *http.Request, appName string) {
	checksumHeader := r.Header.Get("X-Deploy-Checksum")
	dataDir := state.dataDir

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

	// Receive tarball
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

	// Verify checksum
	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if checksumHeader != "" && checksumHeader != checksum {
		sendEvent("verify", "fail", ",\"error\":\"checksum mismatch\"")
		return
	}
	sendEvent("verify", "done", fmt.Sprintf(",\"checksum\":%q", checksum))

	// Extract
	appDir := filepath.Join(dataDir, "apps", appName)
	os.MkdirAll(appDir, 0755)
	if err := runShell(fmt.Sprintf("tar xzf %s -C %s", tarPath, appDir)); err != nil {
		sendEvent("extract", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
		return
	}
	sendEvent("extract", "done", fmt.Sprintf(",\"path\":%q", appDir))

	// Read manifest
	var manifest buildManifest
	if data, err := os.ReadFile(filepath.Join(appDir, "manifest.json")); err == nil {
		json.Unmarshal(data, &manifest)
	}

	// Stop old version
	stopApp(appName)

	// Start app
	appPort := allocatePort()
	appURL := fmt.Sprintf("http://localhost:%d", appPort)

	var stopFn func()
	switch manifest.Type {
	case "static":
		stopFn = startStaticServer(appDir, appPort)
		sendEvent("start", "done", fmt.Sprintf(",\"mode\":\"static\",\"port\":%d", appPort))
	default:
		var err error
		if hasContainerd() {
			stopFn, err = startContainerServer(appDir, manifest, appPort)
			if err == nil {
				sendEvent("start", "done", fmt.Sprintf(",\"mode\":\"container\",\"port\":%d", appPort))
				break
			}
			fmt.Fprintf(os.Stderr, "  container start failed, falling back to process: %v\n", err)
		}
		stopFn, err = startProcessServer(appDir, manifest, appPort)
		if err != nil {
			sendEvent("start", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
			return
		}
		sendEvent("start", "done", fmt.Sprintf(",\"mode\":\"process\",\"port\":%d", appPort))
	}

	// Health check
	healthy := false
	for i := 0; i < 15; i++ {
		time.Sleep(time.Second)
		resp, err := http.Get(appURL + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				healthy = true
				break
			}
		}
	}

	status := "running"
	if !healthy {
		status = "unhealthy"
	}
	sendEvent("health", status, fmt.Sprintf(",\"url\":%q", appURL))

	// Register
	state.mu.Lock()
	state.apps[appName] = &appRecord{
		Name:       appName,
		Type:       manifest.Type,
		Version:    manifest.Version,
		Status:     status,
		Port:       appPort,
		URL:        appURL,
		DeployDir:  deployDir,
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		stop:       stopFn,
	}
	state.mu.Unlock()

	fmt.Fprintf(w, "event: done\ndata: {\"ok\":true,\"app\":%q,\"version\":%q,\"url\":%q,\"port\":%d}\n\n",
		appName, manifest.Version, appURL, appPort)
	if flusher != nil {
		flusher.Flush()
	}
}

func startStaticServer(appDir string, port int) func() {
	// Find the static output dir
	staticDir := appDir
	for _, candidate := range []string{"app/dist", "app/build", "app/public", "dist", "build"} {
		d := filepath.Join(appDir, candidate)
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			staticDir = d
			break
		}
	}

	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir(staticDir))
	mux.Handle("/", fs)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	go srv.ListenAndServe()

	return func() { srv.Close() }
}

func startProcessServer(appDir string, manifest buildManifest, port int) (func(), error) {
	entry := manifest.Entry
	if entry == "" {
		return nil, fmt.Errorf("no entry point in manifest")
	}

	entryPath := filepath.Join(appDir, "app", entry)
	if _, err := os.Stat(entryPath); err != nil {
		entryPath = filepath.Join(appDir, entry)
	}

	// Resolve runtime binary
	runtime, err := resolveRuntime(manifest.Runtime, entry)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(runtime, entryPath)
	cmd.Dir = filepath.Dir(entryPath)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"NODE_ENV=production",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", entry, err)
	}

	return func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}, nil
}

var runtimeImages = map[string]string{
	"bun":    "oven/bun:1.3-alpine",
	"node":   "node:22-alpine",
	"python": "python:3.12-alpine",
}

func resolveRuntime(runtime, entry string) (string, error) {
	if runtime == "" {
		runtime = "node"
	}

	// For .ts files, prefer bun
	if runtime == "node" && strings.HasSuffix(entry, ".ts") {
		runtime = "bun"
	}

	binaryName := runtime
	switch runtime {
	case "python":
		binaryName = "python3"
	}

	// 1. Check PATH
	if p, err := exec.LookPath(binaryName); err == nil {
		return p, nil
	}

	// 2. Check dew runtimes dir
	localBin := filepath.Join(dewDataDir(), "runtimes", runtime, "bin", binaryName)
	if runtime == "bun" {
		localBin = filepath.Join(dewDataDir(), "runtimes", "bun", "bun")
	}
	if _, err := os.Stat(localBin); err == nil {
		return localBin, nil
	}

	// 3. Auto-download
	fmt.Fprintf(os.Stderr, "  Downloading %s runtime...\n", runtime)
	downloaded, err := downloadRuntime(runtime)
	if err != nil {
		return "", fmt.Errorf("runtime %q not found and auto-download failed: %w", runtime, err)
	}
	return downloaded, nil
}

func downloadRuntime(runtime string) (string, error) {
	dir := filepath.Join(dewDataDir(), "runtimes", runtime)
	os.MkdirAll(dir, 0755)

	switch runtime {
	case "bun":
		binPath := filepath.Join(dir, "bun")
		cmd := exec.Command("sh", "-c",
			fmt.Sprintf("curl -fsSL https://bun.sh/install | BUN_INSTALL=%s bash", dir))
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", err
		}
		if _, err := os.Stat(binPath); err == nil {
			return binPath, nil
		}
		binPath = filepath.Join(dir, "bin", "bun")
		return binPath, nil

	case "node":
		arch := "x64"
		platform := "darwin"
		if out, _ := exec.Command("uname", "-m").Output(); strings.TrimSpace(string(out)) == "arm64" {
			arch = "arm64"
		}
		if out, _ := exec.Command("uname", "-s").Output(); strings.TrimSpace(string(out)) == "Linux" {
			platform = "linux"
		}
		url := fmt.Sprintf("https://nodejs.org/dist/v22.0.0/node-v22.0.0-%s-%s.tar.xz", platform, arch)
		cmd := exec.Command("sh", "-c",
			fmt.Sprintf("curl -fsSL '%s' | tar xJ --strip-components=1 -C '%s'", url, dir))
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return filepath.Join(dir, "bin", "node"), nil

	default:
		return "", fmt.Errorf("auto-download not supported for runtime %q", runtime)
	}
}

func runtimeBaseImage(runtime string) string {
	if img, ok := runtimeImages[runtime]; ok {
		return img
	}
	return "node:22-alpine"
}

func handleImageDeploy(w http.ResponseWriter, r *http.Request, appName string) {
	var body struct {
		Image string `json:"image"`
		Port  int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Image == "" {
		http.Error(w, `{"error":"image field required"}`, 400)
		return
	}

	containerPort := body.Port
	if containerPort == 0 {
		containerPort = 80
	}
	hostPort := allocatePort()

	stopApp(appName)

	// Try nerdctl first, then docker
	runtime := "nerdctl"
	if _, err := exec.LookPath("nerdctl"); err != nil {
		runtime = "docker"
	}

	cmd := exec.Command(runtime, "run", "-d",
		"--name", "dew-"+appName,
		"-p", fmt.Sprintf("%d:%d", hostPort, containerPort),
		body.Image,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"container start failed: %s"}`, err), 500)
		return
	}

	appURL := fmt.Sprintf("http://localhost:%d", hostPort)

	state.mu.Lock()
	state.apps[appName] = &appRecord{
		Name:       appName,
		Type:       "image",
		Version:    body.Image,
		Status:     "running",
		Port:       hostPort,
		URL:        appURL,
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		stop: func() {
			exec.Command(runtime, "rm", "-f", "dew-"+appName).Run()
		},
	}
	state.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"app":"%s","image":"%s","url":"%s","port":%d}`, appName, body.Image, appURL, hostPort)
}

func hasContainerd() bool {
	_, err := exec.LookPath("nerdctl")
	return err == nil
}

func startContainerServer(appDir string, manifest buildManifest, port int) (func(), error) {
	runtime := manifest.Runtime
	if runtime == "" {
		runtime = "node"
	}
	if runtime == "node" && strings.HasSuffix(manifest.Entry, ".ts") {
		runtime = "bun"
	}

	baseImage := runtimeBaseImage(runtime)
	containerName := "dew-app-" + filepath.Base(appDir)

	exec.Command("nerdctl", "rm", "-f", containerName).Run()

	entry := manifest.Entry
	if entry == "" {
		entry = "server.js"
	}

	cmd := exec.Command("nerdctl", "run", "-d",
		"--name", containerName,
		"--mount", fmt.Sprintf("type=bind,src=%s/app,dst=/app", appDir),
		"-e", fmt.Sprintf("PORT=%d", port),
		"-e", "NODE_ENV=production",
		"-p", fmt.Sprintf("%d:%d", port, port),
		"-w", "/app",
		baseImage,
		runtime, "/app/"+entry,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nerdctl run: %w", err)
	}

	return func() {
		exec.Command("nerdctl", "rm", "-f", containerName).Run()
	}, nil
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
