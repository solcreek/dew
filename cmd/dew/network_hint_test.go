//go:build darwin

package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/vm"
)

// captureStderrString runs fn with os.Stderr redirected to a pipe.
func captureStderrString(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	done := make(chan struct{})
	var out []byte
	go func() {
		out, _ = io.ReadAll(r)
		close(done)
	}()
	fn()
	w.Close()
	os.Stderr = orig
	<-done
	return string(out)
}

// The fresh-eyes agent report flagged restricted-mode failures as
// silent: apk add exits -1 with no error output, the user has no
// idea why. The hint added in appendGuestParams surfaces the most
// common cause (forgot --allow-host) before the VM boots, so the
// user can interrupt and retry without spending 30s on a doomed
// install.
func TestAppendGuestParams_RestrictedWithoutAllowHostWarns(t *testing.T) {
	cfg := vm.Config{
		Network:       true,
		NetworkPolicy: "restricted",
		// No AllowHosts — the documented foot-gun.
	}
	out := captureStderrString(t, func() {
		appendGuestParams(&cfg)
	})

	for _, needle := range []string{
		"restricted",
		"--allow-host",
		"blocked",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("warning missing %q\nfull stderr:\n%s", needle, out)
		}
	}
	// The hint must point at the escape hatch (--network-policy=open),
	// otherwise users with a legitimate need to run unrestricted feel
	// stuck.
	if !strings.Contains(out, "open") {
		t.Errorf("warning should mention the --network-policy=open escape hatch:\n%s", out)
	}
}

// Restricted with allow-hosts is the working configuration —
// no warning needed.
func TestAppendGuestParams_RestrictedWithAllowHostQuiet(t *testing.T) {
	cfg := vm.Config{
		Network:       true,
		NetworkPolicy: "restricted",
		AllowHosts:    []string{"pkgs.alpinelinux.org"},
	}
	out := captureStderrString(t, func() {
		appendGuestParams(&cfg)
	})
	if strings.Contains(out, "blocked") {
		t.Errorf("no warning expected when allow-host is set; got:\n%s", out)
	}
}

// Open policy doesn't warn either — it's the unrestricted default
// once --network or --network-policy=open is on.
func TestAppendGuestParams_OpenPolicyQuiet(t *testing.T) {
	cfg := vm.Config{
		Network:       true,
		NetworkPolicy: "open",
	}
	out := captureStderrString(t, func() {
		appendGuestParams(&cfg)
	})
	if strings.Contains(out, "restricted") || strings.Contains(out, "blocked") {
		t.Errorf("open policy should be silent; got:\n%s", out)
	}
}
