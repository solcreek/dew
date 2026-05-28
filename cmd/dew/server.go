//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/solcreek/capstan"
	"github.com/solcreek/dew/internal/progress"
)

func cmdServer(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: dew server <create|list|destroy> [flags]\n")
		return nil
	}

	switch args[0] {
	case "create":
		return cmdServerCreate(args[1:])
	case "list":
		return cmdServerList(args[1:])
	case "destroy":
		return cmdServerDestroy(args[1:])
	default:
		return fmt.Errorf("unknown server subcommand %q (use: create, list, destroy)", args[0])
	}
}

func cmdServerCreate(args []string) error {
	var provider, region, plan, name string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			i++
			if i < len(args) {
				provider = args[i]
			}
		case "--region":
			i++
			if i < len(args) {
				region = args[i]
			}
		case "--plan":
			i++
			if i < len(args) {
				plan = args[i]
			}
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		}
	}

	if provider == "" {
		return fmt.Errorf("--provider is required (hetzner, digitalocean, linode, vultr)")
	}

	providerName := capstan.ProviderName(provider)
	spec := capstan.Spec(providerName)
	if spec == nil {
		return fmt.Errorf("unknown provider %q (supported: hetzner, digitalocean, linode, vultr)", provider)
	}

	token, err := loadProviderToken(providerName)
	if err != nil {
		return err
	}

	if name == "" {
		name = fmt.Sprintf("dew-%s", randomHex(4))
	}

	if region == "" {
		region = defaultRegion(providerName)
	}
	if plan == "" {
		plan = defaultPlan(providerName)
	}

	dewToken, err := generateDewToken()
	if err != nil {
		return fmt.Errorf("generate deploy token: %w", err)
	}

	cloudInit := generateCloudInit(dewToken)

	sp := progress.New()
	sp.Step("Creating VPS")

	p := newProvider(providerName, token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	srv, err := p.Create(ctx, capstan.CreateOpts{
		Name:     name,
		Plan:     plan,
		Region:   region,
		Image:    "debian-12",
		UserData: cloudInit,
	})
	if err != nil {
		sp.Fail(err.Error())
		return err
	}

	ip := srv.PublicIPv4
	if ip == "" {
		sp.Step("Waiting for IP")
		for i := 0; i < 30; i++ {
			time.Sleep(2 * time.Second)
			s, err := p.Get(ctx, srv.ID)
			if err == nil && s.PublicIPv4 != "" {
				ip = s.PublicIPv4
				break
			}
		}
		if ip == "" {
			sp.Fail("no IPv4 assigned")
			return fmt.Errorf("server %s created but no IPv4 address assigned", srv.ID)
		}
	}

	sp.Step("Waiting for dew serve")
	healthURL := fmt.Sprintf("http://%s:9080/v1/system/health", ip)
	ready := false
	for i := 0; i < 60; i++ {
		time.Sleep(3 * time.Second)
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
	}
	if !ready {
		sp.Timeout(ip)
		fmt.Fprintf(os.Stderr, "  cloud-init may still be running.\n")
		fmt.Fprintf(os.Stderr, "  check: curl %s\n\n", healthURL)
	}

	if err := saveCredentials(ip, dewToken); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not save credentials: %v\n", err)
	}
	if err := saveServer(providerName, srv.ID, name, ip, region, plan); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: could not save server info: %v\n", err)
	}

	if ready {
		sp.Done(ip)
	}
	fmt.Fprintf(os.Stderr, "  Token: %s...%s\n", dewToken[:14], dewToken[len(dewToken)-4:])
	fmt.Fprintf(os.Stderr, "  Deploy: dew deploy %s\n\n", ip)

	if flagJSON {
		fmt.Printf(`{"ok":true,"ip":"%s","provider":"%s","region":"%s","plan":"%s","name":"%s","id":"%s"}%s`,
			ip, provider, region, plan, name, srv.ID, "\n")
	}

	return nil
}

func cmdServerList(args []string) error {
	servers, err := loadServers()
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		fmt.Println("No servers.")
		return nil
	}
	fmt.Printf("%-20s %-15s %-12s %-8s %-10s\n", "NAME", "IP", "PROVIDER", "REGION", "PLAN")
	for _, s := range servers {
		fmt.Printf("%-20s %-15s %-12s %-8s %-10s\n", s.Name, s.IP, s.Provider, s.Region, s.Plan)
	}
	return nil
}

func cmdServerDestroy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew server destroy <name-or-ip>")
	}
	target := args[0]

	servers, err := loadServers()
	if err != nil {
		return err
	}

	var found *serverRecord
	for i := range servers {
		if servers[i].Name == target || servers[i].IP == target {
			found = &servers[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("server %q not found (use: dew server list)", target)
	}

	token, err := loadProviderToken(capstan.ProviderName(found.Provider))
	if err != nil {
		return err
	}

	p := newProvider(capstan.ProviderName(found.Provider), token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.Destroy(ctx, found.ID); err != nil {
		return fmt.Errorf("destroy %s: %w", found.Name, err)
	}

	removeServer(found.IP)
	removeCredentials(found.IP)

	fmt.Fprintf(os.Stderr, "  Server %s (%s) destroyed.\n", found.Name, found.IP)
	return nil
}

func newProvider(name capstan.ProviderName, token string) capstan.Provider {
	switch name {
	case capstan.Hetzner:
		return capstan.NewHetzner(token)
	case capstan.DigitalOcean:
		return capstan.NewDigitalOcean(token)
	case capstan.Linode:
		return capstan.NewLinode(token)
	case capstan.Vultr:
		return capstan.NewVultr(token)
	default:
		return nil
	}
}

func defaultRegion(name capstan.ProviderName) string {
	switch name {
	case capstan.Hetzner:
		return "ash"
	case capstan.DigitalOcean:
		return "nyc1"
	case capstan.Linode:
		return "us-east"
	case capstan.Vultr:
		return "ewr"
	default:
		return ""
	}
}

func defaultPlan(name capstan.ProviderName) string {
	switch name {
	case capstan.Hetzner:
		return "cx22"
	case capstan.DigitalOcean:
		return "s-1vcpu-1gb"
	case capstan.Linode:
		return "g6-nanode-1"
	case capstan.Vultr:
		return "vc2-1c-1gb"
	default:
		return ""
	}
}

func generateDewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "crk_admin_" + hex.EncodeToString(b), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func generateCloudInit(token string) string {
	tokenHash := hashToken(token)
	return fmt.Sprintf(`#!/bin/bash
set -e

# Install dew
curl -fsSL https://dewvm.dev/install.sh -o /tmp/install-dew.sh
bash /tmp/install-dew.sh

# Store token hash (not plaintext — provider metadata API can read user-data)
mkdir -p /var/dew
echo '%s' > /var/dew/token-hash

# Install containerd
CONTAINERD_VERSION="2.1.1"
RUNC_VERSION="1.2.6"
curl -fsSL -o /tmp/containerd.tar.gz \
  "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-static-${CONTAINERD_VERSION}-linux-amd64.tar.gz"
tar xzf /tmp/containerd.tar.gz -C /usr/local/
curl -fsSL -o /usr/local/bin/runc \
  "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.amd64"
chmod 755 /usr/local/bin/runc

# Pull base runtime image
mkdir -p /run/containerd /var/lib/containerd /etc/containerd
cat > /etc/containerd/config.toml << 'CONF'
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
CONF
containerd &
sleep 3

# Start dew serve
cat > /etc/systemd/system/dew-serve.service << 'SVC'
[Unit]
Description=Dew Serve
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dew serve
Restart=always
RestartSec=5
Environment=DEW_TOKEN_FILE=/var/dew/token

[Install]
WantedBy=multi-user.target
SVC

systemctl daemon-reload
systemctl enable --now dew-serve
`, tokenHash)
}

// ─── Credential storage ────────────────────────────────────────────

func dewConfigDir() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "dew")
	os.MkdirAll(dir, 0700)
	return dir
}

// ─── Credential storage (JSON) ─────────────────────────────────────

type credentialStore struct {
	Credentials  map[string]string `json:"credentials"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

func loadCredentialStore() *credentialStore {
	path := filepath.Join(dewConfigDir(), "credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return &credentialStore{Credentials: make(map[string]string)}
	}
	var store credentialStore
	if json.Unmarshal(data, &store) != nil || store.Credentials == nil {
		return &credentialStore{Credentials: make(map[string]string), Fingerprints: make(map[string]string)}
	}
	if store.Fingerprints == nil {
		store.Fingerprints = make(map[string]string)
	}
	return &store
}

func (s *credentialStore) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dewConfigDir(), "credentials.json"), data, 0600)
}

func saveCredentials(host, token string) error {
	store := loadCredentialStore()
	store.Credentials[host] = token
	return store.save()
}

func removeCredentials(host string) {
	store := loadCredentialStore()
	delete(store.Credentials, host)
	store.save()
}

// ─── Server record storage (JSON) ───────────────────────────────────

type serverRecord struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Region   string `json:"region"`
	Plan     string `json:"plan"`
}

type serverStore struct {
	Servers []serverRecord `json:"servers"`
}

func loadServerStore() *serverStore {
	path := filepath.Join(dewConfigDir(), "servers.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return &serverStore{}
	}
	var store serverStore
	json.Unmarshal(data, &store)
	return &store
}

func (s *serverStore) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dewConfigDir(), "servers.json"), data, 0600)
}

func saveServer(provider capstan.ProviderName, id, name, ip, region, plan string) error {
	store := loadServerStore()
	store.Servers = append(store.Servers, serverRecord{
		Provider: string(provider), ID: id, Name: name, IP: ip, Region: region, Plan: plan,
	})
	return store.save()
}

func loadServers() ([]serverRecord, error) {
	return loadServerStore().Servers, nil
}

func removeServer(ip string) {
	store := loadServerStore()
	var kept []serverRecord
	for _, s := range store.Servers {
		if s.IP != ip {
			kept = append(kept, s)
		}
	}
	store.Servers = kept
	store.save()
}

func loadProviderToken(name capstan.ProviderName) (string, error) {
	envKeys := map[capstan.ProviderName]string{
		capstan.Hetzner:      "HETZNER_API_TOKEN",
		capstan.DigitalOcean: "DIGITALOCEAN_TOKEN",
		capstan.Linode:       "LINODE_TOKEN",
		capstan.Vultr:        "VULTR_API_KEY",
	}

	envKey := envKeys[name]
	if v := os.Getenv(envKey); v != "" {
		return v, nil
	}

	path := filepath.Join(dewConfigDir(), "providers.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var providers map[string]string
		if json.Unmarshal(data, &providers) == nil {
			if t, ok := providers[string(name)]; ok {
				return t, nil
			}
		}
	}

	return "", fmt.Errorf("no API token for %s\nSet %s or run: dew provider auth %s", name, envKey, name)
}
