# Roadmap

## v0.1

macOS. Apple Virtualization.framework. Alpine Linux.

- `dew up` — zero-config project detection (Vite, Next.js, Astro, Nuxt, SvelteKit)
- Three profiles: minimal, node, standard (containerd)
- vsock exec with `--json`, `--events`, `--stream`
- Port forwarding, persistent disk, network isolation
- Auto-download assets on first use

## v0.2 (current)

- **Windows support** — WSL2 backend, `dew.exe` wrapper
- **Python profile** — Django, Flask, FastAPI, Streamlit detection
- **`dew up --with postgres,redis`** — services alongside your app
- **`dew down`** — explicit stop
- **CLI spinner** — step-by-step progress with 💧 ⚡ branding
- **Release workflow** — GitHub Actions builds all artifacts on tag push

## v0.3

- **`dew.toml`** — project config (port, env vars, services, scripts). Reproducible dev environments across teams.
- **`dew ps`** — list running VMs with resource usage
- **Pre-baked Node.js in initramfs** — first boot from 20s to 5s
- **Go detection** — detect `go.mod`, auto `go run`
- **`dew up --env KEY=VAL`** — pass environment variables to VM
- **Turbo kernel for all profiles** — ext4/overlay/packet built-in

## v0.4

- **MCP server** — Dew as a tool provider for AI agents (Claude Code, Cursor). Direct `dew.exec()` without subprocess.
- **`dew publish`** — share custom profiles via registry
- **Multi-VM** — run multiple dew instances (monorepo: frontend + backend)
- **`dew logs`** — stream all service logs
- **Official website** — dewvm.dev with asciinema demos
- **Rust / Deno detection**
