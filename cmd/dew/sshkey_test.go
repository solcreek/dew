//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Field-validated literal — what an agent would actually paste in.
const sampleKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyForTestsOnly user@host"

func TestResolveSSHKey_OptOut(t *testing.T) {
	k, src, err := resolveSSHKey("", true)
	if err != nil {
		t.Fatal(err)
	}
	if k != "" {
		t.Errorf("opt-out should yield empty key, got %q", k)
	}
	if !strings.Contains(src, "no-ssh-key") {
		t.Errorf("source should explain opt-out, got %q", src)
	}
}

func TestResolveSSHKey_FlagInlineLiteral(t *testing.T) {
	k, src, err := resolveSSHKey(sampleKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("key = %q, want %q", k, sampleKey)
	}
	if !strings.Contains(src, "literal") {
		t.Errorf("source = %q, want literal indication", src)
	}
}

func TestResolveSSHKey_FlagInlineLiteral_RejectsMalformed(t *testing.T) {
	_, _, err := resolveSSHKey("ssh-totallymalformed", false)
	if err == nil {
		t.Fatal("expected error on malformed inline key")
	}
}

func TestResolveSSHKey_FlagFilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_test.pub")
	if err := os.WriteFile(path, []byte(sampleKey+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	k, src, err := resolveSSHKey(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("key = %q, want %q", k, sampleKey)
	}
	if !strings.Contains(src, path) {
		t.Errorf("source = %q, want path", src)
	}
}

func TestResolveSSHKey_EnvLiteral(t *testing.T) {
	t.Setenv("DEW_SSH_KEY", sampleKey)
	t.Setenv("DEW_SSH_KEY_FILE", "")
	k, src, err := resolveSSHKey("", false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("key = %q, want %q", k, sampleKey)
	}
	if !strings.Contains(src, "DEW_SSH_KEY") || strings.Contains(src, "FILE") {
		t.Errorf("source = %q, want DEW_SSH_KEY env", src)
	}
}

func TestResolveSSHKey_EnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pub")
	if err := os.WriteFile(path, []byte(sampleKey), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEW_SSH_KEY", "")
	t.Setenv("DEW_SSH_KEY_FILE", path)
	k, src, err := resolveSSHKey("", false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("key = %q, want %q", k, sampleKey)
	}
	if !strings.Contains(src, "DEW_SSH_KEY_FILE") {
		t.Errorf("source = %q, want DEW_SSH_KEY_FILE", src)
	}
}

func TestResolveSSHKey_AutoDiscovery(t *testing.T) {
	// Point HOME at a temp dir with a planted ed25519 key.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, ".ssh", "id_ed25519.pub")
	if err := os.WriteFile(keyPath, []byte(sampleKey), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("DEW_SSH_KEY", "")
	t.Setenv("DEW_SSH_KEY_FILE", "")

	k, src, err := resolveSSHKey("", false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("auto-discovery missed planted key: %q", k)
	}
	if !strings.Contains(src, "auto-discovered") || !strings.Contains(src, "id_ed25519") {
		t.Errorf("source = %q, want auto-discovered id_ed25519", src)
	}
}

func TestResolveSSHKey_FlagBeatsEnv(t *testing.T) {
	t.Setenv("DEW_SSH_KEY", "ssh-rsa AAAAfromEnv user@host")
	k, src, err := resolveSSHKey(sampleKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if k != sampleKey {
		t.Errorf("flag should win over env; got %q", k)
	}
	if !strings.Contains(src, "literal") {
		t.Errorf("source = %q, want flag literal", src)
	}
}

func TestResolveSSHKey_NoneAvailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DEW_SSH_KEY", "")
	t.Setenv("DEW_SSH_KEY_FILE", "")
	k, src, err := resolveSSHKey("", false)
	if err != nil {
		t.Fatal(err)
	}
	if k != "" {
		t.Errorf("expected empty key, got %q", k)
	}
	if src != "none" {
		t.Errorf("source = %q, want \"none\"", src)
	}
}
