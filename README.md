# Dew

Ultra-lightweight VM for macOS. Sub-second boot. Hardware-level isolation. Agent-native.

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

No Node.js on your Mac. No Docker. No Homebrew. Everything runs inside a Linux VM that boots in under a second.

### Why Dew

- **850ms cold boot** — full Linux VM, faster than `docker run`
- **50ms exec** — warm VM, faster than SSH
- **Zero config** — `dew up` detects your project and starts everything
- **VM isolation** — hardware boundary, not container namespaces
- **Agent-native** — `--json` structured output, `--events` lifecycle stream, error fallback with suggestions. Built for LLM agents that generate and run code.
- **Three network modes** — fully isolated (no NIC), host-only (vsock), or NAT
- **Hot reload** — edit on macOS, Vite HMR fires inside the VM
- **4.4MB binary** — no Docker Desktop (700MB), no background daemon

## Install

```bash
# Homebrew
brew tap solcreek/dew && brew install dew

# npm
npm install -g @solcreek/dew

# Or download directly
curl -fsSL https://github.com/solcreek/dew/releases/latest/download/install.sh | sh
```

## Quick Start

```bash
# Auto-detect project and start dev environment
cd my-vite-app
dew up

# Or run a single command in an isolated VM
dew run -- uname -a

# Or start a persistent VM with services
dew start --network --forward 5432:5432
dew exec "nerdctl run -d --net=host postgres:16"
psql -h localhost -p 5432
```

## `dew up`

Zero-config dev environment. Detects your project, boots a VM, installs dependencies, starts the dev server, and tells you the URL.

```bash
cd my-project
dew up
```

Auto-detects:

| Framework | Config file | Port | Package manager |
|---|---|---|---|
| Vite | `vite.config.*` | 5173 | npm / yarn / pnpm / bun |
| Next.js | `next.config.*` | 3000 | (auto from lock file) |
| Astro | `astro.config.*` | 4321 | |
| Nuxt | `nuxt.config.*` | 3000 | |
| SvelteKit | `svelte.config.*` | 5173 | |
| Node.js | `package.json` | 3000 | |

Hot reload: your project directory is mounted via virtiofs. Edit files on macOS, Vite HMR updates the browser instantly. `node_modules` stays inside the VM — your Mac stays clean.

## When to Use Dew

- **AI agent sandbox** — run LLM-generated code with VM isolation, structured output, and network control
- **Local dev services** — postgres, redis, or any Docker image without Docker Desktop
- **Zero-setup onboarding** — `dew up` in any supported project, no dev tools required
- **CI/test isolation** — reproducible environments that boot in under a second

## Profiles

| Profile | What's inside | Size | Default RAM | Default disk |
|---|---|---|---|---|
| **minimal** | Alpine + exec agent | 5MB | 512MB | none (tmpfs) |
| **node** | + Node.js, npm, build-base | 31MB | 1GB | 4GB auto |
| **standard** | + containerd, nerdctl, runc, CNI | 129MB | 2GB | 10GB auto |

`dew up` selects the right profile automatically. Node projects get `node`, Docker workflows get `standard`, sandboxing gets `minimal`.

## Commands

```
dew up [dir]                   Auto-detect and start dev environment
dew start [flags]              Boot a VM (persistent, daemon socket)
dew run [flags] [--] <cmd>     Boot, exec, exit (ephemeral)
dew exec <cmd>                 Exec in a running VM (from any terminal)
dew session create [flags]     In-process session (~50ms per exec)
dew session exec <id> <cmd>    Exec in session
dew session destroy <id>       Destroy session
dew version                    Print version
```

## Flags

```
--profile <name>     Profile: minimal, node, or standard (default)
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

When a step fails, agents get structured errors with actionable suggestions:

```json
{"type":"install","status":"failed","error":"peer dep conflict","suggestion":"try --legacy-peer-deps"}
```

### Exec API

```bash
dew run --json -- "npm test"
# {"exit_code":0,"stdout":"5 passing\n","stderr":""}

# Boot once, exec many (50ms per exec)
dew start --network --forward 3000:3000
dew exec "npm install"
dew exec "npm test"
dew exec "npm run build"
```

## Network Isolation

Three modes — choose per VM:

```bash
# Fully isolated (default) — no network device at all
dew run -- "curl google.com"         # fails: no NIC

# NAT — guest can reach the internet
dew run --network -- "curl google.com"  # works

# Host-only — vsock channel, no IP stack
dew start --vsock 1024               # exec works, no internet
```

## Port Forwarding

Through vsock — no SSH tunnel, no NAT port mapping.

```bash
dew start --network --forward 5432:5432 --forward 6379:6379

# From any terminal, any app
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

| | Cold boot → exec | Binary | Total footprint |
|---|---|---|---|
| **Dew minimal** | **850ms** | 4.4MB | 5MB |
| **Dew node** | **~3s** | 4.4MB | 31MB + 4GB disk |
| **Dew standard** | **~5s** (first boot) | 4.4MB | 129MB + 10GB disk |
| Docker Desktop | 30s+ | — | 700MB + GB disk |
| Lima | 33s | 33MB | 33MB + 8.7GB disk |

Session exec (warm VM): **50ms**.

## Security

- **VM isolation** — Apple Virtualization.framework hardware boundary
- **Auth token** — per-VM token injected via vsock handshake (never on disk or cmdline)
- **Capability drop** — agent drops root capabilities after vsock bind
- **Cgroup limits** — CPU/memory limits per VM
- **Read-only shares** — `--share` defaults to read-only; explicit `:rw` required
- **Per-exec timeout** — commands killed after deadline (default 30s)
- **Network off by default** — VM has no NIC unless `--network` is passed

## Limitations

- **macOS only** — requires Apple Virtualization.framework (macOS 13+)
- **No Windows or Linux host** — Linux doesn't need a VM; Windows is future work
- **Hot reload** — works with `dew up` and minimal profile; standard profile containers need rebuild for code changes
- **No docker-compose** — use multiple `dew exec "nerdctl run ..."` or `dew up` for single-app projects

## Architecture

```
cmd/dew/             Host CLI (macOS)
cmd/dew-agent/       Guest agent (Linux, static binary)
internal/vm/         Platform-agnostic VM interface
internal/vm/darwin/  Apple Virtualization.framework backend
internal/vsock/      Length-prefixed JSON exec protocol
internal/detect/     Project auto-detection (registry-based)
internal/session/    Long-lived VM session management
internal/daemon/     Unix socket for cross-process exec
initramfs/           Profile build scripts
kernel/              Custom kernel build (Dockerfile)
```

## How It Works

1. `dew up` detects framework, package manager, and port from your project directory
2. Boots a Linux VM via Apple Virtualization.framework (~200ms)
3. Mounts your project via virtiofs (live bidirectional sync)
4. Installs dependencies inside the VM (cached on persistent disk)
5. Starts the dev server, forwards the port to your Mac
6. Health-checks the server, prints the URL when ready

For `dew start`: same VM boot, plus a daemon socket so `dew exec` works from any terminal.

## License

MIT
