// Package serve implements the dew serve HTTP API and app management.
// No build tags — compiles on all platforms.
package serve

import (
	"bytes"
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

	"github.com/solcreek/dew/internal/tlscert"
)

type State struct {
	mu       sync.RWMutex
	apps     map[string]*AppRecord
	nextPort int
	DataDir  string
}

type AppRecord struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Version    string `json:"version"`
	Status     string `json:"status"`
	Port       int    `json:"port,omitempty"`
	URL        string `json:"url,omitempty"`
	DeployDir  string `json:"deploy_dir"`
	DeployedAt string `json:"deployed_at"`
	stop       func()
}

type Manifest struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Runtime string `json:"runtime"`
	Type    string `json:"type"`
	Entry   string `json:"entry"`
	Port    int    `json:"port"`
}

var Version = "dev"

func NewState(dataDir string) *State {
	return &State{
		apps:     make(map[string]*AppRecord),
		nextPort: 10000,
		DataDir:  dataDir,
	}
}

func (s *State) AllocatePort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nextPort
	s.nextPort++
	return p
}

func (s *State) SetApp(name string, app *AppRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[name] = app
}

func (s *State) GetApp(name string) (*AppRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.apps[name]
	return a, ok
}

func (s *State) ListApps() []*AppRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	apps := make([]*AppRecord, 0, len(s.apps))
	for _, a := range s.apps {
		apps = append(apps, a)
	}
	return apps
}

func (s *State) StopApp(name string) {
	s.mu.Lock()
	app, ok := s.apps[name]
	if ok {
		if app.stop != nil {
			app.stop()
		}
		delete(s.apps, name)
	}
	s.mu.Unlock()
}

func Run(port, dataDir, tokenFile string) error {
	state := NewState(dataDir)

	if tokenFile == "" {
		tokenFile = filepath.Join(dataDir, "token")
	}

	verifyToken, err := loadTokenVerifier(dataDir, tokenFile)
	if err != nil {
		return err
	}

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
			if !strings.HasPrefix(h, "Bearer ") || !verifyToken(strings.TrimPrefix(h, "Bearer ")) {
				http.Error(w, `{"error":"unauthorized"}`, 401)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /v1/system/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"version":"%s"}`, Version)
	})

	mux.HandleFunc("GET /v1/apps", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state.ListApps())
	}))

	mux.HandleFunc("POST /v1/apps/{app}/deploy", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		if r.Header.Get("Content-Type") == "application/json" {
			handleImageDeploy(w, r, appName, state)
			return
		}
		handleTarballDeploy(w, r, appName, state)
	}))

	mux.HandleFunc("GET /v1/apps/{app}/health", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		app, ok := state.GetApp(appName)
		if !ok {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(app)
	}))

	mux.HandleFunc("POST /v1/apps/{app}/rollback", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		app, ok := state.GetApp(appName)
		if !ok {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"app":"%s","version":"%s","action":"rollback"}`, appName, app.Version)
	}))

	mux.HandleFunc("DELETE /v1/apps/{app}", auth(func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		state.StopApp(appName)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"app":"%s","action":"deleted"}`, appName)
	}))

	mux.HandleFunc("GET /proxy/{app}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		appName := r.PathValue("app")
		app, ok := state.GetApp(appName)
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
	tlsDir := filepath.Join(dataDir, "tls")
	cert, err := tlscert.Generate(tlsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: TLS cert generation failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "  💧 dew serve listening on %s (HTTP, insecure)\n", addr)
		fmt.Fprintf(os.Stderr, "  Health: http://localhost%s/v1/system/health\n\n", addr)
		return http.ListenAndServe(addr, mux)
	}

	fmt.Fprintf(os.Stderr, "  💧 dew serve listening on %s (HTTPS)\n", addr)
	fmt.Fprintf(os.Stderr, "  Fingerprint: %s\n", cert.Fingerprint)
	fmt.Fprintf(os.Stderr, "  Health: https://localhost%s/v1/system/health\n\n", addr)
	return http.ListenAndServeTLS(addr, cert.CertFile, cert.KeyFile, mux)
}

func loadTokenVerifier(dataDir, tokenFile string) (func(string) bool, error) {
	hashFile := filepath.Join(dataDir, "token-hash")
	if data, err := os.ReadFile(hashFile); err == nil {
		storedHash := strings.TrimSpace(string(data))
		return func(t string) bool {
			h := sha256.Sum256([]byte(t))
			return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(storedHash)) == 1
		}, nil
	}
	if data, err := os.ReadFile(tokenFile); err == nil {
		serverToken := strings.TrimSpace(string(data))
		return func(t string) bool { return t == serverToken }, nil
	}
	return nil, fmt.Errorf("no token found at %s or %s", hashFile, tokenFile)
}

func handleTarballDeploy(w http.ResponseWriter, r *http.Request, appName string, state *State) {
	checksumHeader := r.Header.Get("X-Deploy-Checksum")
	dataDir := state.DataDir

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
	cmd := exec.Command("sh", "-c", fmt.Sprintf("tar xzf %s -C %s", tarPath, appDir))
	// Capture tar stderr in addition to teeing it to the server's
	// own stderr (so the journal still has it). Without this, an
	// extract failure surfaced to the client as the opaque
	// "exit status 2" and the actual line ("Cannot create symlink
	// to ''", "No space left on device", …) was only readable via
	// SSH + journalctl. The captured bytes are inlined into the
	// SSE fail event so the client sees the cause without a round-
	// trip to the server.
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		// Cap to avoid blowing past SSE event size limits — tar
		// failures are usually one or two lines. Leave room for
		// the rest of the JSON envelope.
		if len(tail) > 4000 {
			tail = "...(truncated)...\n" + tail[len(tail)-4000:]
		}
		sendEvent("extract", "fail", fmt.Sprintf(",\"error\":%q,\"stderr\":%q",
			err.Error(), strings.TrimSpace(tail)))
		return
	}
	sendEvent("extract", "done", fmt.Sprintf(",\"path\":%q", appDir))

	var manifest Manifest
	if data, err := os.ReadFile(filepath.Join(appDir, "manifest.json")); err == nil {
		json.Unmarshal(data, &manifest)
	}

	state.StopApp(appName)
	appPort := state.AllocatePort()
	appURL := fmt.Sprintf("http://localhost:%d", appPort)

	var stopFn func()
	if manifest.Type == "static" {
		stopFn = startStaticServer(appDir, appPort)
		sendEvent("start", "done", fmt.Sprintf(",\"mode\":\"static\",\"port\":%d", appPort))
	} else {
		var err error
		stopFn, err = startProcessServer(appDir, manifest, appPort)
		if err != nil {
			sendEvent("start", "fail", fmt.Sprintf(",\"error\":%q", err.Error()))
			return
		}
		sendEvent("start", "done", fmt.Sprintf(",\"mode\":\"process\",\"port\":%d", appPort))
	}

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

	state.SetApp(appName, &AppRecord{
		Name: appName, Type: manifest.Type, Version: manifest.Version,
		Status: status, Port: appPort, URL: appURL,
		DeployDir: deployDir, DeployedAt: time.Now().UTC().Format(time.RFC3339),
		stop: stopFn,
	})

	fmt.Fprintf(w, "event: done\ndata: {\"ok\":true,\"app\":%q,\"version\":%q,\"url\":%q,\"port\":%d}\n\n",
		appName, manifest.Version, appURL, appPort)
	if flusher != nil {
		flusher.Flush()
	}
}

func handleImageDeploy(w http.ResponseWriter, r *http.Request, appName string, state *State) {
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
	hostPort := state.AllocatePort()

	state.StopApp(appName)

	runtime := "nerdctl"
	if _, err := exec.LookPath("nerdctl"); err != nil {
		runtime = "docker"
	}

	cmd := exec.Command(runtime, "run", "-d",
		"--name", "dew-"+appName,
		"-p", fmt.Sprintf("%d:%d", hostPort, containerPort),
		body.Image,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"container start failed: %s"}`, err), 500)
		return
	}

	appURL := fmt.Sprintf("http://localhost:%d", hostPort)
	state.SetApp(appName, &AppRecord{
		Name: appName, Type: "image", Version: body.Image,
		Status: "running", Port: hostPort, URL: appURL,
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		stop: func() {
			exec.Command(runtime, "rm", "-f", "dew-"+appName).Run()
		},
	})

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"app":"%s","image":"%s","url":"%s","port":%d}`, appName, body.Image, appURL, hostPort)
}

func startStaticServer(appDir string, port int) func() {
	staticDir := appDir
	for _, candidate := range []string{"app/dist", "app/build", "app/public", "dist", "build"} {
		d := filepath.Join(appDir, candidate)
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			staticDir = d
			break
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	go srv.ListenAndServe()
	return func() { srv.Close() }
}

func startProcessServer(appDir string, manifest Manifest, port int) (func(), error) {
	entry := manifest.Entry
	if entry == "" {
		return nil, fmt.Errorf("no entry point in manifest")
	}

	entryPath := filepath.Join(appDir, "app", entry)
	if _, err := os.Stat(entryPath); err != nil {
		entryPath = filepath.Join(appDir, entry)
	}

	runtime := manifest.Runtime
	if runtime == "" {
		runtime = "node"
	}
	if runtime == "node" && strings.HasSuffix(entry, ".ts") {
		runtime = "bun"
	}

	cmdName := runtime
	switch runtime {
	case "python":
		cmdName = "python3"
	}

	cmd := exec.Command(cmdName, entryPath)
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
