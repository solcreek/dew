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

Deploy. One binary for local dev and production.

- **`dew build`** — package app into deploy tarball (421KB typical). Detects runtime, runs build command, generates manifest.
- **`dew deploy <target>`** — HTTP POST tarball to remote `dew serve`. SSE progress stream. Fallback chain (HTTP → SSH tunnel → SCP).
- **`dew serve`** — production deploy receiver on VPS. Auto-installs containerd from static binaries. Apps run in containers (base image + bind-mounted tarball). Multi-app isolation via namespaces + cgroups.
- **`dew auth`** — credential management (`set`, `login`, `list`, `remove`). `crk_` prefixed tokens for secret scanner detection.
- **`dew env`** — remote env var management. Secrets never travel in the tarball.
- **`dew rollback`** — restore previous deploy. Zero re-upload.
- **Built-in reverse proxy** — hostname-based routing to app containers.
- **ACME / TLS** — automatic Let's Encrypt certificates via certmagic.
- **`dew domain`** — `add`, `remove`, `list`. DNS verification + auto cert issuance.
- **`dew.toml`** — project config (port, env vars, services, scripts). Same file for local dev and production deploy.

## v0.4

Dashboard and polish.

- **Dashboard** — built-in web UI (Go templates + htmx, `//go:embed`). App list, logs, resource usage, deploy history, env management.
- **`dew ps`** — list local VMs and remote apps with resource usage.
- **`dew logs`** — stream app logs from `dew serve`.
- **Service provisioning** — postgres, redis, mysql as containers alongside your app. Declared in `dew.toml`, provisioned automatically.
- **Resource limits** — per-app CPU and memory limits via cgroups.
- **`dew clone <url>`** — git clone + detect + up in one command.
- **`dew share`** — temporary public HTTPS URL via Cloudflare Quick Tunnel.

## v0.5

- **Go / Rust / Deno detection** — detect `go.mod`, `Cargo.toml`, `deno.json`.
- **MCP server** — Dew as a tool provider for AI agents. Direct `dew.exec()`, `dew.deploy()` without subprocess.
- **Pre-baked runtimes in initramfs** — first boot from 20s to 5s.
- **Multi-VM** — run multiple dew instances (monorepo: frontend + backend).
- **Official website** — dewvm.dev with asciinema demos.
