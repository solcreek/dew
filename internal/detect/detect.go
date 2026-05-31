// Package detect inspects a project directory and determines the
// runtime, framework, package manager, dev command, and port.
package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Project struct {
	Dir        string
	Runtime    string // "node", "python", "go", "rust"
	Framework  string // "vite", "nextjs", "astro", "nuxt", "sveltekit", "node", "django", "flask"
	PackageMgr string // "npm", "yarn", "pnpm", "bun", "pip", "go"
	DevCmd     string // full command to start dev server
	BuildCmd   string // full command to build for production
	InstallCmd string // full command to install dependencies
	Port       int
	Entry      string // production entry point (e.g. "server.ts")
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
	Register(&pythonDetector{})
	Register(&staticDetector{})
}

// ── Static site detector ──

type staticDetector struct{}

func (d *staticDetector) Name() string { return "static" }

func (d *staticDetector) Match(dir string) bool {
	return exists(filepath.Join(dir, "index.html"))
}

func (d *staticDetector) Detect(dir string) *Project {
	return &Project{
		Runtime: "static",
		Profile: "minimal",
		Port:    8080,
		Entry:   "index.html",
		DevCmd:  "httpd -f -p 8080 -h .",
	}
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
	p.BuildCmd = buildBuildCmd(p.PackageMgr, dir)
	p.Port = defaultPort(p.Framework)
	p.Entry = detectEntry(dir)

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

func buildBuildCmd(mgr string, dir string) string {
	scripts := readScripts(dir)
	if _, ok := scripts["build"]; ok {
		switch mgr {
		case "yarn":
			return "yarn build"
		case "bun":
			return "bun run build"
		case "pnpm":
			return "pnpm build"
		default:
			return "npm run build"
		}
	}
	return ""
}

func detectEntry(dir string) string {
	candidates := []string{"server.ts", "server.js", "index.ts", "index.js", "main.ts", "main.js", "app.ts", "app.js"}
	for _, c := range candidates {
		if exists(filepath.Join(dir, c)) {
			return c
		}
	}
	pkg := readPkgJSON(dir)
	if pkg.Scripts != nil {
		if start, ok := pkg.Scripts["start"]; ok {
			parts := strings.Fields(start)
			if len(parts) >= 2 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
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
			return appendHostArg(run, mgr, framework)
		}
		if _, ok := scripts["start"]; ok {
			switch mgr {
			case "yarn":
				return appendHostArg("yarn start", mgr, framework)
			case "bun":
				return appendHostArg("bun start", mgr, framework)
			case "pnpm":
				return appendHostArg("pnpm start", mgr, framework)
			default:
				return appendHostArg("npm start", mgr, framework)
			}
		}
		return "node ."
	}

	return appendHostArg(run, mgr, framework)
}

// appendHostArg makes the dev server bind to 0.0.0.0 so the host port
// forward can reach it inside the VM. Vite, SvelteKit, Astro, Nuxt and
// Next.js each accept this differently; npm and yarn need `--` to
// forward arguments to the underlying script while pnpm/bun forward
// them transparently.
func appendHostArg(cmd, mgr, framework string) string {
	var arg string
	switch framework {
	case "nextjs":
		arg = "-H 0.0.0.0"
	default:
		// vite, sveltekit, astro, nuxt all accept `--host 0.0.0.0`.
		arg = "--host 0.0.0.0"
	}
	switch mgr {
	case "npm", "yarn":
		return cmd + " -- " + arg
	default:
		return cmd + " " + arg
	}
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

// ── Python detector ──

type pythonDetector struct{}

func (d *pythonDetector) Name() string { return "python" }

func (d *pythonDetector) Match(dir string) bool {
	return exists(filepath.Join(dir, "pyproject.toml")) ||
		exists(filepath.Join(dir, "requirements.txt")) ||
		exists(filepath.Join(dir, "setup.py")) ||
		exists(filepath.Join(dir, "Pipfile"))
}

func (d *pythonDetector) Detect(dir string) *Project {
	p := &Project{
		Runtime: "python",
		Profile: "python",
	}

	p.PackageMgr = detectPythonPkgMgr(dir)
	p.Framework = detectPythonFramework(dir)
	p.InstallCmd = buildPythonInstallCmd(p.PackageMgr, dir)
	p.DevCmd = buildPythonDevCmd(p.Framework, dir)
	p.Port = defaultPythonPort(p.Framework)

	return p
}

func detectPythonPkgMgr(dir string) string {
	if exists(filepath.Join(dir, "Pipfile")) {
		return "pipenv"
	}
	if exists(filepath.Join(dir, "poetry.lock")) {
		return "poetry"
	}
	if exists(filepath.Join(dir, "uv.lock")) {
		return "uv"
	}
	return "pip"
}

func detectPythonFramework(dir string) string {
	// Check requirements.txt for framework hints
	for _, reqFile := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
		data, err := os.ReadFile(filepath.Join(dir, reqFile))
		if err != nil {
			continue
		}
		content := string(data)
		if contains(content, "django") {
			return "django"
		}
		if contains(content, "flask") {
			return "flask"
		}
		if contains(content, "fastapi") || contains(content, "uvicorn") {
			return "fastapi"
		}
		if contains(content, "streamlit") {
			return "streamlit"
		}
	}

	if exists(filepath.Join(dir, "manage.py")) {
		return "django"
	}
	if exists(filepath.Join(dir, "app.py")) {
		return "flask"
	}

	return "python"
}

func contains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func buildPythonInstallCmd(mgr string, dir string) string {
	switch mgr {
	case "poetry":
		return "poetry install"
	case "pipenv":
		return "pipenv install"
	case "uv":
		return "uv sync"
	default:
		if exists(filepath.Join(dir, "requirements.txt")) {
			return "pip install -r requirements.txt"
		}
		if exists(filepath.Join(dir, "pyproject.toml")) {
			return "pip install -e ."
		}
		return "pip install -e ."
	}
}

func buildPythonDevCmd(framework string, dir string) string {
	switch framework {
	case "django":
		return "python manage.py runserver 0.0.0.0:8000"
	case "flask":
		return "flask run --host=0.0.0.0"
	case "fastapi":
		return "uvicorn main:app --host 0.0.0.0 --port 8000"
	case "streamlit":
		return "streamlit run app.py --server.address 0.0.0.0"
	default:
		if exists(filepath.Join(dir, "app.py")) {
			return "python app.py"
		}
		if exists(filepath.Join(dir, "main.py")) {
			return "python main.py"
		}
		return "python -m http.server 8000"
	}
}

func defaultPythonPort(framework string) int {
	switch framework {
	case "django", "fastapi":
		return 8000
	case "flask":
		return 5000
	case "streamlit":
		return 8501
	default:
		return 8000
	}
}
