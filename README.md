# Dew

Ultra-lightweight VM for macOS. Sub-second boot. Hardware-level isolation.

Dew uses Apple Virtualization.framework to run Linux VMs in under a second. Designed for running untrusted code, AI agent sandboxes, and local development services.

## Install

```bash
# Download the latest release
curl -fsSL https://github.com/solcreek/dew/releases/latest/download/dew-darwin-$(uname -m) -o /usr/local/bin/dew
chmod +x /usr/local/bin/dew

# Sign with virtualization entitlement (required by macOS)
codesign --entitlements <(echo '<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>com.apple.security.virtualization</key><true/>
</dict></plist>') --force -s - /usr/local/bin/dew

# Download VM assets (kernel + initramfs)
dew assets pull
```

Or build from source:

```bash
git clone https://github.com/solcreek/dew.git && cd dew
make sign
bash initramfs/build.sh
```

## Quick Start

```bash
# Boot a VM and run a command
dew run -- uname -a

# Start a persistent VM with port forwarding
dew start --network --disk ./dev.img --forward 5432:5432

# From another terminal, exec into the running VM
dew exec "nerdctl run -d --net=host postgres:16"

# Connect from your app
psql -h localhost -p 5432
```

## When to Use Dew

- **AI agent sandbox** — run LLM-generated code with VM-level isolation, structured output, and network control
- **Local dev services** — postgres, redis, or any Docker image without Docker Desktop
- **CI/test isolation** — reproducible environments that match production, boot in under a second

## Features

**Fast.** 850ms cold boot (minimal profile). 50ms session exec. Boots a full Linux VM before most tools finish initializing.

**Small.** 4.4MB binary. 5MB minimal profile. No Homebrew, no Docker Desktop, no background daemon when stopped.

**Isolated.** Three network modes — fully isolated (no NIC), host-only (vsock), or NAT. The VM has no network device unless you ask for one.

**Structured.** `--json` for machine-readable output. `--events` for NDJSON streaming. `--stream` for real-time stdout/stderr. Built for agents, not just humans.

**Containers.** Standard profile includes containerd + nerdctl. `dew exec "nerdctl run postgres:16"` just works.

## Profiles

| Profile | What's inside | Size | Boot | Use case |
|---|---|---|---|---|
| **minimal** | Alpine + vsock agent | 5MB | ~850ms | Sandboxing, exec, scripts |
| **standard** | + containerd, nerdctl, runc, CNI | 129MB | ~2.5s | Docker images, services |

Standard is the default. Minimal is `--profile minimal`.

## Commands

```
dew start [flags]              Boot a VM (interactive, daemon socket)
dew run [flags] [--] <cmd>     Boot, exec, exit (ephemeral)
dew exec <cmd>                 Exec in a running VM (via daemon)
dew session create [flags]     Persistent session (~50ms per exec)
dew session exec <id> <cmd>    Exec in session
dew session destroy <id>       Destroy session
dew version                    Print version
```

## Flags

```
--profile <name>     Profile: standard (default) or minimal
--kernel <path>      Custom kernel path
--initrd <path>      Custom initramfs path
--cpus <n>           vCPUs (default: 1)
--memory <mb>        Memory in MB (default: 512)
--network            Enable NAT networking
--disk <path>        Persistent disk (ext4, created if absent)
--forward <h:g>      Forward host port to guest (e.g. 3000:3000)
--share <tag:path>   Share host dir into guest (read-only by default, :rw for write)
--stream             Real-time stdout/stderr
--events             NDJSON event stream (for agents)
--json               Structured JSON output
--vsock <port>       Custom vsock port
```

## Agent Integration

```bash
# Structured output
dew run --json -- "npm test"
# {"exit_code":0,"stdout":"...","stderr":""}

# NDJSON event stream
dew run --events -- "npm install"
# {"type":"stdout","data":"added 150 packages\n"}
# {"type":"exit","exit_code":0,"error":""}

# Session workflow (boot once, exec many)
ID=$(dew session create --json | jq -r .id)
dew session exec $ID "npm install"
dew session exec $ID "npm test"
dew session destroy $ID
```

## Network Isolation

```bash
# No network (default) — VM has no NIC at all
dew run -- "curl google.com"  # fails: no network device

# NAT networking — guest can reach the internet
dew run --network -- "curl google.com"  # works

# Host-only — vsock channel only, no IP stack
dew start --vsock 1024  # exec works, no internet
```

## Port Forwarding

Port forwarding works through vsock — no SSH tunnel, no NAT port mapping.

```bash
dew start --network --forward 5432:5432 --forward 6379:6379

# From any terminal on the host
psql -h localhost -p 5432
redis-cli -p 6379
```

## Persistent Data

```bash
# First run: creates and formats a 10GB ext4 disk
dew start --disk ./mydata.img

# Data in /data persists across VM restarts
dew exec "echo 'hello' > /data/greeting.txt"

# Next boot: data is still there
dew start --disk ./mydata.img
dew exec "cat /data/greeting.txt"  # hello
```

## Hot Reload (Minimal Profile)

Share your project directory into the VM with virtiofs. Changes on the host are immediately visible inside the VM.

```bash
dew start --profile minimal --share src:/app -- "cd /app && node --watch server.js"
```

## Security

- **VM isolation** — Apple Virtualization.framework hardware boundary, not container namespaces
- **Auth token** — per-VM token injected via vsock handshake (not kernel cmdline)
- **Unprivileged exec** — minimal profile runs commands as non-root user
- **Capability drop** — agent drops root capabilities after vsock bind
- **Cgroup limits** — configurable CPU/memory limits via kernel cmdline
- **Read-only shares** — `--share` defaults to read-only; explicit `:rw` required
- **Per-exec timeout** — commands killed after deadline (default 30s)

## Limitations

- **macOS only** — requires Apple Virtualization.framework (Apple Silicon or Intel Mac with macOS 13+)
- **No Windows or Linux host** — Linux doesn't need a VM (use containerd directly); Windows support is future work
- **Standard profile requires --disk** — containerd needs ext4 filesystem, not tmpfs
- **No docker-compose equivalent** — use multiple `dew exec "nerdctl run ..."` commands
- **Hot reload** — works with minimal profile + `--share`; standard profile requires container rebuild for code changes

## Building

```bash
# Build and sign (codesign required for Virtualization.framework)
make sign

# Build initramfs profiles
bash initramfs/build.sh minimal    # 5MB
bash initramfs/build.sh standard   # 129MB (default)

# Run tests
make test

# Cross-compile guest agent
make agent
```

Requires Go 1.23+ and macOS with Apple Virtualization.framework support.

## Architecture

```
cmd/dew/           Host CLI (macOS)
cmd/dew-agent/     Guest agent (Linux, static binary)
internal/vm/       Platform-agnostic VM interface
internal/vm/darwin/ Apple Virtualization.framework backend
internal/vsock/    Length-prefixed JSON exec protocol
internal/session/  Long-lived VM session management
internal/daemon/   Unix socket for cross-process exec
internal/serialexec/ Serial console fallback
initramfs/         Build scripts for VM images
kernel/            Turbo kernel build (Dockerfile)
```

## How It Works

1. `dew start` boots a Linux VM via Apple Virtualization.framework (~200ms)
2. Custom init loads kernel modules, configures networking, starts containerd
3. `dew-agent` inside the VM listens on vsock for exec requests
4. Host connects via vsock, sends JSON exec requests, receives structured responses
5. Port forwarding: host TCP listener → vsock → guest agent → guest TCP
6. Standard profile: `switch_root` from initramfs to ext4 disk for overlayfs support

## License

MIT
