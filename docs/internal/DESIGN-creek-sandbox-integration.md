# Creek Sandbox × Dew Integration Design

**Date:** 2026-05-27
**Status:** Design
**Author:** Lawrence Lin

## Summary

`creek sandbox` is a one-command local PaaS experience powered by Dew.
Dew remains an independent product; Creek is its highest-value consumer.

## Product Relationship

```
Dew (MIT, solcreek/dew)          Creek (Apache 2.0, solcreek/creek)
├── VM sandbox primitive          ├── PaaS framework
├── Own CLI, own brand            ├── Uses Dew as VM layer on macOS
├── Agent builders, security      ├── creek sandbox = Dew + creekd
└── dewvm.dev                     └── creek.dev
```

Analogy: Vagrant → Atlas, Terraform → Terraform Cloud. The
infrastructure tool has independent value; the platform product
makes it shine.

## User Flow

```
$ npx creek sandbox ./my-app

creek: downloading dew (3.5MB)...
creek: booting sandbox (850ms)
creek: starting creekd...
creek: deploying my-app...

  ✓ Dashboard    http://localhost:9080
  ✓ my-app       http://localhost:3000
  ✓ postgres     localhost:5432
  ✓ redis        localhost:6379

creek: sandbox ready
```

## Architecture

```
npx creek sandbox ./my-app
│
├── 1. Resolve Dew binary
│   ├── Check ~/.local/share/dew/dew
│   ├── If missing → download from GitHub Releases (3.5MB)
│   └── Codesign if needed
│
├── 2. Start Dew VM
│   dew start \
│     --profile standard \
│     --disk ~/.creek/sandbox.img \
│     --network \
│     --forward 9080:9080 \    # creekd admin API
│     --forward 3000:3000 \    # app port
│     --forward 5432:5432 \    # postgres
│     --forward 6379:6379 \    # redis
│     -- creekd-init
│
├── 3. creekd-init (inside VM)
│   ├── Start creekd supervisor daemon
│   ├── creekd reads creek.toml from shared dir or copies from host
│   ├── Provisions primitives (postgres, redis per creek.toml)
│   └── Deploys user app
│
├── 4. Dashboard accessible
│   └── host browser → localhost:9080 → vsock → VM:9080 → creekd
│
└── 5. Ongoing operations
    ├── creek deploy → calls creekd API via localhost:9080
    ├── creek logs → streams from creekd API
    ├── creek top → stats from creekd API
    └── creek stop → dew session destroy
```

## Integration Mode

### Phase 1: Subprocess (immediate)

Creek CLI shells out to `dew` binary. Simple, no API changes needed.

```typescript
// packages/cli/src/commands/sandbox.ts
import { execFileSync } from 'child_process';

const dewBin = await resolveDewBinary();
execFileSync(dewBin, [
  'start', '--profile', 'standard',
  '--disk', sandboxDiskPath,
  '--network',
  '--forward', '9080:9080',
  '--forward', `${appPort}:${appPort}`,
  '--', 'creekd-init',
]);
```

### Phase 2: Library import (future)

Move Dew's core packages from `internal/` to public API.
creekd (Go) imports Dew directly — no subprocess overhead.

```go
// creekd on macOS
sess, _ := session.Create(vm.Config{
    Network: true,
    DiskPath: "~/.creek/sandbox.img",
    Forwards: []vm.PortForward{{9080,9080},{3000,3000}},
})
sess.Exec("creekd serve --admin-port 9080", 0)
```

## Dew Requirements for Creek Sandbox

| Requirement | Status | Notes |
|---|---|---|
| VM boot < 1s | ✅ 850ms | Turbo kernel: 831ms |
| Port forwarding | ✅ vsock | Verified E2E |
| Persistent disk | ✅ --disk | ext4, auto-format |
| NAT networking | ✅ --network | DHCP works |
| containerd in VM | ⬚ standard profile | Need to build |
| Session daemon | ⬚ | Cross-process exec |
| Source code sync | ⬚ | virtiofs exists, need watcher |

## Dew Standard Profile Contents

The standard profile initramfs must include:

```
alpine minirootfs     3MB
dew-agent             2.4MB
containerd            30MB
nerdctl               8MB
cni-plugins           5MB
creekd binary         ~10MB (optional, creek-specific)
───────────────────────────
total                 ~50-60MB
```

## creekd Inside Dew VM

creekd already runs on Linux with SQLite + filesystem storage.
Inside Dew VM, it uses:

- `/data/` (persistent disk) for app data, SQLite DB, logs
- containerd for running user apps as containers
- Admin API on :9080, proxied to host via vsock

### creek.toml → Primitives

```toml
[app]
name = "my-app"
runtime = "node"

[database]
driver = "postgres"

[cache]
driver = "redis"
```

creekd reads this, provisions postgres + redis containers,
deploys the user app — same flow as production creekd on VPS.

## What Creek Sandbox Proves

1. **Dew works** — real workload, not just benchmarks
2. **creekd portability** — same code runs in cloud, VPS, and local Mac
3. **Agent DX** — `creek sandbox` is what an agent calls to test code
4. **Marketing** — 60-second demo video for Show HN

## Non-Goals

- Dew does NOT know about Creek (no Creek-specific code in Dew)
- Creek sandbox does NOT require creekd (power users can `dew start` alone)
- No GUI in Dew itself (Creek Dashboard provides the UI)

## Open Questions

1. How does host source code get into the VM? (virtiofs --share vs copy)
2. Should creekd binary be baked into standard profile or downloaded at runtime?
3. Multi-region simulation: multiple Dew VMs on one Mac?
