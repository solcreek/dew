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
	if p.DevCmd != "npm run dev -- --host 0.0.0.0" {
		t.Errorf("DevCmd = %q, want 'npm run dev -- --host 0.0.0.0'", p.DevCmd)
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
	if p.DevCmd != "yarn dev -- -H 0.0.0.0" {
		t.Errorf("DevCmd = %q, want 'yarn dev -- -H 0.0.0.0'", p.DevCmd)
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
	if p.DevCmd != "pnpm dev --host 0.0.0.0" {
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
	if p.DevCmd != "bun run dev --host 0.0.0.0" {
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
	if p.DevCmd != "npm start -- --host 0.0.0.0" {
		t.Errorf("DevCmd = %q, want 'npm start -- --host 0.0.0.0'", p.DevCmd)
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
	if p.DevCmd != "npm run dev -- --host 0.0.0.0" {
		t.Errorf("DevCmd = %q, want 'npm run dev -- --host 0.0.0.0'", p.DevCmd)
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

func TestDetect_InstallCmd(t *testing.T) {
	tests := []struct {
		mgr  string
		want string
	}{
		{"npm", "npm install --legacy-peer-deps"},
		{"yarn", "yarn install"},
		{"pnpm", "pnpm install"},
		{"bun", "bun install"},
	}
	for _, tt := range tests {
		got := buildInstallCmd(tt.mgr)
		if got != tt.want {
			t.Errorf("buildInstallCmd(%q) = %q, want %q", tt.mgr, got, tt.want)
		}
	}
}

func TestDetect_DefaultPort(t *testing.T) {
	tests := []struct {
		framework string
		want      int
	}{
		{"vite", 5173},
		{"nextjs", 3000},
		{"astro", 4321},
		{"nuxt", 3000},
		{"sveltekit", 5173},
		{"node", 3000},
		{"unknown", 3000},
	}
	for _, tt := range tests {
		got := defaultPort(tt.framework)
		if got != tt.want {
			t.Errorf("defaultPort(%q) = %d, want %d", tt.framework, got, tt.want)
		}
	}
}

func TestDetect_Profile(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json": `{"dependencies":{"react":"^18"}}`,
		"vite.config.js": `export default {}`,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile != "node" {
		t.Errorf("Profile = %q, want node", p.Profile)
	}
	if p.Runtime != "node" {
		t.Errorf("Runtime = %q, want node", p.Runtime)
	}
}

func TestDetect_NuxtWithYarn(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"package.json":  `{"dependencies":{"nuxt":"^3"}}`,
		"nuxt.config.ts": `export default {}`,
		"yarn.lock":     ``,
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "nuxt" {
		t.Errorf("Framework = %q", p.Framework)
	}
	if p.PackageMgr != "yarn" {
		t.Errorf("PackageMgr = %q", p.PackageMgr)
	}
	if p.Port != 3000 {
		t.Errorf("Port = %d", p.Port)
	}
	if p.DevCmd != "yarn dev -- --host 0.0.0.0" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
	if p.InstallCmd != "yarn install" {
		t.Errorf("InstallCmd = %q", p.InstallCmd)
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

// ── Python tests ──

func TestDetect_Django(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"requirements.txt": "django==5.0\npsycopg2-binary",
		"manage.py":        "#!/usr/bin/env python",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", p.Runtime)
	}
	if p.Framework != "django" {
		t.Errorf("Framework = %q, want django", p.Framework)
	}
	if p.PackageMgr != "pip" {
		t.Errorf("PackageMgr = %q, want pip", p.PackageMgr)
	}
	if p.Port != 8000 {
		t.Errorf("Port = %d, want 8000", p.Port)
	}
	if p.DevCmd != "python manage.py runserver 0.0.0.0:8000" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
	if p.InstallCmd != "pip install -r requirements.txt" {
		t.Errorf("InstallCmd = %q", p.InstallCmd)
	}
	if p.Profile != "python" {
		t.Errorf("Profile = %q, want python", p.Profile)
	}
}

func TestDetect_Flask(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"requirements.txt": "flask==3.0\ngunicorn",
		"app.py":           "from flask import Flask",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "flask" {
		t.Errorf("Framework = %q, want flask", p.Framework)
	}
	if p.Port != 5000 {
		t.Errorf("Port = %d, want 5000", p.Port)
	}
	if p.DevCmd != "flask run --host=0.0.0.0" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
}

func TestDetect_FastAPI(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"requirements.txt": "fastapi\nuvicorn",
		"main.py":          "from fastapi import FastAPI",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "fastapi" {
		t.Errorf("Framework = %q, want fastapi", p.Framework)
	}
	if p.Port != 8000 {
		t.Errorf("Port = %d, want 8000", p.Port)
	}
	if p.DevCmd != "uvicorn main:app --host 0.0.0.0 --port 8000" {
		t.Errorf("DevCmd = %q", p.DevCmd)
	}
}

func TestDetect_Poetry(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"pyproject.toml": "[tool.poetry]\nname = \"myapp\"",
		"poetry.lock":    "",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.PackageMgr != "poetry" {
		t.Errorf("PackageMgr = %q, want poetry", p.PackageMgr)
	}
	if p.InstallCmd != "poetry install" {
		t.Errorf("InstallCmd = %q", p.InstallCmd)
	}
}

func TestDetect_Pipenv(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"Pipfile": "[packages]\nflask = \"*\"",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.PackageMgr != "pipenv" {
		t.Errorf("PackageMgr = %q, want pipenv", p.PackageMgr)
	}
}

func TestDetect_Streamlit(t *testing.T) {
	dir := setupProject(t, map[string]string{
		"requirements.txt": "streamlit\npandas",
		"app.py":           "import streamlit",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Framework != "streamlit" {
		t.Errorf("Framework = %q, want streamlit", p.Framework)
	}
	if p.Port != 8501 {
		t.Errorf("Port = %d, want 8501", p.Port)
	}
}

func TestDetect_PythonOverNode(t *testing.T) {
	// Node detector is registered first, but if only Python files exist, Python wins
	dir := setupProject(t, map[string]string{
		"requirements.txt": "flask",
	})
	p, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Runtime != "python" {
		t.Errorf("Runtime = %q, want python (no package.json)", p.Runtime)
	}
}
