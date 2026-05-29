# Dew

[![CI](https://github.com/solcreek/dew/actions/workflows/ci.yml/badge.svg)](https://github.com/solcreek/dew/actions/workflows/ci.yml)
[![Release](https://github.com/solcreek/dew/actions/workflows/release.yml/badge.svg)](https://github.com/solcreek/dew/releases/latest)
[![npm](https://img.shields.io/npm/v/@solcreek/dew)](https://www.npmjs.com/package/@solcreek/dew)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Run any app, anywhere. Dev environments, app runner, deploy tool — one binary.

```bash
$ dew app run excalidraw --port 3000
  excalidraw v0.18.0
  Starting excalidraw ✓
  ✓ http://localhost:3000
```

```bash
$ dew up
  detected: vite (npm)
  ✓ http://localhost:5173
```

No Docker. No Homebrew. One 4MB binary.

## Install

```bash
# macOS / Linux
curl -fsSL https://github.com/solcreek/dew/releases/latest/download/install.sh | sh

# Windows (PowerShell)
irm https://github.com/solcreek/dew/releases/latest/download/install.ps1 | iex

# npm (all platforms)
npx @solcreek/dew --help
```

## What it does

### Run open-source apps

Browse and run apps from the [dew-apps](https://github.com/solcreek/dew-apps) catalog.

```bash
dew apps                              # browse 11 available apps
dew app run ghost --port 3000         # run Ghost blog
dew app run uptime-kuma --port 3001   # run Uptime Kuma
dew app list                          # see what's running
dew app stop ghost                    # stop an app
```

Apps run in an isolated VM — not on your host. No Docker required.

### Dev environments

Auto-detects your project and starts a dev environment with hot reload.

```bash
cd my-vite-app
dew up                               # detect, boot, install, start
dew up --with postgres,redis          # dev with services
dew down                             # stop
```

Supports: Vite, Next.js, Astro, Nuxt, SvelteKit, Django, Flask, FastAPI, static HTML.

### Share instantly

Temporary public HTTPS URL for any local port. Zero config, zero account.

```bash
dew share 3000
  ✓ https://random-words.trycloudflare.com
  Press Ctrl+C to stop
```

### Deploy to a VPS

Build locally, deploy to any VPS. No Docker on the server.

```bash
dew build                             # package app (421KB tarball)
dew server create --provider hetzner  # provision VPS ($5/mo)
dew deploy 5.161.53.168               # deploy with SSE progress
```

The server runs `dew serve` (7.1MB Linux binary) — containerd for isolation, self-signed TLS, health checks, rollback.

## Agent integration

Every command supports `--json` for machine-readable output and `--dry-run` for validation without execution.

```bash
dew app run ghost --port 3000 --json
# {"ok":true,"app":"ghost","port":3000,"url":"http://localhost:3000"}

dew deploy prod --dry-run
# Would deploy my-app.tar.gz to https://prod:9080
# No changes made.

dew run --json -- npm test
# {"exit_code":0,"stdout":"5 passing\n"}
```

Input hardening: rejects path traversal, query injection, control characters. `--events` for NDJSON lifecycle streaming.

## Architecture

```
macOS/Windows              Linux VPS
─────────────              ─────────
dew (4MB binary)           dew (7MB Linux binary)
├── dew up                 ├── HTTP deploy API
├── dew app run            ├── containerd + nerdctl
├── dew build              ├── self-signed TLS
├── dew deploy ──────────→ ├── process management
├── dew share              └── health check + rollback
└── Apple VZ / WSL2 VM
    └── containerd inside
```

### Kernel

macOS VMs use a custom monolithic kernel (dew-virt, 11MB x86_64 / 15MB ARM64). All containerd features built-in — zero module loading, zero compatibility issues with Apple Virtualization.framework.

- x86_64: custom build from `config-dew-virt-x86_64.fragment`
- ARM64: Kata Containers pre-built kernel (30ms boot, 28MB RAM)

### Profiles

| Profile | Contents | Size | RAM |
|---|---|---|---|
| minimal | Alpine + exec agent | 5MB | 512MB |
| node | + Node.js, npm, build tools | 31MB | 1GB |
| python | + Python 3, pip | 31MB | 1GB |
| standard | + containerd, nerdctl, runc | 129MB | 2GB |

## Security

- **Signed + notarized** — Developer ID, hardened runtime, Apple notarization
- **VM isolation** — Apple Virtualization.framework hardware boundary
- **Self-signed TLS** — auto-generated ECDSA cert for deploy API
- **Token hash** — SHA-256 in cloud-init, not plaintext
- **Constant-time comparison** — timing-safe token verification
- **Input hardening** — reject path traversal, control chars, query injection
- **Network off by default** — no NIC unless `--network`
- **Capability drop** — guest agent drops root after vsock bind
- **`--dry-run`** — validate without executing

## Full command reference

```
Dev:
  dew up [dir]                   Start dev environment
  dew up --with postgres,redis   Dev with services
  dew down                       Stop dev environment

Share:
  dew share [port]               Temporary public HTTPS URL

Apps:
  dew apps                       Browse available apps
  dew app run <name> [--port N]  Run an app
  dew app stop <name>            Stop an app
  dew app list [--json]          Show running apps

Deploy:
  dew build [dir]                Package app for deployment
  dew deploy <target>            Deploy to remote server
  dew rollback <target> <app>    Restore previous version
  dew env ...                    Manage environment variables
  dew auth ...                   Manage credentials

Infrastructure:
  dew server create [--provider]  Provision a VPS
  dew server list                 List managed servers
  dew server destroy <name>       Remove a server
  dew serve                       Run deploy receiver (VPS)

Advanced:
  dew run [--] <cmd>             Execute in ephemeral VM
  dew exec <cmd>                 Execute in running VM
  dew session ...                Persistent VM sessions
  dew assets ...                 Manage VM images
  dew update                     Update to latest version

Output:
  --json        Machine-readable JSON (all commands)
  --events      NDJSON lifecycle stream
  --dry-run     Validate without executing
```

## License

MIT
