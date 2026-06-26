//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/ocistage"
)

// The overlay rootfs path is encoded in two languages: ocistage.GuestRootPath
// (Go, written into config.json's root.path on the host) and the dew-oci-run
// launcher's RUN=/var/lib/dew/oci/$NAME + $RUN/merged (bash, in build.sh). If
// they drift, crun gets a root.path the launcher never created and the
// container fails to start at boot with an opaque error. This guards the
// contract the way TestInitramfsBuildScript_* guards the modprobe allowlist.
func TestOCIOverlayPathContractMatchesLauncher(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)

	// The launcher must set RUN to the per-name base and mount <base>/merged.
	if !strings.Contains(script, `RUN="/var/lib/dew/oci/$NAME"`) {
		t.Error(`dew-oci-run no longer sets RUN="/var/lib/dew/oci/$NAME" — overlay path contract with ocistage.GuestRootPath is broken`)
	}
	if !strings.Contains(script, `"$RUN/merged"`) {
		t.Error(`dew-oci-run no longer mounts "$RUN/merged" — overlay path contract with ocistage.GuestRootPath is broken`)
	}

	// The Go side must produce exactly the shell path with $NAME substituted.
	want := "/var/lib/dew/oci/NAME/merged"
	if got := ocistage.GuestRootPath("NAME"); got != want {
		t.Errorf("GuestRootPath = %q, want %q (must match dew-oci-run's $RUN/merged)", got, want)
	}
}

// dew-oci-run must give each container a /etc/hosts so `localhost` resolves
// locally instead of falling through to DNS. Minimal images ship none, which
// silently breaks same-VM service-to-service config (redis://localhost:6379).
func TestOCIRunInjectsLocalhostHosts(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "initramfs", "build.sh"))
	if err != nil {
		t.Fatalf("read build.sh: %v", err)
	}
	script := string(data)
	for _, want := range []string{`"$RUN/merged/etc/hosts"`, "127.0.0.1", "localhost"} {
		if !strings.Contains(script, want) {
			t.Errorf("dew-oci-run no longer writes a localhost /etc/hosts (missing %q)", want)
		}
	}
}
