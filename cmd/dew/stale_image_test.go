//go:build darwin

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A second VM on the same disk must fail fast with "in use by another
// running dew VM" guidance — and must NOT advise `rm` (which would destroy
// the first VM's data). Diskless (minimal) acquires no lock.
func TestAcquireDiskLock(t *testing.T) {
	if lk, err := acquireDiskLock(""); lk != nil || err != nil {
		t.Fatalf(`acquireDiskLock("") = %v, %v; want nil, nil`, lk, err)
	}

	disk := filepath.Join(t.TempDir(), "node.img")
	lk, err := acquireDiskLock(disk)
	if err != nil || lk == nil {
		t.Fatalf("first acquireDiskLock: lk=%v err=%v", lk, err)
	}
	defer lk.Release()

	_, err = acquireDiskLock(disk)
	if err == nil {
		t.Fatal("second acquireDiskLock on same disk = nil, want in-use error")
	}
	if !strings.Contains(err.Error(), "in use by another running dew VM") {
		t.Errorf("err = %q, want in-use guidance", err)
	}
	if strings.Contains(err.Error(), "rm ") {
		t.Errorf("in-use error must not advise rm: %q", err)
	}
}

// `dew assets pull standard` must target the standard profile, not
// silently fall back to minimal. Both the positional and --profile
// forms are supported; --profile wins if both appear.
func TestParseAssetsArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		def      string
		wantProf string
		wantForc bool
	}{
		{"positional", []string{"pull", "standard"}, "minimal", "standard", false},
		{"flag", []string{"pull", "--profile", "node"}, "minimal", "node", false},
		{"positional+force", []string{"pull", "standard", "--force"}, "minimal", "standard", true},
		{"flag wins over positional", []string{"pull", "minimal", "--profile", "standard"}, "minimal", "standard", false},
		{"default when bare", []string{"pull"}, "minimal", "minimal", false},
		{"seeded default", []string{"list"}, "node", "node", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProf, gotForc := parseAssetsArgs(tt.args, tt.def)
			if gotProf != tt.wantProf {
				t.Errorf("profile = %q, want %q", gotProf, tt.wantProf)
			}
			if gotForc != tt.wantForc {
				t.Errorf("force = %v, want %v", gotForc, tt.wantForc)
			}
		})
	}
}

func TestStaleDiskHint(t *testing.T) {
	// No disk → no hint (minimal profile has no persistent disk).
	if h := staleDiskHint("", "dew up --reset-disk"); h != "" {
		t.Errorf("expected empty hint with no disk, got %q", h)
	}
	// Disk profile → hint names the rebuild command and the disk path.
	h := staleDiskHint("/home/u/.local/share/dew/standard.img", "dew up --reset-disk")
	for _, want := range []string{"dew up --reset-disk", "/home/u/.local/share/dew/standard.img", "rm "} {
		if !strings.Contains(h, want) {
			t.Errorf("hint missing %q:\n%s", want, h)
		}
	}
}

// A named VM must get its own per-name disk image so concurrent named VMs
// don't collide on one "<profile>.img" (which VZ rejects on the second
// boot). The default unnamed VM keeps the historical path so existing
// disks are reused.
func TestProfileDiskPath(t *testing.T) {
	const dir = "/home/u/.local/share/dew"
	tests := []struct {
		profile, name, want string
	}{
		{"node", "", "/home/u/.local/share/dew/node.img"},
		{"node", "redis", "/home/u/.local/share/dew/node-redis.img"},
		{"standard", "", "/home/u/.local/share/dew/standard.img"},
		{"python", "api", "/home/u/.local/share/dew/python-api.img"},
	}
	for _, tt := range tests {
		if got := profileDiskPath(dir, tt.profile, tt.name); got != tt.want {
			t.Errorf("profileDiskPath(%q, %q, %q) = %q, want %q",
				dir, tt.profile, tt.name, got, tt.want)
		}
	}
}

func TestParseFlags_ResetDisk(t *testing.T) {
	if _, _, err := parseFlags([]string{"--reset-disk"}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !flagResetDisk {
		t.Error("--reset-disk was not parsed")
	}
	// Resets when absent on a later call.
	if _, _, err := parseFlags([]string{}); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flagResetDisk {
		t.Error("flagResetDisk should reset to false when absent")
	}
}
