# Dew

[![CI](https://github.com/solcreek/dew/actions/workflows/ci.yml/badge.svg)](https://github.com/solcreek/dew/actions/workflows/ci.yml)
[![Release](https://github.com/solcreek/dew/actions/workflows/release.yml/badge.svg)](https://github.com/solcreek/dew/releases/latest)
[![npm](https://img.shields.io/npm/v/@solcreek/dew)](https://www.npmjs.com/package/@solcreek/dew)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Sandboxed Linux compute, agent-native and human-friendly.

Boot a real Linux environment locally. Drive it from a shell, a script,
or an agent.

```bash
$ dew up
  detected: vite (npm)
  ✓ http://localhost:5173

$ dew exec --json "go test ./..."
  {"stdout":"PASS\nok  ./...","exitCode":0}

$ dew app run ghost --port 3000
  ✓ http://localhost:3000
```

## Install

```bash
# macOS / Linux
curl -fsSL https://dewvm.dev/install.sh | sh

# macOS Homebrew
brew install solcreek/tap/dew

# npm (all platforms)
npm install -g @solcreek/dew

# Windows (PowerShell)
irm https://github.com/solcreek/dew/releases/latest/download/install.ps1 | iex
```

Then try:

```bash
dew run -- uname -a
# (first time: downloads kernel + minimal initramfs, ~15s; then boots a
#  real Linux VM and prints its uname)
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

Apps run in an isolated VM, not on your host. No extra runtime to install.

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

Build locally, deploy to any VPS. No extra runtime on the server.

```bash
dew build                             # package app (421KB tarball)
dew server create --provider hetzner  # provision VPS ($5/mo)
dew deploy 5.161.53.168               # deploy with SSE progress
```

The server runs `dew serve` (7.1MB Linux binary) — containerd for isolation, self-signed TLS, health checks.

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
Local                       Linux server
─────                       ────────────
dew                         dew (deploy receiver)
├── dew up                  ├── HTTP deploy API
├── dew app run             ├── containers
├── dew exec                ├── TLS
├── dew build               ├── process management
├── dew deploy ──────────→  └── health check
└── Linux VM
    └── containers inside
```

### Profiles

| Profile | Use case |
|---|---|
| minimal | Generic Linux shell, lightest footprint |
| node | Node.js / npm projects |
| python | Python projects |
| standard | App catalog, containers, services |

## Security

- Hardware-VM isolation. Network off until you flip `--network`. Input validated against path traversal, control characters, and injection.

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
  dew assets ...                 Manage VM images
  dew update                     Update to latest version

Output:
  --json        Machine-readable JSON (all commands)
  --events      NDJSON lifecycle stream
  --dry-run     Validate without executing
```

## License

MIT
