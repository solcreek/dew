package detect

import (
	"testing"
)

func TestNeedsNativeBuildTools_LockfileHit(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"dependencies":{"next":"^14"}}`,
		"package-lock.json": `{
			"lockfileVersion": 3,
			"packages": {
				"": {"name":"app","version":"0.0.0"},
				"node_modules/next": {"version":"14.0.0"},
				"node_modules/sharp": {"version":"0.33.0"},
				"node_modules/react": {"version":"18.2.0"}
			}
		}`,
	})
	needs, matched := NeedsNativeBuildTools(dir)
	if !needs {
		t.Fatalf("expected sharp to trigger native build tools, got matched=%v", matched)
	}
	found := false
	for _, m := range matched {
		if m == "sharp" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sharp in matched, got %v", matched)
	}
}

func TestNeedsNativeBuildTools_NestedDep(t *testing.T) {
	// A native dep nested inside another package's tree should still
	// trigger the heuristic — npm install would compile it just the
	// same.
	dir := setupProject(t, map[string]string{
		"package-lock.json": `{
			"packages": {
				"node_modules/some-pkg": {},
				"node_modules/some-pkg/node_modules/bcrypt": {}
			}
		}`,
	})
	needs, matched := NeedsNativeBuildTools(dir)
	if !needs || len(matched) != 1 || matched[0] != "bcrypt" {
		t.Errorf("expected matched=[bcrypt], got needs=%v matched=%v", needs, matched)
	}
}

func TestNeedsNativeBuildTools_PackageJsonOnly(t *testing.T) {
	// Before first install there's no lockfile — fall back to package.json
	dir := setupProject(t, map[string]string{
		"package.json": `{
			"dependencies": {"react": "^19"},
			"devDependencies": {"sqlite3": "^5"}
		}`,
	})
	needs, matched := NeedsNativeBuildTools(dir)
	if !needs {
		t.Fatalf("expected sqlite3 from devDependencies to trigger, got matched=%v", matched)
	}
}

func TestNeedsNativeBuildTools_PureJSProject(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{
			"dependencies": {"react": "^19", "react-dom": "^19"},
			"devDependencies": {"vite": "^8", "@vitejs/plugin-react": "^6"}
		}`,
		"package-lock.json": `{
			"packages": {
				"node_modules/react": {},
				"node_modules/react-dom": {},
				"node_modules/vite": {},
				"node_modules/@vitejs/plugin-react": {}
			}
		}`,
	})
	needs, matched := NeedsNativeBuildTools(dir)
	if needs {
		t.Errorf("pure vite+react project should not need build tools, got matched=%v", matched)
	}
}

func TestNeedsNativeBuildTools_NoFiles(t *testing.T) {
	dir := t.TempDir()
	needs, matched := NeedsNativeBuildTools(dir)
	if needs || len(matched) != 0 {
		t.Errorf("empty dir: expected needs=false matched=[], got %v %v", needs, matched)
	}
}

func TestScanInstallStderrForNativeBuild(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"clean gyp err", "npm error gyp ERR! build error", true},
		{"node-gyp rebuild line", "node-gyp rebuild failed", true},
		{"gcc not found", "sh: g++: not found", true},
		{"python missing", "python3: command not found", true},
		{"prebuild-install warning", "prebuild-install warn install No prebuilt binaries found", true},
		{"make error line", "make: *** [Makefile:42] Error 1", true},
		{"network error", "npm ERR! errno ECONNREFUSED", false},
		{"peer dep conflict", "npm ERR! ERESOLVE could not resolve peer", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanInstallStderrForNativeBuild(tc.in)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// setupProject lives in detect_test.go; we reuse it here.
