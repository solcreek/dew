# Roadmap

## v0.1 — VM Foundation

macOS. Apple Virtualization.framework. Alpine Linux.

- `dew up` — zero-config project detection (Vite, Next.js, Astro, Nuxt, SvelteKit)
- Three profiles: minimal, node, standard (containerd)
- vsock exec with `--json`, `--events`, `--stream`
- Port forwarding, persistent disk, network isolation
- Auto-download assets on first use

## v0.2 — Cross-platform

- Windows support (WSL2 backend)
- Python profile (Django, Flask, FastAPI, Streamlit)
- `dew up --with postgres,redis` — services alongside app
- `dew down`, CLI spinner, release workflow

## v0.3 — Deploy Pipeline

- `dew build` — package app for deployment
- `dew deploy` — tarball + image mode, SSE progress
- `dew serve` — production deploy receiver with TLS
- `dew share` — temporary public HTTPS URL
- `dew server create/list/destroy` — VPS provisioning via capstan
- `dew auth`, `dew env`, `dew rollback`
- Self-signed TLS, token hash, constant-time comparison

## v0.4 (current) — Apps + CLI Restructure

- **`dew app run/stop/list`** — run open-source apps from registry
- **`dew apps`** — browse 11 pre-packaged apps (dew-apps catalog)
- **`dew-serve` standalone binary** — cross-compiled Linux (7.1MB)
- **dew-virt kernel** — custom monolithic x86_64 (11MB, zero modules). ARM64 via Kata pre-built (15.4MB, 30ms boot)
- **CLI restructure** — Dev → Share → Apps → Deploy → Infrastructure → Advanced
- **Containers inside Dew VM** — no host Docker dependency
- **Static site detector** — index.html with busybox httpd
- **npm @solcreek/dew** — auto-download binary via postinstall

## v0.5 — Production Ready

- **`dew logs`** — stream app logs from `dew serve`
- **`dew app list --json`** — structured output for agents
- **ACME / TLS** — automatic Let's Encrypt certificates via certmagic
- **`dew domain add/remove`** — custom domains with auto cert issuance
- **`dew.toml`** — project config (port, env, services, volumes)
- **Reverse proxy** — hostname-based routing to app containers
- **dew-apps tarball builds** — pre-packaged tarballs (10x smaller than Docker images)

## v0.6 — Dashboard + Observability

- **Dashboard** — built-in web UI (Go templates + htmx)
- **`dew app list`** — resource usage (CPU, RAM, network)
- **Service provisioning** — postgres, redis as containers via `dew.toml`
- **Resource limits** — per-app CPU and memory via cgroups
- **`dew clone <url>`** — git clone + detect + up

## v0.7 — Agent + Ecosystem

- **MCP server** — Dew as tool provider for AI agents
- **Go / Rust / Deno detection**
- **`dew build --workspace`** — monorepo support
- **`dew share` stable subdomain** — Creek Cloud relay
- **Grove desktop app** — Tauri GUI for non-developers
- **Official website** — dewvm.dev
