//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/solcreek/dew/internal/progress"
)

var tunnelURLPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func cmdShare(args []string) error {
	port := "3000"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i < len(args) {
				port = args[i]
			}
		default:
			if !strings.HasPrefix(args[i], "-") {
				port = args[i]
			}
		}
	}

	localURL := "http://localhost:" + port
	if err := checkPort(port); err != nil {
		return fmt.Errorf("nothing running on port %s", port)
	}

	cfPath, err := ensureCloudflared()
	if err != nil {
		return err
	}

	sp := progress.New()
	sp.Step("Starting tunnel")

	cmd := exec.Command(cfPath, "tunnel", "--url", localURL)
	cmd.Env = append(os.Environ(), "NO_AUTOUPDATE=1")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		sp.Fail("failed to start cloudflared")
		return fmt.Errorf("cloudflared: %w", err)
	}

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigC
		cmd.Process.Signal(syscall.SIGTERM)
	}()

	scanner := bufio.NewScanner(stderr)
	var publicURL string
	for scanner.Scan() {
		line := scanner.Text()
		if match := tunnelURLPattern.FindString(line); match != "" {
			publicURL = match
			break
		}
	}

	if publicURL == "" {
		sp.Fail("no tunnel URL received")
		cmd.Process.Kill()
		return fmt.Errorf("cloudflared did not return a tunnel URL")
	}

	sp.Done(publicURL)
	fmt.Fprintf(os.Stderr, "  localhost:%s → %s\n", port, publicURL)
	fmt.Fprintf(os.Stderr, "  Press Ctrl+C to stop\n\n")

	if flagJSON {
		fmt.Printf(`{"url":"%s","port":"%s"}%s`, publicURL, port, "\n")
	}

	cmd.Wait()
	return nil
}

func checkPort(port string) error {
	cmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"http://localhost:"+port+"/")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) == "000" {
		return fmt.Errorf("no response")
	}
	return nil
}

func ensureCloudflared() (string, error) {
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}

	dataDir := dewDataDir()
	localPath := dataDir + "/bin/cloudflared"
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	sp := progress.New()
	sp.Step("Downloading cloudflared")

	var url string
	switch {
	case isARM():
		url = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-arm64.tgz"
	default:
		url = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-amd64.tgz"
	}

	os.MkdirAll(dataDir+"/bin", 0755)

	cmd := exec.Command("sh", "-c",
		fmt.Sprintf("curl -fsSL '%s' | tar xz -C '%s/bin/' cloudflared", url, dataDir))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		sp.Fail("download failed")
		return "", fmt.Errorf("download cloudflared: %w", err)
	}
	os.Chmod(localPath, 0755)
	sp.Done("cloudflared")
	return localPath, nil
}

func isARM() bool {
	cmd := exec.Command("uname", "-m")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)) == "arm64"
}
