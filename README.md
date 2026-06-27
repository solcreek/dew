# Dew

[![CI](https://github.com/solcreek/dew/actions/workflows/ci.yml/badge.svg)](https://github.com/solcreek/dew/actions/workflows/ci.yml)
[![Release](https://github.com/solcreek/dew/actions/workflows/release.yml/badge.svg)](https://github.com/solcreek/dew/releases/latest)
[![npm](https://img.shields.io/npm/v/dew)](https://www.npmjs.com/package/dew)
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
```

## Install

```bash
# macOS / Linux
curl -fsSL https://dewvm.dev/install.sh | sh

# macOS Homebrew
brew install solcreek/tap/dew

# npm (all platforms)
npm install -g dew

# Windows (PowerShell)
irm https://github.com/solcreek/dew/releases/latest/download/install.ps1 | iex
```

> One-off try-out without installing: `npx dew run -- uname -a` works too.
> Once dew is installed (brew, install.sh, or `npm install -g`), call `dew`
> directly — `npx dew` always resolves the npm package and ignores the
> binary already on your PATH, adding startup overhead on every call.

Then try:

```bash
dew run -- uname -a
# (first time: downloads kernel + minimal initramfs, ~15s; then boots a
#  real Linux VM and prints its uname)
```

## What it does

### Dev environments

Auto-detects your project and starts a dev environment with hot reload.

```bash
cd my-vite-app
dew up                               # detect, boot, install, start
dew up --with postgres,redis          # dev with built-in services
dew down                             # stop
```

Supports: Vite, Next.js, Astro, Nuxt, SvelteKit, Django, Flask, FastAPI, static HTML.

For arbitrary services (any OCI image) in the same VM, add a `dew.toml`
(`dew up --init` writes a starter). Auto-detection still works without it.

```toml
# dew.toml
[project]
profile = "node"

[dev]
command = "npm run dev"
port = 3000

[[service]]
name = "redis"
image = "redis:7-alpine"
port = 6379

[[service]]
name = "mailpit"
image = "axllent/mailpit:latest"
port = 8025
```

`dew up` then boots the project and its services in one VM; containers use
the VM network, so the dev server reaches them on `localhost`.

`--services-only` (alias `--no-dev`) boots just the services — handy when
the app itself runs on the host and only its backing services live in dew.
This is the docker-compose-shaped path: several arbitrary OCI images,
health-gated and port-forwarded, in one microVM with no container daemon.

### Reach the macOS host from the VM

A service in the VM often has to call back to a process on your Mac — a
websocket gateway hitting a host RPC, an app talking to a database you run
natively. dew gives you two host aliases so you never hardcode the
`192.168.64.x` gateway IP. Both resolve inside the guest **and** inside
`--net=host` containers, so a `dew.toml` service reaches the host the same
way the guest does.

| Alias | Reaches a host service bound to | Path | Use when |
|---|---|---|---|
| `host.internal` (alias `host.dew.internal`) | `0.0.0.0` (all interfaces) | VZ NAT gateway | the host service already listens on all interfaces |
| `host.lo.internal` | **`127.0.0.1`** (the dev default) | vsock → host loopback | the service is loopback-only, or you want to dodge the macOS 26 VZ NAT regression |

**`host.internal` — the NAT-gateway alias**, dew's equivalent of
`host.docker.internal`. The host service must bind `0.0.0.0`; a `127.0.0.1`
bind stays unreachable (same caveat as docker's alias):

```bash
# host process on 0.0.0.0:50051 → from the VM: host.internal:50051
```

**`host.lo.internal` — a vsock tunnel straight to the host's loopback.**
This is the one for the common dev case where the host service is bound to
`127.0.0.1` (a Rails RPC, a local Postgres, etc.). Declare the ports to
expose; dew forwards each over vsock to the host's `127.0.0.1` — no
`0.0.0.0` rebind, and because it never touches the network stack it is
**immune to the macOS 26 VZ NAT regression**:

```bash
dew up --expose-host 50051
# in the VM: host.lo.internal:50051 → the host's 127.0.0.1:50051
```

```toml
# or, in dew.toml:
[host]
expose = [50051]
```

dew only ever dials its own loopback on a port you declared, so the VM can't
reach anything else on the host.

### Run any container

Run a single OCI image in a microVM — no Docker daemon, sub-second boot,
automatic `--net=host` and port forwarding:

```bash
dew run --image redis:7-alpine -p 6379:6379
dew run --image axllent/mailpit -p 8025:8025 -e MP_SMTP_AUTH_ACCEPT_ANY=1
```

Each `dew run` is ephemeral and isolated, so several run side by side
without contending for a shared disk. For a long-lived VM that persists
state, `--name` gives it its own disk so named instances coexist:

```bash
dew vm start --name ci --profile standard   # a second VM alongside the default
dew exec --name ci -- ./run-tests.sh
dew vm stop --name ci
```

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

`dew server create` auto-discovers an SSH public key from
`~/.ssh/id_ed25519.pub` (or `id_rsa.pub`) and locks root password
auth in the same boot. Override with `--ssh-key`, the `DEW_SSH_KEY`
env, or `--no-ssh-key` to keep the provider's emailed password.

> **DigitalOcean Web Console caveat:** if you ever need to paste a
> long command (an SSH key, a recovery script) into DO's browser
> console, note that it does **not** support bracketed paste — long
> lines wrap and corrupt mid-paste. Prefer `--ssh-key` at create
> time so SSH works from the first boot and the console isn't
> needed.

## Agent integration

Every command supports `--json` for machine-readable output and `--dry-run` for validation without execution.

```bash
dew up --dry-run --json
# {"type":"dry-run","framework":"vite","profile":"node",...}

dew deploy prod --dry-run
# Would deploy my-app.tar.gz to https://prod:9080
# No changes made.

dew run --json -- npm test
# {"exit_code":0,"stdout":"5 passing\n"}
```

Input hardening: rejects path traversal, query injection, control characters. `--events` for NDJSON lifecycle streaming.

### AI agent skills

dew ships [agentskills.io](https://agentskills.io)-compatible skill files
that teach coding agents (Claude Code, Cursor, Codex, Copilot, Gemini CLI,
70+ others) how to use the CLI correctly — including when *not* to reach
for it.

```bash
npx skills add solcreek/dew
```

Installs four skills: `dew-server-create`, `dew-deploy`, `dew-diagnose`,
`dew-upgrade`. Source lives in [`skills/`](./skills/) and versions with
each release.

## Architecture

```
Local                       Linux server
─────                       ────────────
dew                         dew (deploy receiver)
├── dew up                  ├── HTTP deploy API
├── dew run                 ├── containers
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
| standard | Containers, services |

### Running x86_64 (amd64) containers on Apple Silicon

Add `--rosetta` to mount Apple's Rosetta translator into the guest and register
`binfmt_misc`, so amd64 binaries and containers run transparently under
translation:

```bash
dew run --profile standard --network --rosetta -- \
  nerdctl run --rm --platform linux/amd64 alpine uname -m   # -> x86_64
```

Apple Silicon only (Intel Macs have no Rosetta-for-Linux). Performance depends
on the workload's hot path: roughly 70-80% of native for ordinary compiled
code, less for interpreter-heavy or crypto/SIMD-heavy code. For peak
performance prefer a native `linux/arm64` image.

## Security

- Hardware-VM isolation. Network off until you flip `--network`. Input validated against path traversal, control characters, and injection.

## Full command reference

```
Dev:
  dew up [dir]                   Start dev environment
  dew up --with postgres,redis   Dev with built-in services
  dew up --init                  Write a starter dew.toml
  dew up --services-only         Boot services without a dev server
  dew up --expose-host PORT      Reach a host 127.0.0.1 service in the VM
  dew down                       Stop dev environment

Share:
  dew share [port]               Temporary public HTTPS URL

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
  dew run --image <ref>          Run an OCI image in a microVM
  dew exec [--name <vm>] <cmd>   Execute in a running VM
  dew vm start/stop [--name]     Manage long-lived (named) VMs
  dew assets ...                 Manage VM images
  dew update                     Update to latest version

Output:
  --json        Machine-readable JSON (all commands)
  --events      NDJSON lifecycle stream
  --dry-run     Validate without executing
```

## License

MIT
