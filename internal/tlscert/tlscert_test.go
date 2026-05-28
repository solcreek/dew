package tlscert

import (
	"crypto/tls"
	"os"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	pair, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(pair.CertFile); err != nil {
		t.Errorf("cert file not created: %v", err)
	}
	if _, err := os.Stat(pair.KeyFile); err != nil {
		t.Errorf("key file not created: %v", err)
	}
	if !strings.HasPrefix(pair.Fingerprint, "sha256:") {
		t.Errorf("fingerprint should start with sha256:, got %s", pair.Fingerprint)
	}

	_, err = tls.LoadX509KeyPair(pair.CertFile, pair.KeyFile)
	if err != nil {
		t.Errorf("cert/key pair invalid: %v", err)
	}
}

func TestGenerateIdempotent(t *testing.T) {
	dir := t.TempDir()
	p1, _ := Generate(dir)
	p2, _ := Generate(dir)
	if p1.Fingerprint != p2.Fingerprint {
		t.Error("second call should reuse existing cert")
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	pair, _ := Generate(dir)
	fp, err := Fingerprint(pair.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if fp != pair.Fingerprint {
		t.Errorf("fingerprint mismatch: %s != %s", fp, pair.Fingerprint)
	}
}
