package guestenv

import (
	"strings"
	"testing"
)

func pathOf(env []string) string {
	val := ""
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			val = strings.TrimPrefix(e, "PATH=")
		}
	}
	return val
}

// The agent pins its own PATH to DefaultPath so exec.LookPath resolves the
// util-linux setpriv (/usr/bin) baked into the standard profile rather than
// the BusyBox applet (/bin), which lacks --bounding-set and breaks --confine's
// capability drop. That selection holds only if /usr/bin precedes /bin (and
// /usr/sbin precedes /sbin) in DefaultPath — guard the ordering.
func TestDefaultPath_UtilLinuxBeforeBusyBox(t *testing.T) {
	dirs := strings.Split(DefaultPath, ":")
	idx := func(d string) int {
		for i, x := range dirs {
			if x == d {
				return i
			}
		}
		return -1
	}
	for _, pair := range [][2]string{{"/usr/bin", "/bin"}, {"/usr/sbin", "/sbin"}} {
		usr, busybox := idx(pair[0]), idx(pair[1])
		if usr < 0 || busybox < 0 {
			t.Fatalf("DefaultPath %q missing %s or %s", DefaultPath, pair[0], pair[1])
		}
		if usr > busybox {
			t.Errorf("DefaultPath orders %s (%d) after %s (%d); BusyBox setpriv would shadow util-linux", pair[0], usr, pair[1], busybox)
		}
	}
}

func TestExecEnv_InjectsDefaultWhenMissing(t *testing.T) {
	env := ExecEnv([]string{"HOME=/root", "TERM=xterm"}, nil)
	if got := pathOf(env); got != DefaultPath {
		t.Errorf("PATH = %q, want default %q", got, DefaultPath)
	}
}

func TestExecEnv_InjectsDefaultWhenEmpty(t *testing.T) {
	env := ExecEnv([]string{"PATH=", "HOME=/root"}, nil)
	if got := pathOf(env); got != DefaultPath {
		t.Errorf("PATH = %q, want default %q", got, DefaultPath)
	}
	for _, e := range env {
		if e == "PATH=" {
			t.Error("empty PATH= leaked into env")
		}
	}
}

func TestExecEnv_PreservesExistingPath(t *testing.T) {
	env := ExecEnv([]string{"PATH=/custom/bin", "HOME=/root"}, nil)
	if got := pathOf(env); got != "/custom/bin" {
		t.Errorf("PATH = %q, want /custom/bin (should not override existing)", got)
	}
}

func TestExecEnv_ExtraOverridesPath(t *testing.T) {
	env := ExecEnv([]string{"PATH=/custom/bin"}, []string{"PATH=/override/bin"})
	if got := pathOf(env); got != "/override/bin" {
		t.Errorf("PATH = %q, want /override/bin (request env should win)", got)
	}
}

func TestExecEnv_AppendsExtra(t *testing.T) {
	env := ExecEnv([]string{"PATH=/bin"}, []string{"FOO=bar"})
	var found bool
	for _, e := range env {
		if e == "FOO=bar" {
			found = true
		}
	}
	if !found {
		t.Error("extra env FOO=bar not appended")
	}
}
