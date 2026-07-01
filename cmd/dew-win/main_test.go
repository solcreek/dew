//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/solcreek/dew/internal/dewfile"
	"github.com/solcreek/dew/internal/services"
)

// withStubWSL replaces the wslQuery seam with a fake that answers the
// query/control commands (--status, --list [--running], --terminate,
// and the `-d dew -- true` probe) from the given state, so command
// logic can be tested without a real WSL2 install. Restored on cleanup.
func withStubWSL(t *testing.T, installed bool, all, running []string, fn func()) {
	t.Helper()
	orig := wslQuery
	defer func() { wslQuery = orig }()
	wslQuery = func(args ...string) ([]byte, error) {
		if !installed {
			return nil, fmt.Errorf("wsl not installed")
		}
		switch {
		case slices.Contains(args, "--status"):
			return []byte("ok"), nil
		case slices.Contains(args, "--terminate"):
			return nil, nil
		case slices.Contains(args, "--list") && slices.Contains(args, "--running"):
			return []byte(strings.Join(running, "\n") + "\n"), nil
		case slices.Contains(args, "--list"):
			return []byte(strings.Join(all, "\n") + "\n"), nil
		case slices.Contains(args, "true"): // distroExists probe
			if slices.Contains(all, distroName) {
				return nil, nil
			}
			return nil, fmt.Errorf("distro not registered")
		}
		return nil, fmt.Errorf("unexpected wslQuery args: %v", args)
	}
	fn()
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// what was written, so tests can assert on a command's printed output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	// Restore Stdout via defer so a t.Fatal inside fn (runtime.Goexit)
	// can't leave later tests writing to this pipe. Close both ends too:
	// the writer so ReadAll sees EOF, the reader so we don't leak fds.
	defer func() { os.Stdout = orig }()
	defer r.Close()
	func() {
		defer w.Close()
		fn()
	}()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("captureStdout: reading captured output: %v", err)
	}
	return string(out)
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"simple", "'simple'"},
		{"/mnt/c/Users/foo/proj", "'/mnt/c/Users/foo/proj'"},
		{"with space", "'with space'"},
		{"", "''"},
		// A single quote must be closed, escaped, and reopened.
		{"a'b", `'a'\''b'`},
		{"O'Brien's dir", `'O'\''Brien'\''s dir'`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRootfsAssetArch(t *testing.T) {
	cases := []struct {
		goarch, want string
	}{
		{"amd64", "x86_64"},
		{"arm64", "aarch64"},
		{"386", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := rootfsAssetArch(c.goarch); got != c.want {
			t.Errorf("rootfsAssetArch(%q) = %q, want %q", c.goarch, got, c.want)
		}
	}
}

func TestExecWSLArgs(t *testing.T) {
	got := execWSLArgs([]string{"ls", "-la", "/tmp"})
	want := []string{"-d", distroName, "--exec", "ls", "-la", "/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execWSLArgs = %v, want %v", got, want)
	}
	// Regression guard: element 2 must stay "--exec". Reverting to the
	// bare "--" separator re-introduces the implicit /bin/sh that mangles
	// argv (pipes interpreted, $vars and spaces lost).
	if got[2] != "--exec" {
		t.Errorf("execWSLArgs separator = %q, want %q", got[2], "--exec")
	}
}

func TestParseDistroNames(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want map[string]bool
	}{
		{
			name: "plain utf8",
			in:   []byte("Ubuntu\ndew\n"),
			want: map[string]bool{"Ubuntu": true, "dew": true},
		},
		{
			name: "crlf trimmed",
			in:   []byte("Ubuntu\r\ndew\r\n"),
			want: map[string]bool{"Ubuntu": true, "dew": true},
		},
		{
			// Legacy WSL emits UTF-16LE: each ASCII byte followed by NUL.
			// Stripping NULs must recover the names.
			name: "utf16le with nuls",
			in:   []byte("d\x00e\x00w\x00\n\x00"),
			want: map[string]bool{"dew": true},
		},
		{
			name: "empty",
			in:   []byte(""),
			want: map[string]bool{},
		},
		{
			name: "blank lines ignored",
			in:   []byte("\n  \ndew\n\n"),
			want: map[string]bool{"dew": true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDistroNames(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseDistroNames(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestDetectDevScript(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"dev preferred over start", `{"scripts":{"dev":"vite","start":"node ."}}`, "dev"},
		{"start fallback", `{"scripts":{"start":"node ."}}`, "start"},
		{"no runnable script", `{"scripts":{"build":"tsc"}}`, ""},
		{"no scripts key", `{"name":"x"}`, ""},
		{"malformed json tolerated", `{not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			pkg := filepath.Join(dir, "package.json")
			if err := os.WriteFile(pkg, []byte(c.content), 0644); err != nil {
				t.Fatal(err)
			}
			if got := detectDevScript(pkg); got != c.want {
				t.Errorf("detectDevScript(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
	// A missing file must not panic and must yield "".
	if got := detectDevScript(filepath.Join(t.TempDir(), "nope.json")); got != "" {
		t.Errorf("detectDevScript(missing) = %q, want \"\"", got)
	}
}

func TestStripLeadingSeparator(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"--", "uname", "-a"}, []string{"uname", "-a"}},
		{[]string{"uname", "-a"}, []string{"uname", "-a"}},
		{[]string{"--"}, []string{}},
		{[]string{}, []string{}},
		// Only ONE leading separator is stripped; a second "--" is an arg.
		{[]string{"--", "--", "x"}, []string{"--", "x"}},
	}
	for _, c := range cases {
		got := stripLeadingSeparator(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("stripLeadingSeparator(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFormatVMList(t *testing.T) {
	all := map[string]bool{"Ubuntu": true, distroName: true, "Alpine": true}
	running := map[string]bool{distroName: true}
	got := formatVMList(all, running)

	// The dew distro sorts first and is tagged; the rest are alphabetical.
	want := "" +
		"  dew                  running  (dew)\n" +
		"  Alpine               stopped\n" +
		"  Ubuntu               stopped\n"
	if got != want {
		t.Errorf("formatVMList mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWslInstalled(t *testing.T) {
	withStubWSL(t, true, nil, nil, func() {
		if !wslInstalled() {
			t.Error("wslInstalled() = false, want true")
		}
	})
	withStubWSL(t, false, nil, nil, func() {
		if wslInstalled() {
			t.Error("wslInstalled() = true, want false")
		}
	})
}

func TestDistroHelpersAndState(t *testing.T) {
	withStubWSL(t, true, []string{"Ubuntu", "dew"}, []string{"dew"}, func() {
		if reg, err := distroRegistered(); err != nil || !reg {
			t.Errorf("distroRegistered() = %v, %v; want true, nil", reg, err)
		}
		if run, err := distroRunningNow(); err != nil || !run {
			t.Errorf("distroRunningNow() = %v, %v; want true, nil", run, err)
		}
		if got, err := distroState(); err != nil || got != "running" {
			t.Errorf("distroState() = %q, %v; want running, nil", got, err)
		}
	})
	withStubWSL(t, true, []string{"Ubuntu", "dew"}, nil, func() {
		if got, err := distroState(); err != nil || got != "stopped" {
			t.Errorf("distroState() = %q, %v; want stopped, nil", got, err)
		}
	})
	withStubWSL(t, true, []string{"Ubuntu"}, nil, func() {
		if reg, err := distroRegistered(); err != nil || reg {
			t.Errorf("distroRegistered() = %v, %v; want false, nil", reg, err)
		}
		if got, err := distroState(); err != nil || got != "not installed" {
			t.Errorf("distroState() = %q, %v; want not installed, nil", got, err)
		}
	})
	// When wsl.exe errors, the helpers return an error (not a silent empty
	// set), so callers can surface it instead of misreporting "not installed".
	withStubWSL(t, false, nil, nil, func() {
		if _, err := distroRegistered(); err == nil {
			t.Error("distroRegistered() err = nil with wsl absent, want error")
		}
		if _, err := distroState(); err == nil {
			t.Error("distroState() err = nil with wsl absent, want error")
		}
	})
}

func TestCmdVM(t *testing.T) {
	t.Run("status running", func(t *testing.T) {
		withStubWSL(t, true, []string{"dew"}, []string{"dew"}, func() {
			out := captureStdout(t, func() {
				if err := cmdVM([]string{"status"}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "dew: running") {
				t.Errorf("status output = %q", out)
			}
		})
	})
	t.Run("start ensures + reports running", func(t *testing.T) {
		withStubWSL(t, true, []string{"dew"}, nil, func() {
			out := captureStdout(t, func() {
				if err := cmdVM([]string{"start"}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "running") {
				t.Errorf("start output = %q", out)
			}
		})
	})
	t.Run("stop terminates", func(t *testing.T) {
		withStubWSL(t, true, []string{"dew"}, nil, func() {
			out := captureStdout(t, func() {
				if err := cmdVM([]string{"stop"}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "dew: stopped") {
				t.Errorf("stop output = %q", out)
			}
		})
	})
	t.Run("list shows distros", func(t *testing.T) {
		withStubWSL(t, true, []string{"dew", "Ubuntu"}, []string{"dew"}, func() {
			out := captureStdout(t, func() {
				if err := cmdVM([]string{"list"}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "dew") || !strings.Contains(out, "Ubuntu") {
				t.Errorf("list output = %q", out)
			}
		})
	})
	t.Run("unknown subcommand errors", func(t *testing.T) {
		withStubWSL(t, true, nil, nil, func() {
			if err := cmdVM([]string{"bogus"}); err == nil {
				t.Error("want error for unknown subcommand")
			}
		})
	})
	t.Run("no subcommand errors", func(t *testing.T) {
		if err := cmdVM(nil); err == nil {
			t.Error("want usage error when no subcommand given")
		}
	})
}

func TestVmStatusLine(t *testing.T) {
	cases := []struct {
		name         string
		all, running []string
		want         string
	}{
		{"absent", []string{"Ubuntu"}, nil, "dew: not installed (run: dew setup)"},
		{"stopped", []string{"dew"}, nil, "dew: stopped"},
		{"running", []string{"dew"}, []string{"dew"}, "dew: running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStubWSL(t, true, c.all, c.running, func() {
				if got, err := vmStatusLine(); err != nil || got != c.want {
					t.Errorf("vmStatusLine() = %q, %v; want %q, nil", got, err, c.want)
				}
			})
		})
	}
	// A wsl --list failure must surface as an error, not a bogus status.
	t.Run("list error", func(t *testing.T) {
		withStubWSL(t, false, nil, nil, func() {
			if _, err := vmStatusLine(); err == nil {
				t.Error("vmStatusLine() err = nil on wsl failure, want error")
			}
		})
	})
}

func TestDoctorReport(t *testing.T) {
	labels := func(cs []doctorCheck) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.level + " " + c.label
		}
		return out
	}
	cases := []struct {
		name    string
		in      doctorInputs
		healthy bool
		want    []string
	}{
		{
			name:    "wsl missing short-circuits",
			in:      doctorInputs{wslInstalled: false},
			healthy: false,
			want:    []string{"FAIL wsl2"},
		},
		{
			name:    "distro not imported fails",
			in:      doctorInputs{wslInstalled: true, mirrored: true, rootfsMB: 35},
			healthy: false,
			want:    []string{"OK wsl2", "FAIL distro", "OK mirrored net", "OK rootfs"},
		},
		{
			name: "all healthy",
			in: doctorInputs{
				wslInstalled: true, registered: true, running: true,
				nodeVersion: "v22.0.0", mirrored: true, rootfsMB: 35,
			},
			healthy: true,
			want:    []string{"OK wsl2", "OK distro", "OK node", "OK mirrored net", "OK rootfs"},
		},
		{
			name: "warnings do not fail the verdict",
			in: doctorInputs{
				wslInstalled: true, registered: true, running: false,
				nodeVersion: "", mirrored: false, rootfsMB: -1,
			},
			healthy: true,
			want:    []string{"OK wsl2", "OK distro", "WARN node", "WARN mirrored net", "WARN rootfs"},
		},
		{
			// wsl --list failed: report it, skip the node check, don't
			// misclaim the distro is un-imported.
			name: "list failure fails and skips node",
			in: doctorInputs{
				wslInstalled: true, listErr: "exit status 1",
				mirrored: true, rootfsMB: 35,
			},
			healthy: false,
			want:    []string{"OK wsl2", "FAIL distro", "OK mirrored net", "OK rootfs"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs, healthy := doctorReport(c.in)
			if healthy != c.healthy {
				t.Errorf("healthy = %v, want %v", healthy, c.healthy)
			}
			if got := labels(cs); !reflect.DeepEqual(got, c.want) {
				t.Errorf("checks = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCmdRun(t *testing.T) {
	// After stripping the optional separator, an empty command line is a
	// usage error returned before any wsl.exe call.
	if err := cmdRun(nil); err == nil {
		t.Error("cmdRun(nil) = nil, want usage error")
	}
	if err := cmdRun([]string{"--"}); err == nil {
		t.Error("cmdRun([--]) = nil, want usage error")
	}
}

func TestDewDataDir(t *testing.T) {
	if got := dewDataDir(); !strings.HasSuffix(got, ".dew") {
		t.Errorf("dewDataDir() = %q, want a path ending in .dew", got)
	}
}

func TestParseUpArgs(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		dir          string
		with         []string
		servicesOnly bool
		wantErr      bool
	}{
		{"empty", nil, ".", nil, false, false},
		{"dir only", []string{"./app"}, "./app", nil, false, false},
		{"with space form", []string{"--with", "postgres,redis"}, ".", []string{"postgres", "redis"}, false, false},
		{"with equals form", []string{"--with=postgres"}, ".", []string{"postgres"}, false, false},
		{"dir and with", []string{"./app", "--with", "redis"}, "./app", []string{"redis"}, false, false},
		{"with then dir (PR example)", []string{"--with", "redis", "./app"}, "./app", []string{"redis"}, false, false},
		{"csv trims blanks", []string{"--with", "a, b ,,c"}, ".", []string{"a", "b", "c"}, false, false},
		{"dedup preserves order", []string{"--with", "redis,redis,postgres,redis"}, ".", []string{"redis", "postgres"}, false, false},
		{"services-only flag", []string{"--services-only"}, ".", nil, true, false},
		{"services-only with dir", []string{"--services-only", "./eng"}, "./eng", nil, true, false},
		{"with needs arg", []string{"--with"}, "", nil, false, true},
		{"with empty equals rejected", []string{"--with="}, "", nil, false, true},
		{"with blank value rejected", []string{"--with", "  ,  "}, "", nil, false, true},
		{"unknown flag", []string{"--bogus"}, "", nil, false, true},
		{"multiple dirs rejected", []string{"./a", "./b"}, "", nil, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, with, servicesOnly, err := parseUpArgs(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if dir != c.dir {
				t.Errorf("dir = %q, want %q", dir, c.dir)
			}
			if !reflect.DeepEqual(with, c.with) {
				t.Errorf("with = %v, want %v", with, c.with)
			}
			if servicesOnly != c.servicesOnly {
				t.Errorf("servicesOnly = %v, want %v", servicesOnly, c.servicesOnly)
			}
		})
	}
}

func TestServiceFromDewfile(t *testing.T) {
	ds := dewfile.Service{
		Name:  "pocketbase",
		Image: "ghcr.io/pocketbase/pocketbase:latest",
		Port:  8090,
		Ports: []string{"9000:9000"}, // extra forwards: not yet mapped
		Env:   []string{"FOO=bar"},
		Data:  "/pb_data", // volume persistence: not yet mapped
		Args:  []string{"serve", "--http=0.0.0.0:8090"},
	}
	got := serviceFromDewfile(ds)
	want := services.Service{
		Name:  "pocketbase",
		Image: "ghcr.io/pocketbase/pocketbase:latest",
		Port:  8090,
		Env:   []string{"FOO=bar"},
		Args:  []string{"serve", "--http=0.0.0.0:8090"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceFromDewfile() = %+v, want %+v", got, want)
	}
}

func TestCmdServicesJSON(t *testing.T) {
	dir := t.TempDir()
	toml := `[[service]]
  name = "pocketbase-pocketbase"
  image = "ghcr.io/x/pocketbase:0.39.0"
  port = 8090

[[service]]
  name = "redis"
  image = "docker.io/library/redis:alpine"
  port = 6379
`
	if err := os.WriteFile(filepath.Join(dir, "dew.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Distro not running -> every service reports stopped, and no podman probe
	// is attempted (a status query must not start the distro).
	var out string
	withStubWSL(t, true, []string{distroName}, nil, func() {
		out = captureStdout(t, func() {
			if err := cmdServices([]string{"--json", dir}); err != nil {
				t.Fatalf("cmdServices: %v", err)
			}
		})
	})

	var env struct {
		OK            bool   `json:"ok"`
		SchemaVersion string `json:"schema_version"`
		Data          struct {
			Services []struct {
				Name      string `json:"name"`
				Running   bool   `json:"running"`
				HostPort  int    `json:"host_port"`
				GuestPort int    `json:"guest_port"`
			} `json:"services"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("parse envelope %q: %v", out, err)
	}
	if !env.OK || env.SchemaVersion != "1.0" {
		t.Errorf("envelope ok=%v schema=%q, want ok=true schema=1.0", env.OK, env.SchemaVersion)
	}
	if len(env.Data.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(env.Data.Services))
	}
	pb := env.Data.Services[0]
	if pb.Name != "pocketbase-pocketbase" || pb.Running || pb.HostPort != 8090 || pb.GuestPort != 8090 {
		t.Errorf("service[0] = %+v, want name=pocketbase-pocketbase running=false host=guest=8090", pb)
	}
}

func TestSplitExecJSONFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
		wantRest []string
	}{
		{"leading --json triggers envelope", []string{"--json", "sh", "-c", "echo hi"}, true, []string{"sh", "-c", "echo hi"}},
		{"no --json is passthrough", []string{"sh", "-c", "echo hi"}, false, []string{"sh", "-c", "echo hi"}},
		{"non-leading --json stays guest argv", []string{"sh", "-c", "printf --json"}, false, []string{"sh", "-c", "printf --json"}},
		{"only --json", []string{"--json"}, true, []string{}},
		{"empty", nil, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotJSON, gotRest := splitExecJSONFlag(c.args)
			if gotJSON != c.wantJSON {
				t.Errorf("jsonOut = %v, want %v", gotJSON, c.wantJSON)
			}
			if !reflect.DeepEqual(gotRest, c.wantRest) {
				t.Errorf("rest = %v, want %v", gotRest, c.wantRest)
			}
		})
	}
}

func TestCmdServicesRejectsMultipleDirs(t *testing.T) {
	err := cmdServices([]string{"./a", "./b"})
	if err == nil {
		t.Fatal("cmdServices(./a ./b) = nil, want an error rejecting the second dir")
	}
	if !strings.Contains(err.Error(), "at most one dir") {
		t.Errorf("error = %q, want it to mention 'at most one dir'", err)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b ,,c", []string{"a", "b", "c"}},
		{"", nil},
		{"   ", nil},
		{"solo", []string{"solo"}},
	}
	for _, c := range cases {
		if got := splitCSV(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestServiceContainer(t *testing.T) {
	if got := serviceContainer("redis"); got != "dew-svc-redis" {
		t.Errorf("serviceContainer(redis) = %q, want dew-svc-redis", got)
	}
}

func TestPodmanRunArgs(t *testing.T) {
	// No env, no server args: exact argv.
	redis := services.Service{Name: "redis", Image: "docker.io/library/redis:7-alpine", Port: 6379}
	want := []string{"-d", "dew", "--exec", "podman", "run", "-d",
		"--name", "dew-svc-redis", "--network=host", "docker.io/library/redis:7-alpine"}
	if got := podmanRunArgs(redis); !reflect.DeepEqual(got, want) {
		t.Errorf("redis: got %v, want %v", got, want)
	}

	// Env pairs must precede the image; server args must follow it.
	mysql := services.Service{
		Name: "mysql", Image: "docker.io/library/mysql:8-oracle", Port: 3306,
		Env:  []string{"MYSQL_ROOT_PASSWORD=dew"},
		Args: []string{"--bind-address=0.0.0.0"},
	}
	got := podmanRunArgs(mysql)
	if !slices.Contains(got, "--network=host") {
		t.Errorf("mysql: --network=host missing in %v", got)
	}
	env := slices.Index(got, "MYSQL_ROOT_PASSWORD=dew")
	img := slices.Index(got, mysql.Image)
	arg := slices.Index(got, "--bind-address=0.0.0.0")
	if env < 0 || img < 0 || arg < 0 {
		t.Fatalf("mysql: missing element in %v", got)
	}
	if !(env < img && img < arg) {
		t.Errorf("mysql: order wrong (env=%d img=%d arg=%d) in %v", env, img, arg, got)
	}
}

func TestHasMirroredNetworking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical", "[wsl2]\nnetworkingMode=mirrored\n", true},
		{"spaces around equals", "[wsl2]\nnetworkingMode = mirrored\n", true},
		{"tabs around equals", "[wsl2]\nnetworkingMode\t=\tmirrored\n", true},
		{"tab indented", "[wsl2]\n\tnetworkingMode=mirrored\n", true},
		{"case insensitive", "[wsl2]\nNetworkingMode=Mirrored\n", true},
		{"nat mode", "[wsl2]\nnetworkingMode=nat\n", false},
		{"absent", "[wsl2]\nmemory=4GB\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasMirroredNetworking([]byte(c.in)); got != c.want {
				t.Errorf("hasMirroredNetworking(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
