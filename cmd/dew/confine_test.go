//go:build darwin

package main

import (
	"reflect"
	"testing"
)

// wrapWithSetpriv prepends the setpriv prefix and, for a single shell-string
// arg, wraps it in /bin/sh -c so setpriv (which doesn't parse shell syntax)
// execs a real argv.
func TestWrapWithSetpriv(t *testing.T) {
	prefix := []string{"setpriv", "--reuid", "65534"}

	// Multi-arg argv is passed straight through after `--`.
	got := wrapWithSetpriv(prefix, []string{"/x/app", "--flag"})
	want := []string{"setpriv", "--reuid", "65534", "--", "/x/app", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv form = %v, want %v", got, want)
	}

	// Single shell-string arg is wrapped in /bin/sh -c.
	got = wrapWithSetpriv(prefix, []string{"echo a; echo b"})
	want = []string{"setpriv", "--reuid", "65534", "--", "/bin/sh", "-c", "echo a; echo b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("shell form = %v, want %v", got, want)
	}
}
