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
		return cmdInstallList()
	}

	appName := args[0]
	hostPort := 0
	for i := 1; i < len(args); i++ {
		if (args[i] == "--port" || args[i] == "-p") && i+1 < len(args) {
			i++
			fmt.Sscanf(args[i], "%d", &hostPort)
		}
	}
	sp := progress.New()

	sp.Step("Fetching manifest")
	manifest, err := fetchManifest(appName)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}

	fmt.Fprintf(os.Stderr, "\n  %s v%s\n", manifest.Name, manifest.Version)
	fmt.Fprintf(os.Stderr, "  %s\n", manifest.Description)
	fmt.Fprintf(os.Stderr, "  Port: %d | Runtime: %s\n\n", manifest.Port, manifest.Runtime)

	if manifest.DockerImage != "" {
		exposedPort := manifest.Port
		if hostPort > 0 {
			exposedPort = hostPort
		}
		containerName := "dew-" + manifest.Name

		// Ensure Dew VM is running with containerd
		sp.Step("Preparing VM")
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
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				sp.Fail("start failed")
				return fmt.Errorf("failed to start %s: %w", manifest.Name, err)
			}
		} else {
			// Run inside Dew VM
			sp.Step(fmt.Sprintf("Starting %s", manifest.Name))
			dewExec := exec.Command(os.Args[0], "exec",
				fmt.Sprintf("export TMPDIR=/tmp/containerd-tmp && mkdir -p /tmp/containerd-tmp && chmod 1777 /tmp/containerd-tmp && nerdctl rm -f %s 2>/dev/null; nerdctl run -d --name %s -p %d:%d %s",
					containerName, containerName, manifest.Port, manifest.Port, manifest.DockerImage))
			dewExec.Stdout = os.Stderr
			dewExec.Stderr = os.Stderr
			if err := dewExec.Run(); err != nil {
				sp.Fail("start failed in VM")
				return fmt.Errorf("failed to start %s in VM: %w", manifest.Name, err)
			}
		}

		if manifest.HealthCheck != "" {
			sp.Step("Waiting for healthy")
			url := fmt.Sprintf("http://localhost:%d%s", exposedPort, manifest.HealthCheck)
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				resp, err := http.Get(url)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 400 {
						break
					}
				}
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
	// Check if dew VM daemon socket exists (VM already running)
	home, _ := os.UserHomeDir()
	sock := filepath.Join(home, ".local", "state", "dew", "default.sock")
	if _, err := os.Stat(sock); err == nil {
		// VM running, check if port is forwarded
		return nil
	}

	// Start VM in background with port forwarding
	cmd := exec.Command(os.Args[0], "start",
		"--profile", "standard",
		"--forward", fmt.Sprintf("%d:%d", hostPort, guestPort),
		"--network",
		"--json",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// Wait for VM to be ready
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
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
