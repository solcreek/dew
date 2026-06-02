//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/solcreek/dew/internal/progress"
	"github.com/solcreek/dew/internal/validate"
	"github.com/solcreek/dew/pkg/dewerr"
)

// registryBase is the dew-apps registry root. var (not const) so
// tests can point it at httptest servers without hitting the network.
var registryBase = "https://raw.githubusercontent.com/solcreek/dew-apps/main"

type appManifest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Runtime     string            `json:"runtime"`
	Type        string            `json:"type"`
	BaseImage   string            `json:"base_image"`
	Port        int               `json:"port"`
	Volumes     map[string]string `json:"volumes"`
	Env         map[string]string `json:"env"`
	HealthCheck string            `json:"health_check"`
	DockerImage string            `json:"docker_image"`
	Tags        []string          `json:"tags"`
}

func cmdInstall(args []string) error {
	printAppsDeprecationNotice()
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(`dew app — run open-source apps from the registry

Usage:
  dew app run <name> [--port N] [--dry-run] [--json]    Run an app
  dew app stop <name>                                    Stop an app
  dew app list [--json]                                  Show running apps

Browse:
  dew apps                                              List available apps

Examples:
  dew app run excalidraw --port 3000
  dew app run ghost --port 2368
  dew app stop excalidraw
  dew app list`)
		return nil
	}

	switch args[0] {
	case "run":
		if len(args) < 2 {
			return dewerr.New(dewerr.CodeUsage, "usage: dew app run <name> [--port N]")
		}
		if args[1] == "--help" || args[1] == "-h" {
			fmt.Println(`dew app run — run an app from the registry

Usage:
  dew app run <name> [--port N] [--dry-run] [--json]

Flags:
  --port, -p <N>    Host port to expose (default: app's default port)
  --dry-run         Show what would happen without running
  --json            Machine-readable JSON output

Browse available apps: dew apps`)
			return nil
		}
		return cmdAppRun(args[1:])
	case "stop":
		if len(args) < 2 {
			return dewerr.New(dewerr.CodeUsage, "usage: dew app stop <name>")
		}
		return cmdAppStop(args[1])
	case "list":
		return cmdAppList()
	default:
		// Backward compat: dew app <name> = dew app run <name>
		return cmdAppRun(args)
	}
}

func cmdAppStop(name string) error {
	runtime := "docker"
	if _, err := lookPath("nerdctl"); err == nil {
		runtime = "nerdctl"
	}
	containerName := "dew-" + name
	cmd := exec.Command(runtime, "rm", "-f", containerName)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	fmt.Fprintf(os.Stderr, "  %s stopped\n", name)
	return nil
}

func cmdAppList() error {
	// Parse --json flag from os.Args (cmdAppList is called without args here)
	for _, a := range os.Args {
		if a == "--json" {
			flagJSON = true
		}
	}

	runtime := "docker"
	if _, err := lookPath("nerdctl"); err == nil {
		runtime = "nerdctl"
	}
	if _, err := lookPath(runtime); err != nil {
		if flagJSON {
			fmt.Println(`{"ok":false,"error":"no container runtime found","code":"no_runtime","apps":[]}`)
			return nil
		}
		return dewerr.New(dewerr.CodeNotFound, "no container runtime (docker/nerdctl) found")
	}

	if flagJSON {
		cmd := exec.Command(runtime, "ps", "--filter", "name=dew-", "--format", "{{json .}}")
		out, err := cmd.Output()
		if err != nil {
			fmt.Printf(`{"ok":false,"error":%q,"code":"runtime_error","apps":[]}`+"\n", err.Error())
			return nil
		}
		// Output is one JSON object per line; wrap into an array
		fmt.Print(`{"ok":true,"apps":[`)
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		first := true
		for _, line := range lines {
			if line == "" {
				continue
			}
			if !first {
				fmt.Print(",")
			}
			first = false
			fmt.Print(line)
		}
		fmt.Println("]}")
		return nil
	}

	cmd := exec.Command(runtime, "ps", "--filter", "name=dew-", "--format", "table {{.Names}}\t{{.Status}}\t{{.Ports}}")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cmdAppRun(args []string) error {
	appName := args[0]
	if err := validate.AppName(appName); err != nil {
		return err
	}

	hostPort := 0
	noFallback := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i < len(args) {
				fmt.Sscanf(args[i], "%d", &hostPort)
			}
		case "--dry-run":
			flagDryRun = true
		case "--json":
			flagJSON = true
		case "--events":
			flagEvents = true
		case "--no-fallback":
			noFallback = true
		}
	}

	// When --json or --events is set, suppress spinners.
	if suppressProgress() {
		os.Setenv("DEW_NO_PROGRESS", "1")
	}

	if hostPort > 0 {
		if err := validate.Port(hostPort); err != nil {
			return err
		}
	}

	sp := progress.New()

	sp.Step("Fetching manifest")
	manifest, err := fetchManifest(appName)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}

	runtime := manifest.Runtime
	if runtime == "" && manifest.DockerImage != "" {
		runtime = "container"
	}
	if runtime == "" && manifest.Type == "static" {
		runtime = "static"
	}

	fmt.Fprintf(os.Stderr, "\n  %s v%s\n", manifest.Name, manifest.Version)
	fmt.Fprintf(os.Stderr, "  %s\n", manifest.Description)
	fmt.Fprintf(os.Stderr, "  Port: %d | Runtime: %s\n\n", manifest.Port, runtime)

	if flagDryRun {
		exposedPort := manifest.Port
		if hostPort > 0 {
			exposedPort = hostPort
		}
		if flagJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{
				"ok":             true,
				"schema_version": schemaVersion,
				"data": map[string]any{
					"type":        "dry-run",
					"app":         manifest.Name,
					"version":     manifest.Version,
					"runtime":     runtime,
					"image":       manifest.DockerImage,
					"port":        manifest.Port,
					"host_port":   exposedPort,
					"would_pull":  manifest.DockerImage != "",
					"would_start": false,
				},
			})
		}
		fmt.Fprintf(os.Stderr, "  Dry run:\n")
		// Empty image case (node-runtime apps): say so explicitly
		// rather than "Would pull " with a blank line — the agent
		// report flagged uptime-kuma's empty pull as confusing.
		if manifest.DockerImage != "" {
			fmt.Fprintf(os.Stderr, "  Would pull %s\n", manifest.DockerImage)
		} else {
			fmt.Fprintf(os.Stderr, "  No image to pull (%s runtime — built from source)\n", runtime)
		}
		fmt.Fprintf(os.Stderr, "  Would expose port %d\n", exposedPort)
		fmt.Fprintf(os.Stderr, "  No containers started.\n")
		return nil
	}

	if manifest.DockerImage != "" {
		exposedPort := manifest.Port
		if hostPort > 0 {
			exposedPort = hostPort
		}
		containerName := "dew-" + manifest.Name

		// Ensure Dew VM is running with containerd
		sp.Step("Preparing environment")
		emitEvent("preparing", map[string]any{"backend": "vm"})
		vmErr := ensureDewVM(exposedPort, manifest.Port, sp)
		backend := "vm"
		if vmErr != nil {
			sp.Fail(fmt.Sprintf("VM boot failed: %v", vmErr))
			emitEvent("vm_failed", map[string]any{
				"error": fmtErr(vmErr),
				"code":  "vm_boot_failed",
			})
			if noFallback {
				return fmt.Errorf("VM boot failed and --no-fallback set: %w", vmErr)
			}
			// Check if any host container runtime is available before falling back
			runtime := ""
			if _, err := lookPath("nerdctl"); err == nil {
				runtime = "nerdctl"
			} else if _, err := lookPath("docker"); err == nil {
				runtime = "docker"
			}
			if runtime == "" {
				emitEvent("fallback_unavailable", map[string]any{
					"reason": "no_host_runtime",
				})
				return fmt.Errorf("VM boot failed: %w\n\n  No host container runtime (docker/nerdctl) found as fallback.\n  Try: dew doctor", vmErr)
			}
			backend = runtime
			emitEvent("fallback", map[string]any{
				"from":   "vm",
				"to":     runtime,
				"reason": fmtErr(vmErr),
			})
			fmt.Fprintf(os.Stderr, "  Falling back to host %s...\n", runtime)
			sp.Step(fmt.Sprintf("Starting %s (host %s)", manifest.Name, runtime))
			exec.Command(runtime, "rm", "-f", containerName).Run()
			runArgs := buildRunArgs(containerName, exposedPort, manifest)
			cmd := exec.Command(runtime, runArgs...)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				sp.Fail("start failed")
				emitEvent("start_failed", map[string]any{
					"backend": runtime, "error": fmtErr(err),
				})
				return fmt.Errorf("failed to start %s: %w", manifest.Name, err)
			}
		} else {
			// Run inside Dew VM — map container port to exposedPort inside VM
			sp.Step(fmt.Sprintf("Starting %s", manifest.Name))
			dewExec := exec.Command(os.Args[0], "exec",
				fmt.Sprintf("export TMPDIR=/var/lib/containerd/tmp && mkdir -p /var/lib/containerd/tmp && nerdctl rm -f %s 2>/dev/null; nerdctl run -d --name %s -p %d:%d %s",
					containerName, containerName, exposedPort, manifest.Port, manifest.DockerImage))
			dewExec.Stderr = os.Stderr
			if err := dewExec.Run(); err != nil {
				sp.Fail("start failed in VM")
				emitEvent("start_failed", map[string]any{
					"backend": "vm", "error": fmtErr(err),
				})
				return fmt.Errorf("failed to start %s in VM: %w", manifest.Name, err)
			}
		}
		emitEvent("started", map[string]any{
			"backend":   backend,
			"container": containerName,
			"port":      exposedPort,
		})

		if manifest.HealthCheck != "" {
			sp.Step("Waiting for healthy")
			healthy := false
			url := fmt.Sprintf("http://localhost:%d%s", exposedPort, manifest.HealthCheck)
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				resp, err := http.Get(url)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 400 {
						healthy = true
						break
					}
				}
			}
			if !healthy {
				sp.Fail("health check timed out")
				emitEvent("health", map[string]any{
					"status": "unhealthy",
					"url":    fmt.Sprintf("http://localhost:%d%s", exposedPort, manifest.HealthCheck),
				})
				fmt.Fprintf(os.Stderr, "  Container started but http://localhost:%d%s is not responding.\n", exposedPort, manifest.HealthCheck)
				fmt.Fprintf(os.Stderr, "  The app may still be starting. Try opening the URL in a few seconds.\n\n")
				return nil
			}
			emitEvent("health", map[string]any{
				"status": "ok",
				"url":    fmt.Sprintf("http://localhost:%d%s", exposedPort, manifest.HealthCheck),
			})
		}

		sp.Done(fmt.Sprintf("http://localhost:%d", exposedPort))
		fmt.Fprintf(os.Stderr, "  %s is running at http://localhost:%d\n\n", manifest.Name, exposedPort)

		// Final structured result: --json emits the single summary object;
		// --events already streamed lifecycle events and emits "done" here.
		appURL := fmt.Sprintf("http://localhost:%d", exposedPort)
		if flagEvents {
			emitEvent("done", map[string]any{
				"app": manifest.Name, "port": exposedPort,
				"url": appURL, "backend": backend,
			})
		} else if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]any{
				"ok":      true,
				"app":     manifest.Name,
				"port":    exposedPort,
				"url":     appURL,
				"backend": backend,
			})
		}
		return nil
	}

	return fmt.Errorf("app %s has no docker_image — tarball install not yet implemented", appName)
}

func cmdInstallList() error {
	printAppsDeprecationNotice()
	resp, err := http.Get(registryBase + "/registry.json")
	if err != nil {
		return dewerr.Wrap(err, dewerr.CodeNetwork, "fetch registry")
	}
	defer resp.Body.Close()

	var reg struct {
		Apps []string `json:"apps"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)

	// --json: emit a single object with the full per-app manifests an
	// agent might want to filter on (port, runtime, version, image).
	// One round-trip per app to fetch the manifest matches the human
	// path; this isn't a hot loop and the registry is small.
	if flagJSON {
		type appEntry struct {
			Name        string   `json:"name"`
			Version     string   `json:"version"`
			Description string   `json:"description"`
			Runtime     string   `json:"runtime"`
			Type        string   `json:"type"`
			Port        int      `json:"port"`
			Image       string   `json:"image,omitempty"`
			Tags        []string `json:"tags,omitempty"`
			Error       string   `json:"error,omitempty"`
		}
		entries := make([]appEntry, 0, len(reg.Apps))
		for _, name := range reg.Apps {
			m, err := fetchManifest(name)
			if err != nil {
				entries = append(entries, appEntry{Name: name, Error: err.Error()})
				continue
			}
			entries = append(entries, appEntry{
				Name:        m.Name,
				Version:     m.Version,
				Description: m.Description,
				Runtime:     m.Runtime,
				Type:        m.Type,
				Port:        m.Port,
				Image:       m.DockerImage,
				Tags:        m.Tags,
			})
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           map[string]any{"apps": entries},
		})
	}

	fmt.Printf("Available apps (%d):\n\n", len(reg.Apps))
	for _, name := range reg.Apps {
		m, err := fetchManifest(name)
		if err != nil {
			fmt.Printf("  %-20s (manifest error)\n", name)
			continue
		}
		fmt.Printf("  %-20s %s\n", name, m.Description)
	}
	fmt.Println()
	fmt.Println("Install: dew install <app-name>")
	return nil
}

func fetchManifest(name string) (*appManifest, error) {
	url := fmt.Sprintf("%s/apps/%s/manifest.json", registryBase, name)
	resp, err := http.Get(url)
	if err != nil {
		return nil, dewerr.Wrap(err, dewerr.CodeNetwork, "fetch manifest")
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, dewerr.Newf(dewerr.CodeNotFound, "app %q not found in registry", name)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry error: %d %s", resp.StatusCode, body)
	}

	var m appManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

func ensureDewVM(hostPort, guestPort int, sp *progress.Spinner) error {
	home, _ := os.UserHomeDir()
	sock := filepath.Join(home, ".local", "state", "dew", "default.sock")
	if _, err := os.Stat(sock); err == nil {
		return nil
	}

	// Start VM with NAT networking + port forwarding
	// Multiple apps share one VM; each app gets its own port forward
	args := []string{"start",
		"--profile", "standard",
		"--forward", fmt.Sprintf("%d:%d", hostPort, hostPort),
		"--network",
		"--json",
	}
	// Pre-forward common port range for subsequent apps. Best-effort —
	// skip ports already bound by some other process on the host (the
	// user might be running a dev server on 3000, etc.). Without this
	// skip, a single in-use port would prevent VM start with a cryptic
	// "exited early" error, since `dew start --forward N:N` is strict
	// when N is already bound. The explicit hostPort is still added
	// unconditionally — if THAT one is taken, fail loud (the user
	// asked for it).
	for _, p := range []int{3000, 3001, 3002, 3003, 3004, 3005, 8080, 8000, 7456, 5230, 2368} {
		if p == hostPort {
			continue
		}
		if hostPortInUse(p) {
			continue
		}
		args = append(args, "--forward", fmt.Sprintf("%d:%d", p, p))
	}
	cmd := exec.Command(os.Args[0], args...)
	// IMPORTANT: do NOT forward child stdout to our stdout/stderr — it would
	// leak a structured JSON error line into the human progress stream when
	// --json is passed downstream. Capture and discard; we surface the real
	// failure via our own structured event / return value.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// Monitor for early child process death (VZ entitlement failures, etc.)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Wait for VM to be ready. Cold-start (no cached assets + fresh disk)
	// needs ~60-120s: ~30s to download 146MB of vmlinuz + initramfs from
	// GH Release on first use, ~30-60s for first-boot disk init (mkfs +
	// copy rootfs + switch_root + containerd). Hot-start is sub-5s.
	// Generous 300s ceiling covers cold + slow networks.
	const timeoutSec = 300
	for i := 0; i < timeoutSec; i++ {
		select {
		case err := <-exited:
			if err != nil {
				return fmt.Errorf("dew start exited early: %w\n\n  This often means:\n  - The binary is missing the virtualization entitlement (try: dew update)\n  - You're on macOS < 13 (Apple VZ requires macOS 13+)\n  - Another VM is using port forwards (try: dew down)\n", err)
			}
			return fmt.Errorf("dew start exited without setting up the VM")
		case <-time.After(time.Second):
		}
		if _, err := os.Stat(sock); err == nil {
			// Give containerd a moment to start
			time.Sleep(2 * time.Second)
			return nil
		}
		// Heuristic progress messages so the user isn't staring at a
		// black "Preparing environment..." for minutes on first run.
		// Times tuned for cold + first-boot path.
		if sp != nil {
			switch i {
			case 15:
				sp.Step("Downloading VM assets (~146MB, first run only)")
			case 60:
				sp.Step("Booting VM")
			case 120:
				sp.Step("First-time disk init (formatting + populating rootfs)")
			case 200:
				sp.Step("Still working — slow network or first-boot setup")
			}
		}
	}
	return fmt.Errorf("VM did not start within %ds", timeoutSec)
}

func buildRunArgs(containerName string, exposedPort int, manifest *appManifest) []string {
	runArgs := []string{"run", "-d", "--name", containerName, "-p", fmt.Sprintf("%d:%d", exposedPort, manifest.Port)}
	for _, path := range manifest.Volumes {
		hostDir := filepath.Join(homeDir(), "dew-data", manifest.Name)
		os.MkdirAll(hostDir, 0755)
		runArgs = append(runArgs, "-v", fmt.Sprintf("%s:%s", hostDir, path))
	}
	for k, v := range manifest.Env {
		runArgs = append(runArgs, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	runArgs = append(runArgs, manifest.DockerImage)
	return runArgs
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func lookPath(name string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		p := dir + "/" + name
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func execInstallCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// hostPortInUse reports whether some other process on the host already
// holds a TCP listen on 127.0.0.1:port. We use this to skip the
// best-effort pre-forward of common ports in `dew app run`, so the user's
// concurrent dev servers don't break VM boot.
func hostPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}
