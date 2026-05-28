//go:build darwin

package main

import (
	"strings"
	"testing"

	"github.com/solcreek/capstan"
)

func TestGenerateDewToken(t *testing.T) {
	token, err := generateDewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "crk_admin_") {
		t.Errorf("token should start with crk_admin_, got %q", token)
	}
	if len(token) != len("crk_admin_")+48 {
		t.Errorf("token length = %d, want %d (prefix + 48 hex chars)", len(token), len("crk_admin_")+48)
	}

	token2, _ := generateDewToken()
	if token == token2 {
		t.Error("two generated tokens should be different")
	}
}

func TestGenerateCloudInit(t *testing.T) {
	ci := generateCloudInit("crk_admin_test123")
	if strings.Contains(ci, "crk_admin_test123") {
		t.Error("cloud-init must NOT contain plaintext token")
	}
	expectedHash := hashToken("crk_admin_test123")
	if !strings.Contains(ci, expectedHash) {
		t.Error("cloud-init should contain token hash")
	}
	if !strings.Contains(ci, "token-hash") {
		t.Error("cloud-init should write to token-hash file")
	}
	if !strings.Contains(ci, "containerd") {
		t.Error("cloud-init should install containerd")
	}
	if !strings.Contains(ci, "dew-serve.service") {
		t.Error("cloud-init should create systemd service")
	}
}

func TestDefaultRegion(t *testing.T) {
	tests := []struct {
		provider capstan.ProviderName
		want     string
	}{
		{capstan.Hetzner, "ash"},
		{capstan.DigitalOcean, "nyc1"},
		{capstan.Linode, "us-east"},
		{capstan.Vultr, "ewr"},
	}
	for _, tt := range tests {
		got := defaultRegion(tt.provider)
		if got != tt.want {
			t.Errorf("defaultRegion(%s) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

func TestDefaultPlan(t *testing.T) {
	tests := []struct {
		provider capstan.ProviderName
		want     string
	}{
		{capstan.Hetzner, "cx22"},
		{capstan.DigitalOcean, "s-1vcpu-1gb"},
		{capstan.Linode, "g6-nanode-1"},
		{capstan.Vultr, "vc2-1c-1gb"},
	}
	for _, tt := range tests {
		got := defaultPlan(tt.provider)
		if got != tt.want {
			t.Errorf("defaultPlan(%s) = %q, want %q", tt.provider, got, tt.want)
		}
	}
}

func TestRandomHex(t *testing.T) {
	h := randomHex(4)
	if len(h) != 8 {
		t.Errorf("randomHex(4) length = %d, want 8", len(h))
	}
	h2 := randomHex(4)
	if h == h2 {
		t.Error("two randomHex calls should differ")
	}
}
