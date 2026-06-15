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
