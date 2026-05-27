package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func setupProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte(content), 0644)
	}
	return dir
}

func TestDetect_Vite(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json":  `{"dependencies":{"react":"^18"},"devDependencies":{"vite":"^5"}}`,
		"vite.config.js": `export default {}`,
		"package-lock.json": `{}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "vite" {
		t.Errorf("Framework = %q, want vite", p.Framework)
	}
	if p.PackageMgr != "npm" {
		t.Errorf("PackageMgr = %q, want npm", p.PackageMgr)
	}
	if p.Port != 5173 {
		t.Errorf("Port = %d, want 5173", p.Port)
	}
	if p.DevCmd != "npm run dev" {
		t.Errorf("DevCmd = %q, want 'npm run dev'", p.DevCmd)
	}
}

func TestDetect_NextJS(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json":   `{"dependencies":{"next":"^14","react":"^18"}}`,
		"next.config.mjs": `export default {}`,
		"yarn.lock":      ``,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "nextjs" {
		t.Errorf("Framework = %q, want nextjs", p.Framework)
	}
	if p.PackageMgr != "yarn" {
		t.Errorf("PackageMgr = %q, want yarn", p.PackageMgr)
	}
	if p.Port != 3000 {
		t.Errorf("Port = %d, want 3000", p.Port)
	}
	if p.DevCmd != "yarn dev" {
		t.Errorf("DevCmd = %q, want 'yarn dev'", p.DevCmd)
	}
}

func TestDetect_Astro(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json":    `{"dependencies":{"astro":"^4"}}`,
		"astro.config.mjs": `export default {}`,
		"pnpm-lock.yaml":  ``,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "astro" {
		t.Errorf("Framework = %q", p.Framework)
	}
	if p.PackageMgr != "pnpm" {
		t.Errorf("PackageMgr = %q", p.PackageMgr)
	}
	if p.Port != 4321 {
		t.Errorf("Port = %d, want 4321", p.Port)
	}
	if p.DevCmd != "pnpm dev" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
}

func TestDetect_Bun(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"dependencies":{"react":"^18"},"devDependencies":{"vite":"^5"}}`,
		"vite.config.ts": `export default {}`,
		"bun.lockb":     ``,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.PackageMgr != "bun" {
		t.Errorf("PackageMgr = %q, want bun", p.PackageMgr)
	}
	if p.DevCmd != "bun run dev" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
}

func TestDetect_GenericNode(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"scripts":{"start":"node server.js"}}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "node" {
		t.Errorf("Framework = %q, want node", p.Framework)
	}
	if p.DevCmd != "npm start" {
		t.Errorf("DevCmd = %q, want 'npm start'", p.DevCmd)
	}
}

func TestDetect_NodeWithDev(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"nodemon server.js","start":"node server.js"}}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.DevCmd != "npm run dev" {
		t.Errorf("DevCmd = %q, want 'npm run dev'", p.DevCmd)
	}
}

func TestDetect_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "" {
		t.Errorf("Framework = %q, want empty", p.Framework)
	}
	if p.PackageMgr != "" {
		t.Errorf("PackageMgr = %q, want empty", p.PackageMgr)
	}
}

func TestDetect_SvelteKit(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json":     `{"devDependencies":{"@sveltejs/kit":"^2"}}`,
		"svelte.config.js": `export default {}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "sveltekit" {
		t.Errorf("Framework = %q", p.Framework)
	}
	if p.Port != 5173 {
		t.Errorf("Port = %d, want 5173", p.Port)
	}
}

func TestDetect_ViteFromDeps(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"devDependencies":{"vite":"^5","react":"^18"}}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "vite" {
		t.Errorf("Framework = %q, want vite (detected from deps)", p.Framework)
	}
}
