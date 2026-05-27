// Package detect inspects a project directory and determines the
// runtime, framework, package manager, dev command, and port.
package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Project struct {
	Dir        string
	Runtime    string // "node", "python", "go", "rust"
	Framework  string // "vite", "nextjs", "astro", "nuxt", "sveltekit", "node", "django", "flask"
	PackageMgr string // "npm", "yarn", "pnpm", "bun", "pip", "go"
	DevCmd     string // full command to start dev server
	InstallCmd string // full command to install dependencies
	Port       int
	Profile    string // recommended dew profile
}

// Detector checks a directory and returns a Project if it matches.
type Detector interface {
	Name() string
	Match(dir string) bool
	Detect(dir string) *Project
}

var detectors []Detector

// Register adds a detector to the registry.
func Register(d Detector) {
	detectors = append(detectors, d)
}

// Detect runs all registered detectors in order and returns the first match.
func Detect(dir string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	for _, d := range detectors {
		if d.Match(abs) {
			p := d.Detect(abs)
			p.Dir = abs
			return p, nil
		}
	}

	return &Project{Dir: abs}, nil
}

func init() {
	Register(&nodeDetector{})
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ── Node.js detector ──

type nodeDetector struct{}

func (d *nodeDetector) Name() string { return "node" }

func (d *nodeDetector) Match(dir string) bool {
	return exists(filepath.Join(dir, "package.json"))
}

func (d *nodeDetector) Detect(dir string) *Project {
	p := &Project{
		Runtime: "node",
		Profile: "node",
	}

	p.PackageMgr = detectNodePkgMgr(dir)
	p.Framework = detectNodeFramework(dir)
	p.InstallCmd = buildInstallCmd(p.PackageMgr)
	p.DevCmd = buildDevCmd(p.PackageMgr, p.Framework, dir)
	p.Port = defaultPort(p.Framework)

	return p
}

func detectNodePkgMgr(dir string) string {
	checks := []struct {
		file string
		mgr  string
	}{
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"package-lock.json", "npm"},
	}
	for _, c := range checks {
		if exists(filepath.Join(dir, c.file)) {
			return c.mgr
		}
	}
	return "npm"
}

func detectNodeFramework(dir string) string {
	configFiles := []struct {
		pattern   string
		framework string
	}{
		{"vite.config.*", "vite"},
		{"next.config.*", "nextjs"},
		{"astro.config.*", "astro"},
		{"nuxt.config.*", "nuxt"},
		{"svelte.config.*", "sveltekit"},
	}
	for _, c := range configFiles {
		matches, _ := filepath.Glob(filepath.Join(dir, c.pattern))
		if len(matches) > 0 {
			return c.framework
		}
	}

	deps := readDeps(dir)
	for _, fw := range []struct{ pkg, name string }{
		{"vite", "vite"},
		{"next", "nextjs"},
		{"astro", "astro"},
	} {
		if _, ok := deps[fw.pkg]; ok {
			return fw.name
		}
	}

	return "node"
}

func buildInstallCmd(mgr string) string {
	switch mgr {
	case "yarn":
		return "yarn install"
	case "pnpm":
		return "pnpm install"
	case "bun":
		return "bun install"
	default:
		return "npm install --legacy-peer-deps"
	}
}

func buildDevCmd(mgr, framework, dir string) string {
	run := mgr + " run dev"
	switch mgr {
	case "yarn":
		run = "yarn dev"
	case "bun":
		run = "bun run dev"
	case "pnpm":
		run = "pnpm dev"
	}

	if framework == "node" {
		scripts := readScripts(dir)
		if _, ok := scripts["dev"]; ok {
			return run
		}
		if _, ok := scripts["start"]; ok {
			switch mgr {
			case "yarn":
				return "yarn start"
			case "bun":
				return "bun start"
			case "pnpm":
				return "pnpm start"
			default:
				return "npm start"
			}
		}
		return "node ."
	}

	return run
}

func defaultPort(framework string) int {
	switch framework {
	case "vite", "sveltekit":
		return 5173
	case "nextjs", "nuxt", "node":
		return 3000
	case "astro":
		return 4321
	default:
		return 3000
	}
}

type pkgJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Scripts         map[string]string `json:"scripts"`
}

func readPkgJSON(dir string) *pkgJSON {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return &pkgJSON{}
	}
	var p pkgJSON
	json.Unmarshal(data, &p)
	return &p
}

func readDeps(dir string) map[string]string {
	p := readPkgJSON(dir)
	merged := make(map[string]string)
	for k, v := range p.Dependencies {
		merged[k] = v
	}
	for k, v := range p.DevDependencies {
		merged[k] = v
	}
	return merged
}

func readScripts(dir string) map[string]string {
	return readPkgJSON(dir).Scripts
}
