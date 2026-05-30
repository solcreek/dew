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
	// Strip --json out of args once at the dispatcher level so each subcommand
	// reads clean positionals. flagJSON is package-scoped (set in main.go's
	// top-level scan too) so subcommands check it directly.
	args = stripJSONFlag(args)

	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: dew server <create|list|destroy|start|stop|restart|status> [flags]\n")
		return nil
	}

	switch args[0] {
	case "create":
		return cmdServerCreate(args[1:])
	case "list":
		return cmdServerList(args[1:])
	case "destroy":
		return cmdServerDestroy(args[1:])
	case "start":
		return cmdServerStart(args[1:])
	case "stop":
		return cmdServerStop(args[1:])
	case "restart":
		return cmdServerRestart(args[1:])
	case "status":
		return cmdServerStatus(args[1:])
	default:
		return fmt.Errorf("unknown server subcommand %q (use: create, list, destroy, start, stop, restart, status)", args[0])
	}
}

// stripJSONFlag removes --json from args (anywhere) and sets the package
// flagJSON. Subcommands then read clean positionals + check flagJSON for
// the output mode. Multiple --json passes are idempotent.
func stripJSONFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			flagJSON = true
			continue
		}
		out = append(out, a)
	}
	return out
}

// emitJSON writes a structured success payload to stdout when --json mode is
// active. The shape is always {"ok": true, ...payload}; callers pass payload
// as a map. Encoder is shared so injection-safe in names / fields.
func emitJSON(payload map[string]any) error {
	payload["ok"] = true
	return json.NewEncoder(os.Stdout).Encode(payload)
}

// serverJSON converts a capstan.Server into the JSON shape Marina (and any
// other agent consumer) sees. Fields are stable and matched against
// project_capstan_dew_boundary memory.
func serverJSON(srv *capstan.Server) map[string]any {
	out := map[string]any{
		"id":         srv.ID,
		"name":       srv.Name,
		"status":     string(srv.Status),
		"publicIPv4": srv.PublicIPv4,
		"region":     srv.Region,
		"plan":       srv.Plan,
		"createdAt":  srv.CreatedAt,
	}
	if srv.PublicIPv6 != "" {
		out["publicIPv6"] = srv.PublicIPv6
	}
	return out
}

// recordJSON converts a local serverRecord (registry entry) into JSON.
func recordJSON(r serverRecord) map[string]any {
	return map[string]any{
		"id":       r.ID,
		"name":     r.Name,
		"ip":       r.IP,
		"provider": r.Provider,
		"region":   r.Region,
		"plan":     r.Plan,
	}
}

// targetServer bundles the local registry record with a live capstan
// Provider that's already authenticated. Used by every subcommand that
// acts on an existing server (destroy / start / stop / restart / status)
// to keep the lookup + token + provider construction in one place.
type targetServer struct {
	rec serverRecord
	p   capstan.Provider
}

func lookupAndConnect(target string) (*targetServer, error) {
	servers, err := loadServers()
	if err != nil {
		return nil, err
	}
	for i := range servers {
		if servers[i].Name == target || servers[i].IP == target {
			rec := servers[i]
			token, err := loadProviderToken(capstan.ProviderName(rec.Provider))
			if err != nil {
				return nil, err
			}
			p, err := capstan.New(capstan.ProviderName(rec.Provider), token)
			if err != nil {
				return nil, err
			}
			return &targetServer{rec: rec, p: p}, nil
		}
	}
	return nil, fmt.Errorf("server %q not found (use: dew server list)", target)
}

// runAction handles the common pattern: submit a power action, wait for
// the provider to mark it terminal, print the outcome line. Matches the
// destroy command's plain-text style — power actions don't produce a URL
// the way create does, so the spinner Done(url) shape doesn't fit.
// PowerOff on Hetzner typically takes ~10s end-to-end per bench data;
// the user sees the "Verbing..." line and then the result.
func runAction(
	ctx context.Context,
	t *targetServer,
	verbing, doneLabel, actionName string,
	fn func(ctx context.Context, id string) (*capstan.Action, error),
) error {
	if !flagJSON {
		fmt.Fprintf(os.Stderr, "  %s %s...\n", verbing, t.rec.Name)
	}

	action, err := fn(ctx, t.rec.ID)
	if err != nil {
		return err
	}
	if _, err := t.p.WaitForAction(ctx, action.ID); err != nil {
		return err
	}

	if flagJSON {
		return emitJSON(map[string]any{
			"action": actionName,
			"server": map[string]any{
				"id":   t.rec.ID,
				"name": t.rec.Name,
				"ip":   t.rec.IP,
			},
		})
	}
	fmt.Fprintf(os.Stderr, "  Server %s (%s) %s.\n", t.rec.Name, t.rec.IP, doneLabel)
	return nil
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

	p, err := capstan.New(providerName, token)
	if err != nil {
		return err
	}
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
		return emitJSON(map[string]any{
			"server": map[string]any{
				"id":       srv.ID,
				"name":     name,
				"ip":       ip,
				"provider": provider,
				"region":   region,
				"plan":     plan,
			},
		})
	}

	return nil
}

func cmdServerList(args []string) error {
	servers, err := loadServers()
	if err != nil {
		return err
	}
	if flagJSON {
		out := make([]map[string]any, 0, len(servers))
		for _, s := range servers {
			out = append(out, recordJSON(s))
		}
		return emitJSON(map[string]any{"servers": out})
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
	t, err := lookupAndConnect(args[0])
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := t.p.Destroy(ctx, t.rec.ID); err != nil {
		return fmt.Errorf("destroy %s: %w", t.rec.Name, err)
	}
	removeServer(t.rec.IP)
	removeCredentials(t.rec.IP)
	if flagJSON {
		return emitJSON(map[string]any{
			"destroyed": map[string]any{"name": t.rec.Name, "ip": t.rec.IP, "id": t.rec.ID},
		})
	}
	fmt.Fprintf(os.Stderr, "  Server %s (%s) destroyed.\n", t.rec.Name, t.rec.IP)
	return nil
}

func cmdServerStart(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew server start <name-or-ip>")
	}
	t, err := lookupAndConnect(args[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runAction(ctx, t, "Powering on", "running", "start", t.p.PowerOn)
}

func cmdServerStop(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew server stop <name-or-ip>")
	}
	t, err := lookupAndConnect(args[0])
	if err != nil {
		return err
	}
	// PowerOff is graceful ACPI shutdown; bench data shows it
	// typically takes ~10s end-to-end. 5min covers the long tail.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runAction(ctx, t, "Powering off", "stopped", "stop", t.p.PowerOff)
}

func cmdServerRestart(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew server restart <name-or-ip>")
	}
	t, err := lookupAndConnect(args[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runAction(ctx, t, "Rebooting", "rebooted", "restart", t.p.Restart)
}

func cmdServerStatus(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dew server status <name-or-ip>")
	}
	t, err := lookupAndConnect(args[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv, err := t.p.Get(ctx, t.rec.ID)
	if err != nil {
		return fmt.Errorf("get %s: %w", t.rec.Name, err)
	}

	if flagJSON {
		return emitJSON(map[string]any{"server": serverJSON(srv)})
	}

	fmt.Printf("%-12s %s\n", "Name:", srv.Name)
	fmt.Printf("%-12s %s\n", "ID:", srv.ID)
	fmt.Printf("%-12s %s\n", "Status:", srv.Status)
	fmt.Printf("%-12s %s\n", "IPv4:", srv.PublicIPv4)
	if srv.PublicIPv6 != "" {
		fmt.Printf("%-12s %s\n", "IPv6:", srv.PublicIPv6)
	}
	fmt.Printf("%-12s %s\n", "Region:", srv.Region)
	fmt.Printf("%-12s %s\n", "Plan:", srv.Plan)
	fmt.Printf("%-12s %s\n", "Created:", srv.CreatedAt)
	return nil
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
