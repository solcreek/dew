# Dew

A 4MB tool that boots a Linux VM in under a second.

```
$ dew up

  detected: vite (npm)
  profile:  node
  port:     5173

  booting... 1.2s
  installing deps... 3.4s
  waiting for server... ok

  ✓ http://localhost:5173
```

No Node.js on your Mac. No Docker. No Homebrew. One command, and your project runs in an isolated Linux VM with hardware-level isolation.

## Why Dew

**Dew boots a full Linux VM in 850ms.** Your code runs inside the VM. Dependencies install inside the VM. Build artifacts stay inside the VM. Your Mac stays clean — no node_modules, no global packages, no version conflicts.

- **850ms cold boot, 50ms warm exec** — benchmarked on M1 MacBook Pro, Apple Virtualization.framework
- **4.4MB binary** — no Docker Desktop (700MB), no background daemon when stopped
- **Zero config** — `dew up` detects your project (Vite, Next.js, Astro, Nuxt, SvelteKit) and starts everything
- **VM isolation** — Apple Virtualization.framework hardware boundary, not container namespaces
- **Three network modes** — fully isolated (no NIC), host-only (vsock), or NAT. No network by default.

### Built for AI agents

AI coding agents (Claude Code, Cursor, Windsurf) generate code but need somewhere safe to run it. Dew provides structured, machine-readable APIs for every step:

```bash
# Agent runs generated code, gets structured result
dew run --json -- "npm test"
# {"exit_code":0,"stdout":"5 passing\n","stderr":""}

# Agent monitors build progress in real time
dew up --events my-app/
# {"type":"detect","framework":"vite","pkg_mgr":"npm","port":5173}
# {"type":"boot","status":"ready","elapsed_ms":1200}
# {"type":"install","status":"done","elapsed_ms":12000}
# {"type":"health","status":"ok","url":"http://localhost:5173/"}

# When something fails, agents get actionable suggestions
# {"type":"install","status":"failed","error":"peer dep conflict","suggestion":"try --legacy-peer-deps"}
```

No cloud API key. No network round-trip. Works offline. Data never leaves your machine.

## Install

```bash
# Homebrew (recommended)
brew tap solcreek/dew && brew install dew

# npm
npm install -g @solcreek/dew

# Or download directly
curl -fsSL https://github.com/solcreek/dew/releases/latest/download/install.sh | sh
```

## Quick Start

### Start a project (zero config)

```bash
cd my-vite-app
dew up
# → detects Vite + npm, boots VM, installs deps, starts dev server
# → http://localhost:5173
```

### Run a single command

```bash
dew run -- uname -a
# Linux dew 6.12.91-0-virt x86_64 Linux
```

### Start with services

```bash
dew up --with postgres,redis
# → boots VM, starts Postgres (5432) + Redis (6379), starts dev server
# → psql -h localhost -p 5432, redis-cli -p 6379

dew down  # stop everything
```

Available services: postgres, redis, mysql, mongo, minio.

### Run Postgres manually (more control)

```bash
dew start --network --forward 5432:5432
dew exec "nerdctl run -d --net=host postgres:16-alpine"
psql -h localhost -p 5432 -U postgres
```

## `dew up`

Auto-detects your project and starts a dev environment:

| Detected by | Framework | Port | Install command |
|---|---|---|---|
| `vite.config.*` | Vite | 5173 | `npm install` / `yarn` / `pnpm` / `bun` |
| `next.config.*` | Next.js | 3000 | (from lock file) |
| `astro.config.*` | Astro | 4321 | |
| `nuxt.config.*` | Nuxt | 3000 | |
| `svelte.config.*` | SvelteKit | 5173 | |
| `package.json` | Node.js | 3000 | |
| `requirements.txt` | Django / Flask / FastAPI | 8000 / 5000 | `pip install -r requirements.txt` |
| `pyproject.toml` | Python | 8000 | `pip install -e .` / `poetry install` |

Your project directory syncs live between macOS and the VM. Edit files on your Mac, the dev server picks up changes instantly. `node_modules` and build artifacts stay inside the VM — your project directory stays clean.

npm cache persists on disk. Second run installs dependencies in seconds.

## Profiles

| Profile | What's inside | Download | Default RAM | Disk |
|---|---|---|---|---|
| **minimal** | Alpine Linux + exec agent | 5MB | 512MB | none (tmpfs) |
| **node** | + Node.js, npm, build tools | 31MB | 1GB | 4GB (auto) |
| **python** | + Python 3, pip, build tools | 31MB | 1GB | 4GB (auto) |
| **standard** | + containerd, nerdctl, runc, CNI | 129MB | 2GB | 10GB (auto) |

`dew up` selects the profile automatically. Node projects → `node`. Python projects → `python`. `--with` services → `standard`. Pure sandboxing → `minimal`.

Node and standard profiles create a persistent disk automatically at `~/.local/share/dew/`. No flags needed.

## Commands

```
dew up [dir]                   Auto-detect and start dev environment
dew down                       Stop the running VM
dew start [flags]              Boot a VM (persistent, daemon socket)
dew run [flags] [--] <cmd>     Boot, exec, exit (ephemeral)
dew exec <cmd>                 Exec in a running VM (from any terminal)
dew assets pull                Download VM image for current profile
dew session create [flags]     In-process session (~50ms per exec)
dew session exec <id> <cmd>    Exec in session
dew session destroy <id>       Destroy session
dew version                    Print version
```

## Flags

```
--profile <name>     minimal, node, or standard (default)
--cpus <n>           vCPUs (default: 1)
--memory <mb>        Memory in MB (auto per profile)
--network            Enable NAT networking
--disk <path>        Persistent disk (auto per profile)
--forward <h:g>      Forward host port to guest (repeatable)
--share <tag:path>   Share host dir (read-only default, :rw for write)
--stream             Real-time stdout/stderr
--events             NDJSON lifecycle event stream
--json               Structured JSON output
```

## Agent Integration

### Structured output

```bash
dew up --json my-app/
```
```json
{"status":"ready","url":"http://localhost:5173/","port":5173,"framework":"vite","elapsed_ms":15200}
```

### Lifecycle events

```bash
dew up --events my-app/
```
```json
{"type":"detect","framework":"vite","pkg_mgr":"npm","port":5173}
{"type":"boot","status":"ready","elapsed_ms":1200}
{"type":"install","status":"done","elapsed_ms":12000}
{"type":"health","status":"ok","url":"http://localhost:5173/","elapsed_ms":15200}
```

### Error fallback

Each step can fail with a structured error and an actionable suggestion:

```json
{"type":"install","status":"failed","error":"peer dep conflict","suggestion":"try --legacy-peer-deps"}
```

Agents parse the `suggestion` field and retry automatically.

### Exec API (boot once, exec many)

```bash
dew start --network --forward 3000:3000
dew exec "npm install"    # 50ms
dew exec "npm test"       # 50ms
dew exec "npm run build"  # 50ms
```

50ms per exec because the VM stays running. No boot overhead after the first `dew start`.

## Network Isolation

```bash
# Fully isolated (default) — VM has no network device at all
dew run -- "curl google.com"         # fails: no NIC

# NAT — guest can reach the internet
dew run --network -- "curl google.com"  # works

# Host-only — vsock channel, no IP stack
dew start --vsock 1024               # exec works, no internet
```

## Port Forwarding

Through vsock — no SSH tunnel, no NAT port mapping:

```bash
dew start --network --forward 5432:5432 --forward 6379:6379

# From any terminal, any app on the host
psql -h localhost -p 5432
redis-cli -p 6379
```

## Persistent Data

```bash
dew start --disk ./dev.img
dew exec "echo 'hello' > /data/greeting.txt"

# Restart — data is still there
dew start --disk ./dev.img
dew exec "cat /data/greeting.txt"  # hello
```

Node and standard profiles create persistent disks automatically. npm cache, installed packages, and database files survive across VM restarts.

## Benchmarks

Measured on M1 MacBook Pro, macOS 14, Apple Virtualization.framework.

| | Cold boot → exec | Warm exec | Binary | Footprint |
|---|---|---|---|---|
| **Dew minimal** | **850ms** | **50ms** | 4.4MB | 5MB |
| **Dew node** | **~3s** | **50ms** | 4.4MB | 31MB + 4GB disk |
| **Dew standard** | **~5s** (first) | **50ms** | 4.4MB | 129MB + 10GB disk |
| Docker Desktop | 30s+ | ~100ms | — | 700MB + GB disk |
| Lima | 33s | 130ms | 33MB | 33MB + 8.7GB disk |

## Security

- **VM isolation** — Apple Virtualization.framework hardware boundary
- **Auth token** — per-VM random token injected via vsock handshake (never written to disk or kernel cmdline)
- **Capability drop** — guest agent drops root capabilities after binding vsock
- **Cgroup limits** — configurable CPU/memory limits per VM
- **Read-only shares** — `--share` defaults to read-only; explicit `:rw` required for write access
- **Per-exec timeout** — commands killed after deadline (default 30s)
- **Network off by default** — VM has no NIC unless `--network` is passed

## Limitations

- **macOS + Windows** — macOS uses Apple Virtualization.framework (macOS 13+). Windows uses WSL2 (`dew setup` to install). Linux doesn't need a VM (use containerd directly).
- **Hot reload** — works with `dew up` and minimal profile + `--share`. Standard profile containers require image rebuild for code changes.
- **No docker-compose equivalent** — use multiple `dew exec "nerdctl run ..."` commands, or `dew up` for single-app projects.
- **First boot is slower** — standard profile formats a disk and installs Node.js on first use (~20s). Subsequent boots use the cached disk (~5s).

## Architecture

```
cmd/dew/             CLI (macOS host)
cmd/dew-agent/       Guest agent (Linux, static binary, runs inside VM)
internal/vm/         Platform-agnostic VM interface
internal/vm/darwin/  Apple Virtualization.framework backend
internal/vsock/      Length-prefixed JSON protocol over vsock
internal/detect/     Project auto-detection (registry-based, extensible)
internal/session/    Long-lived VM session management
internal/daemon/     Unix socket for cross-process exec
initramfs/           Profile build scripts (minimal, node, standard)
kernel/              Custom kernel build (Dockerfile, reproducible)
```

## How It Works

1. `dew up` reads your project directory — detects framework, package manager, and port
2. Selects the right profile (node for JS projects, standard for Docker, minimal for scripts)
3. Boots a Linux VM via Apple Virtualization.framework (~200ms for the hypervisor)
4. Custom init script loads kernel modules, configures networking, starts containerd (if standard)
5. Mounts your project directory via virtiofs (live bidirectional file sync)
6. Installs dependencies inside the VM (cached on persistent disk for next time)
7. Starts the dev server, forwards the port to localhost on your Mac
8. Polls the server until it responds with HTTP 200, then prints the URL

For `dew start`: boots the VM and opens a Unix socket at `~/.local/state/dew/default.sock`. Any process on the host can run `dew exec <cmd>` to execute commands in the running VM.

## License

MIT
