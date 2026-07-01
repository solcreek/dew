//go:build windows

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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

func TestHasMirroredNetworking(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical", "[wsl2]\nnetworkingMode=mirrored\n", true},
		{"spaces around equals", "[wsl2]\nnetworkingMode = mirrored\n", true},
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
