//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
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
	fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
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
		if !distroRegistered() {
			t.Error("distroRegistered() = false, want true")
		}
		if !distroRunningNow() {
			t.Error("distroRunningNow() = false, want true")
		}
		if got := distroState(); got != "running" {
			t.Errorf("distroState() = %q, want running", got)
		}
	})
	withStubWSL(t, true, []string{"Ubuntu", "dew"}, nil, func() {
		if got := distroState(); got != "stopped" {
			t.Errorf("distroState() = %q, want stopped", got)
		}
	})
	withStubWSL(t, true, []string{"Ubuntu"}, nil, func() {
		if distroRegistered() {
			t.Error("distroRegistered() = true, want false")
		}
		if got := distroState(); got != "not installed" {
			t.Errorf("distroState() = %q, want not installed", got)
		}
	})
	// When wsl.exe errors (not installed), the list is empty, not a panic.
	withStubWSL(t, false, nil, nil, func() {
		if distroRegistered() {
			t.Error("distroRegistered() = true with wsl absent, want false")
		}
		if got := distroState(); got != "not installed" {
			t.Errorf("distroState() = %q, want not installed", got)
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
				if got := vmStatusLine(); got != c.want {
					t.Errorf("vmStatusLine() = %q, want %q", got, c.want)
				}
			})
		})
	}
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
