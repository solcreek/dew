//go:build darwin

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/solcreek/dew/internal/progress"
)

// emitShareEvent writes a single NDJSON event line to stdout when
// --events is set. Each event is a self-contained JSON object on
// one line, terminated by \n. Consumers parse line-by-line.
//
// Event contract — see docs/exit-codes.md (and corresponding doc
// at share-events.md once it lands). Five events fire in order:
//
//	starting        — share invoked; port validated
//	tunnel-url      — public URL obtained from the tunnel impl
//	established     — HTTP probe confirmed traffic reaches the app
//	probe-timeout   — probe loop ended without 2xx/3xx
//	                  (tunnel may still come up; tunnel-url still valid)
//	closed          — tunnel process exited / signal received
//
// Schema is technology-agnostic — events stay valid if we swap
// cloudflared for our own tunnel implementation later. Fields:
//
//	event   string         — one of the names above
//	ts      string (RFC3339Nano)
//	url     string         — present from tunnel-url onward
//	port    string         — present from tunnel-url onward
//	reason  string         — present on closed (optional)
//
// New fields may be added in minor versions; existing fields are
// stable. Consumers should ignore unknown fields.
func emitShareEvent(event string, fields map[string]any) {
	if !flagEvents {
		return
	}
	e := map[string]any{
		"event": event,
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range fields {
		e[k] = v
	}
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

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

	emitShareEvent("starting", map[string]any{"port": port})

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
		emitShareEvent("closed", map[string]any{"reason": "no_tunnel_url"})
		return fmt.Errorf("cloudflared did not return a tunnel URL")
	}

	emitShareEvent("tunnel-url", map[string]any{"url": publicURL, "port": port})

	sp.Step("Verifying tunnel")
	ready := false
	for i := 0; i < 20; i++ {
		time.Sleep(3 * time.Second)
		resp, err := http.Get(publicURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				ready = true
				break
			}
		}
	}
	if !ready {
		sp.Timeout(publicURL)
		fmt.Fprintf(os.Stderr, "  Tunnel created but edge may still be connecting.\n")
		fmt.Fprintf(os.Stderr, "  Try opening the URL in a few seconds.\n\n")
		emitShareEvent("probe-timeout", map[string]any{"url": publicURL, "port": port})
	} else {
		sp.Done(publicURL)
		emitShareEvent("established", map[string]any{"url": publicURL, "port": port})
	}
	fmt.Fprintf(os.Stderr, "  localhost:%s → %s\n", port, publicURL)
	fmt.Fprintf(os.Stderr, "  Press Ctrl+C to stop\n\n")

	if flagJSON {
		fmt.Printf(`{"url":"%s","port":"%s"}%s`, publicURL, port, "\n")
	}

	cmd.Wait()
	emitShareEvent("closed", map[string]any{"reason": "tunnel-exited"})
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
