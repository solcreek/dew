package nmcache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeLockfile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestComputeStamp_NoLockfile(t *testing.T) {
	_, err := ComputeStamp(t.TempDir())
	if !errors.Is(err, ErrNoLockfile) {
		t.Fatalf("want ErrNoLockfile, got %v", err)
	}
}

func TestComputeStamp_NpmLockfile(t *testing.T) {
	dir := t.TempDir()
	writeLockfile(t, dir, "package-lock.json", `{"deps": "foo"}`)
	s, err := ComputeStamp(dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if s.Lockfile != "package-lock.json" {
		t.Fatalf("want package-lock.json, got %q", s.Lockfile)
	}
	if s.LockfileHash == "" {
		t.Fatal("hash should be populated")
	}
	if s.SchemaVersion != schemaVersion {
		t.Fatalf("want schema %d, got %d", schemaVersion, s.SchemaVersion)
	}
}

func TestComputeStamp_LockfileChangeFlipsHash(t *testing.T) {
	dir := t.TempDir()
	writeLockfile(t, dir, "package-lock.json", `{"version": "1"}`)
	s1, _ := ComputeStamp(dir)
	writeLockfile(t, dir, "package-lock.json", `{"version": "2"}`)
	s2, _ := ComputeStamp(dir)
	if s1.LockfileHash == s2.LockfileHash {
		t.Fatalf("lockfile mutation must flip hash: %s == %s", s1.LockfileHash, s2.LockfileHash)
	}
	if s1.Marshal() == s2.Marshal() {
		t.Fatalf("stamp serialization must differ when lockfile differs")
	}
}

func TestComputeStamp_LockfilePriority(t *testing.T) {
	// If multiple lockfiles exist, pnpm wins over yarn wins over npm —
	// match the priority list in supportedLockfiles.
	dir := t.TempDir()
	writeLockfile(t, dir, "package-lock.json", `{}`)
	writeLockfile(t, dir, "yarn.lock", `{}`)
	writeLockfile(t, dir, "pnpm-lock.yaml", `{}`)
	s, err := ComputeStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Lockfile != "pnpm-lock.yaml" {
		t.Fatalf("pnpm should win when present, got %q", s.Lockfile)
	}
}

func TestComputeStamp_AllSupportedManagers(t *testing.T) {
	for _, name := range supportedLockfiles {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLockfile(t, dir, name, `{}`)
			s, err := ComputeStamp(dir)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if s.Lockfile != name {
				t.Fatalf("want %q, got %q", name, s.Lockfile)
			}
		})
	}
}

func TestStamp_MarshalIsCanonical(t *testing.T) {
	// The VM-side script does a byte-equality compare on the stamp
	// string, so the encoding must be stable.
	s := Stamp{SchemaVersion: 1, Lockfile: "package-lock.json", LockfileHash: "abc"}
	m1 := s.Marshal()
	m2 := s.Marshal()
	if m1 != m2 {
		t.Fatalf("non-canonical marshal: %q vs %q", m1, m2)
	}
	// Validate it round-trips.
	var got Stamp
	if err := json.Unmarshal([]byte(m1), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != s {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, s)
	}
}
