//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/solcreek/dew/internal/progress"
	"github.com/solcreek/dew/internal/validate"
)

const registryBase = "https://raw.githubusercontent.com/solcreek/dew-apps/main"

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
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: dew app <run|stop|list> [args]\n")
		return nil
	}

	switch args[0] {
	case "run":
		if len(args) < 2 {
			return fmt.Errorf("usage: dew app run <name> [--port N]")
		}
		return cmdAppRun(args[1:])
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: dew app stop <name>")
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
	runtime := "docker"
	if _, err := lookPath("nerdctl"); err == nil {
		runtime = "nerdctl"
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
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i < len(args) {
				fmt.Sscanf(args[i], "%d", &hostPort)
			}
		case "--dry-run":
			flagDryRun = true
		}
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
		if hostPort > 0 { exposedPort = hostPort }
		fmt.Fprintf(os.Stderr, "  Dry run:\n")
		fmt.Fprintf(os.Stderr, "  Would pull %s\n", manifest.DockerImage)
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
		if err := ensureDewVM(exposedPort, manifest.Port); err != nil {
			// Fallback to host docker if VM unavailable
			sp.Step(fmt.Sprintf("Starting %s (host)", manifest.Name))
			runtime := "docker"
			if _, err := lookPath("nerdctl"); err == nil {
				runtime = "nerdctl"
			}
			exec.Command(runtime, "rm", "-f", containerName).Run()
			runArgs := buildRunArgs(containerName, exposedPort, manifest)
			cmd := exec.Command(runtime, runArgs...)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				sp.Fail("start failed")
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
				return fmt.Errorf("failed to start %s in VM: %w", manifest.Name, err)
			}
		}

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
				fmt.Fprintf(os.Stderr, "  Container started but http://localhost:%d%s is not responding.\n", exposedPort, manifest.HealthCheck)
				fmt.Fprintf(os.Stderr, "  The app may still be starting. Try opening the URL in a few seconds.\n\n")
				return nil
			}
		}

		sp.Done(fmt.Sprintf("http://localhost:%d", exposedPort))
		fmt.Fprintf(os.Stderr, "  %s is running at http://localhost:%d\n\n", manifest.Name, exposedPort)

		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]any{
				"ok":   true,
				"app":  manifest.Name,
				"port": exposedPort,
				"url":  fmt.Sprintf("http://localhost:%d", exposedPort),
			})
		}
		return nil
	}

	return fmt.Errorf("app %s has no docker_image — tarball install not yet implemented", appName)
}

func cmdInstallList() error {
	resp, err := http.Get(registryBase + "/registry.json")
	if err != nil {
		return fmt.Errorf("fetch registry: %w", err)
	}
	defer resp.Body.Close()

	var reg struct {
		Apps []string `json:"apps"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)

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
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("app %q not found in registry", name)
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

func ensureDewVM(hostPort, guestPort int) error {
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
	// Pre-forward common port range for subsequent apps
	for _, p := range []int{3000, 3001, 3002, 3003, 3004, 3005, 8080, 8000, 7456, 5230, 2368} {
		if p != hostPort {
			args = append(args, "--forward", fmt.Sprintf("%d:%d", p, p))
		}
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// Monitor for early child process death (VZ entitlement failures, etc.)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Wait for VM to be ready
	for i := 0; i < 60; i++ {
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
	}
	return fmt.Errorf("VM did not start within 60s")
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
