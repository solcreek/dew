# App Deployment Test Suite

Real-world apps for validating `dew build`, `dew deploy`, and `dew serve`.

## Test Matrix

### Tier 1: Smoke test (every release)

Simplest apps, fast to test, cover the critical paths.

| App | Type | Deploy Mode | Deps | Health Check | Status |
|---|---|---|---|---|---|
| demo-vite-sqlite | server (Bun+SQLite) | tarball | none | `GET /api/health` | ✅ validated |
| open-slide | static (Vite+React) | tarball | none | `GET /` → 200 | ✅ validated |
| Excalidraw | static | image `excalidraw/excalidraw` | none | `GET /` → 200 | ✅ validated |
| Uptime Kuma | server (Node+SQLite) | image `louislam/uptime-kuma:2` | none | `GET /` → 200 | pending |
| Vaultwarden | server (Rust+SQLite) | image `vaultwarden/server` | none | `GET /alive` → 200 | pending |

### Tier 2: Integration test (weekly / pre-release)

Apps with external deps or complex setup.

| App | Type | Deploy Mode | Deps | Health Check | Status |
|---|---|---|---|---|---|
| AnythingLLM | server (Node+SQLite) | image `mintplexlabs/anythingllm` | volume mount | `GET /api/health` | ✅ validated |
| PocketBase | server (Go+SQLite) | binary download | none | `GET /api/health` | pending |
| Gitea | server (Go+SQLite) | image `gitea/gitea` | none (SQLite mode) | `GET /` → 200 | pending |
| Ghost | server (Node+SQLite) | image `ghost` | none (dev mode) | `GET /ghost/api/admin/site/` | pending |
| Miniflux | server (Go+Postgres) | image `miniflux/miniflux` | postgres | `GET /healthcheck` | pending |

### Tier 3: Stress test (milestone releases)

Heavy apps, multiple services, complex orchestration.

| App | Type | Deploy Mode | Deps | Notes |
|---|---|---|---|---|
| Outline | server (Node+React) | image `outlinewiki/outline` | postgres, redis, S3 | Multi-service compose |
| Immich | server (Node+ML) | image `ghcr.io/immich-app/immich-server` | postgres, redis, pgvector | Heavy, photo management |
| Authentik | server (Python+Django) | image `ghcr.io/goauthentik/server` | postgres | Auth platform |

## What each tier validates

| Tier | What it tests |
|---|---|
| **Tier 1** | `dew build` (tarball + static), `dew deploy` (tarball + image), `dew serve` (receive + extract), `dew share` (tunnel) |
| **Tier 2** | Volume mounts, env vars, database persistence, `dew deploy --image` with config |
| **Tier 3** | Multi-service (`--with postgres,redis`), compose-equivalent, resource limits |

## Deploy modes tested

| Mode | Example | Validated? |
|---|---|---|
| `dew build` → tarball → `dew deploy` | demo-vite-sqlite, open-slide | ✅ |
| `dew deploy --image` | Excalidraw, AnythingLLM | ✅ |
| `dew deploy --image` + env vars | AnythingLLM (STORAGE_DIR) | ✅ |
| `dew deploy --image` + volume | AnythingLLM | ✅ |
| `dew deploy --image` + postgres | Miniflux | pending |
| `dew share` | Excalidraw | ✅ |

## Running tests

```bash
# Tier 1: smoke test (requires dew binary + Docker)
bash test/apps/smoke.sh

# Individual app
bash test/apps/smoke.sh excalidraw
bash test/apps/smoke.sh uptime-kuma
```

## App-specific notes

### demo-vite-sqlite
Source: `/Users/linyiru/Projects/creek/demo-vite-sqlite`
Stack: Bun + Hono + React + SQLite. Full CRUD (notes app).
Build: `dew build` → 77KB tarball. Deploy: tarball mode.

### open-slide
Source: `npx @open-slide/cli init my-slide`
Stack: Vite + React. Static site (presentation framework).
Build: `dew build` → 395KB tarball (`type: "static"`).
Issue found: `.claude/` dir crash (fixed), dist/ gitignore exclusion (fixed).

### Excalidraw
Image: `excalidraw/excalidraw`
Stack: nginx serving static React build.
Port: 80. RAM: 4.4MB.
Validated via `dew share` with Cloudflare Quick Tunnel.

### AnythingLLM
Image: `mintplexlabs/anythingllm`
Stack: Node.js + Prisma + SQLite.
Port: 3001. Requires: `STORAGE_DIR=/app/server/storage`, volume mount, run as root.
Issue found: Prisma relative path + file permissions.

### Uptime Kuma
Image: `louislam/uptime-kuma:2`
Stack: Node.js + Vue 3 + SQLite.
Port: 3001. Simple single-container deploy.

### Vaultwarden
Image: `vaultwarden/server`
Stack: Rust + SQLite. Bitwarden-compatible password manager.
Port: 80. Health: `GET /alive`. Minimal resource usage.
