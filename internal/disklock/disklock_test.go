//go:build unix

package disklock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireContendsAndReleases(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "node.img")

	// First acquire succeeds.
	a, err := Acquire(disk)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// A second acquire of the same disk reports it as in use.
	if _, err := Acquire(disk); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Acquire err = %v, want ErrInUse", err)
	}

	// Releasing frees it for a subsequent acquire.
	if err := a.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	b, err := Acquire(disk)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	b.Release()
}

func TestReleaseNilAndDoubleIsSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
	disk := filepath.Join(t.TempDir(), "node.img")
	a, err := Acquire(disk)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("second Release should be a no-op: %v", err)
	}
}

// Different disk paths must not contend with each other.
func TestDistinctDisksDoNotContend(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(filepath.Join(dir, "node.img"))
	if err != nil {
		t.Fatalf("Acquire node: %v", err)
	}
	defer a.Release()
	b, err := Acquire(filepath.Join(dir, "node-redis.img"))
	if err != nil {
		t.Fatalf("Acquire node-redis (distinct disk): %v", err)
	}
	defer b.Release()
}
