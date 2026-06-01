//go:build darwin

package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/solcreek/dew/internal/progress"
	"github.com/solcreek/dew/internal/validate"
	"github.com/solcreek/dew/pkg/dewerr"
)

func cmdDeploy(args []string) error {
	var target, tarballPath, imageName, appName string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--image":
			i++
			if i < len(args) {
				imageName = args[i]
			}
		case "--app":
			i++
			if i < len(args) {
				appName = args[i]
			}
		case "--tarball":
			i++
			if i < len(args) {
				tarballPath = args[i]
			}
		case "--dry-run":
			flagDryRun = true
		default:
			if !strings.HasPrefix(args[i], "-") {
				if target == "" {
					target = args[i]
				}
			}
		}
	}

	if target == "" {
		return dewerr.New(dewerr.CodeUsage, "usage: dew deploy <target> [--tarball <path>] [--image <name>] [--app <name>] [--dry-run]")
	}

	if err := validate.Target(target); err != nil {
		return err
	}

	token, err := loadDeployToken(target)
	if err != nil {
		return err
	}

	endpoint := resolveEndpoint(target)

	if flagDryRun {
		fmt.Fprintf(os.Stderr, "  Dry run:\n")
		if imageName != "" {
			fmt.Fprintf(os.Stderr, "  Would deploy image %s to %s\n", imageName, endpoint)
		} else {
			t := tarballPath
			if t == "" { t = "(auto-detect)" }
			fmt.Fprintf(os.Stderr, "  Would deploy %s to %s\n", t, endpoint)
		}
		fmt.Fprintf(os.Stderr, "  No changes made.\n")
		return nil
	}

	if imageName != "" {
		return deployImage(target, endpoint, token, imageName, appName)
	}

	if tarballPath == "" {
		candidates := []string{
			filepath.Base(mustAbs(".")) + ".tar.gz",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				tarballPath = c
				break
			}
		}
		if tarballPath == "" {
			return dewerr.New(dewerr.CodeNotFound, "no tarball found. Run: dew build")
		}
	}

	return deployTarball(target, endpoint, token, tarballPath, appName)
}

func deployTarball(target, endpoint, token, tarballPath, appName string) error {
	sp := progress.New()

	sp.Step("Reading tarball")
	f, err := os.Open(tarballPath)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}
	defer f.Close()

	stat, _ := f.Stat()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	f.Seek(0, 0)

	if appName == "" {
		appName = inferAppName(tarballPath)
	}

	sp.Step(fmt.Sprintf("Uploading %s (%s)", tarballPath, humanSize(stat.Size())))
	url := fmt.Sprintf("%s/v1/apps/%s/deploy", endpoint, appName)

	req, err := http.NewRequest("POST", url, f)
	if err != nil {
		sp.Fail(err.Error())
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	req.Header.Set("X-Deploy-Checksum", "sha256:"+checksum)
	req.ContentLength = stat.Size()

	client := makeHTTPClient(target)
	resp, err := client.Do(req)
	if err != nil {
		sp.Fail("upload failed")
		return dewerr.Wrapf(err, dewerr.CodeNetwork, "deploy to %s", endpoint)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		sp.Fail("unauthorized")
		return dewerr.Newf(dewerr.CodeAuth, "invalid deploy token for %s", endpoint)
	}
	if resp.StatusCode == 409 {
		sp.Fail("deploy in progress")
		return dewerr.Newf(dewerr.CodeConflict, "another deploy is already running on %s", endpoint)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		sp.Fail(fmt.Sprintf("HTTP %d", resp.StatusCode))
		code := dewerr.CodeGeneric
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			code = dewerr.CodeUnavailable
		} else if resp.StatusCode >= 500 {
			code = dewerr.CodeUnavailable
		}
		return dewerr.Newf(code, "deploy failed: %d %s", resp.StatusCode, body)
	}

	if resp.Header.Get("Content-Type") == "text/event-stream" {
		return streamDeployEvents(sp, resp.Body, appName, endpoint)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	sp.Done(fmt.Sprintf("%s deployed", appName))

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(result)
	}
	return nil
}

func deployImage(target, endpoint, token, imageName, appName string) error {
	sp := progress.New()

	if appName == "" {
		parts := strings.Split(imageName, "/")
		appName = strings.Split(parts[len(parts)-1], ":")[0]
	}

	sp.Step(fmt.Sprintf("Deploying image %s", imageName))
	url := fmt.Sprintf("%s/v1/apps/%s/deploy", endpoint, appName)

	body := fmt.Sprintf(`{"image":"%s"}`, imageName)
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		sp.Fail(err.Error())
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := makeHTTPClient(target)
	resp, err := client.Do(req)
	if err != nil {
		sp.Fail("request failed")
		return dewerr.Wrapf(err, dewerr.CodeNetwork, "deploy to %s", endpoint)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		sp.Fail("unauthorized")
		return dewerr.Newf(dewerr.CodeAuth, "invalid deploy token for %s", endpoint)
	}
	if resp.StatusCode == 409 {
		sp.Fail("deploy in progress")
		return dewerr.Newf(dewerr.CodeConflict, "another deploy is already running on %s", endpoint)
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		sp.Fail(fmt.Sprintf("HTTP %d", resp.StatusCode))
		code := dewerr.CodeGeneric
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			code = dewerr.CodeUnavailable
		}
		return dewerr.Newf(code, "deploy failed: %d %s", resp.StatusCode, respBody)
	}

	if resp.Header.Get("Content-Type") == "text/event-stream" {
		return streamDeployEvents(sp, resp.Body, appName, endpoint)
	}

	sp.Done(fmt.Sprintf("%s → %s", imageName, appName))
	if flagJSON {
		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		json.NewEncoder(os.Stdout).Encode(result)
	}
	return nil
}

func streamDeployEvents(sp *progress.Spinner, body io.Reader, appName, endpoint string) error {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		var event struct {
			Phase  string `json:"phase"`
			Status string `json:"status"`
			Error  string `json:"error"`
			URL    string `json:"url"`
			OK     bool   `json:"ok"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Status {
		case "done":
			sp.Step(event.Phase)
		case "fail":
			sp.Fail(event.Error)
			return dewerr.Newf(dewerr.CodeGeneric, "deploy failed at %s: %s", event.Phase, event.Error)
		}

		if event.OK {
			url := event.URL
			if url == "" {
				url = endpoint
			}
			sp.Done(url)
			fmt.Fprintf(os.Stderr, "  %s deployed to %s\n\n", appName, url)
		}

		if flagJSON {
			fmt.Println(data)
		}
	}
	return scanner.Err()
}

func loadDeployToken(target string) (string, error) {
	if v := os.Getenv("CREEK_TOKEN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("DEW_TOKEN"); v != "" {
		return v, nil
	}

	store := loadCredentialStore()
	if t, ok := store.Credentials[target]; ok {
		return t, nil
	}
	for host, t := range store.Credentials {
		if strings.HasPrefix(target, host) {
			return t, nil
		}
	}

	return "", dewerr.Newf(dewerr.CodeAuth, "no deploy token for %s\nSet DEW_TOKEN env var or run: dew auth set %s <token>", target, target)
}

func resolveEndpoint(target string) string {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return strings.TrimSuffix(target, "/")
	}
	return "https://" + target + ":9080"
}

func makeHTTPClient(target string) *http.Client {
	store := loadCredentialStore()
	_ = store // fingerprint pinning will use this

	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return fmt.Errorf("no server certificate")
					}
					cert := cs.PeerCertificates[0]
					h := sha256.Sum256(cert.Raw)
					fp := "sha256:" + formatFingerprint(hex.EncodeToString(h[:]))

					store := loadCredentialStore()
					if stored, ok := store.Fingerprints[target]; ok {
						if stored != fp {
							return fmt.Errorf("certificate fingerprint mismatch: expected %s, got %s", stored, fp)
						}
					}
					return nil
				},
			},
		},
	}
}

func formatFingerprint(hexStr string) string {
	parts := make([]string, 0, 32)
	for i := 0; i < len(hexStr); i += 2 {
		parts = append(parts, hexStr[i:i+2])
	}
	return strings.Join(parts, ":")
}

func inferAppName(tarballPath string) string {
	base := filepath.Base(tarballPath)
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	return base
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
