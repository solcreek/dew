package dewfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadAbsentIsNil(t *testing.T) {
	f, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if f != nil {
		t.Fatalf("Load absent = %+v, want nil", f)
	}
}

func TestLoadValid(t *testing.T) {
	dir := write(t, `
[project]
profile = "node"

[dev]
install = "npm ci"
command = "npm run dev"
port = 3000

[[service]]
name = "redis"
image = "redis:7-alpine"
port = 6379

[[service]]
name = "mailpit"
image = "axllent/mailpit:latest"
port = 8025
env = ["MP_SMTP_AUTH_ACCEPT_ANY=1"]
data = "/data"
args = ["--smtp-auth-allow-insecure"]
`)
	f, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Project.Profile != "node" {
		t.Errorf("profile = %q", f.Project.Profile)
	}
	if f.Dev.Install != "npm ci" || f.Dev.Command != "npm run dev" || f.Dev.Port != 3000 {
		t.Errorf("dev = %+v", f.Dev)
	}
	if len(f.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(f.Services))
	}

	// ServiceList must map cleanly onto services.Service (DataDir, Args, Env).
	list := f.ServiceList()
	mp := list[1]
	if mp.Name != "mailpit" || mp.Image != "axllent/mailpit:latest" || mp.Port != 8025 {
		t.Errorf("mailpit svc = %+v", mp)
	}
	if mp.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data", mp.DataDir)
	}
	if len(mp.Env) != 1 || mp.Env[0] != "MP_SMTP_AUTH_ACCEPT_ANY=1" {
		t.Errorf("Env = %v", mp.Env)
	}
	if len(mp.Args) != 1 || mp.Args[0] != "--smtp-auth-allow-insecure" {
		t.Errorf("Args = %v", mp.Args)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name, body, wantSubstr string
	}{
		{"missing image", "[[service]]\nname = \"x\"\n", "missing an image"},
		{"missing name", "[[service]]\nimage = \"x\"\n", "missing a name"},
		{"duplicate name", "[[service]]\nname=\"a\"\nimage=\"x\"\nport=1\n[[service]]\nname=\"a\"\nimage=\"y\"\nport=2\n", "duplicate service name"},
		{"bad profile", "[project]\nprofile = \"huge\"\n", "invalid profile"},
		{"missing port", "[[service]]\nname=\"a\"\nimage=\"x\"\n", "needs a port"},
		{"zero port", "[[service]]\nname=\"a\"\nimage=\"x\"\nport=0\n", "needs a port"},
		{"relative data", "[[service]]\nname=\"a\"\nimage=\"x\"\nport=1\ndata=\"var/data\"\n", "must be absolute"},
		{"bad env", "[[service]]\nname=\"a\"\nimage=\"x\"\nport=1\nenv=[\"NOEQUALS\"]\n", "must be KEY=VALUE"},
		{"bad service name", "[[service]]\nname=\"Bad Name\"\nimage=\"x\"\n", "invalid service name"},
		{"unknown key", "[project]\nprofil = \"node\"\n", "unknown key"},
		{"bad toml", "[project\n", "dew.toml:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("err = %q, want substring %q", err, c.wantSubstr)
			}
		})
	}
}

// A starter file must parse back cleanly and pin the detected fields.
func TestStarterRoundTrips(t *testing.T) {
	out := Starter("node", "npm ci", "npm run dev", 3000)
	dir := write(t, out)
	f, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(Starter): %v\n%s", err, out)
	}
	if f.Project.Profile != "node" || f.Dev.Command != "npm run dev" || f.Dev.Port != 3000 {
		t.Errorf("starter did not pin detected fields: %+v / %+v", f.Project, f.Dev)
	}
	// The example services are commented, so none are active.
	if len(f.Services) != 0 {
		t.Errorf("starter services = %d, want 0 (commented)", len(f.Services))
	}
}

// An empty-detection starter (nothing pinned) must still be valid TOML.
func TestStarterEmptyRoundTrips(t *testing.T) {
	out := Starter("", "", "", 0)
	if _, err := Load(write(t, out)); err != nil {
		t.Fatalf("Load(empty Starter): %v\n%s", err, out)
	}
}

func TestExists(t *testing.T) {
	if Exists(t.TempDir()) {
		t.Error("Exists on empty dir = true")
	}
	if !Exists(write(t, "[project]\n")) {
		t.Error("Exists on dir with dew.toml = false")
	}
}
