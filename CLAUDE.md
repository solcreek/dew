# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
make sign          # build + codesign with virtualization entitlement (required)
make agent         # cross-compile guest agent for linux/amd64 + linux/arm64
make test          # go test ./...
go test ./internal/detect/ -run TestDetect_Vite -v   # single test

# Initramfs profiles (run from repo root)
bash initramfs/build.sh minimal    # 5MB, exec-only
bash initramfs/build.sh node       # 31MB, Node.js + npm + build-base
bash initramfs/build.sh standard   # 129MB, + containerd/nerdctl/runc/CNI

# Turbo kernel (Docker required)
bash kernel/build.sh

# Smoke tests (requires built binary + initramfs)
./smoke-test.sh
```

## Architecture

Dew boots Linux VMs on macOS via Apple Virtualization.framework. Two binaries: `dew` (host CLI, macOS) and `dew-agent` (guest daemon, Linux static binary inside VM).

### Host ↔ Guest communication

```
Host (macOS)                           Guest (Alpine Linux VM)
─────────────────────────────────────────────────────────────
dew CLI                                dew-agent
  │                                      │
  ├── VsockConnect(port) ──vsock──→ vsock.Listen(port)
  │                                      │
  ├── WriteJSON(ExecRequest) ─────→ ReadJSON → exec.Command
  │                                      │
  ├── ReadJSON(ExecResponse) ←──── WriteJSON(result)
  │                                      │
  └── ConnectRequest ─────────────→ net.Dial(tcp) → bidirectional proxy
      (port forwarding)
```

All communication uses length-prefixed JSON over vsock (no SSH). Auth token injected via `SetTokenRequest` after boot (never on kernel cmdline or disk).

### Key packages

- **`internal/vm`** — platform-agnostic `VM` interface (`Start/Stop/State/WaitForState/VsockConnect`). `Config` holds kernel, initrd, cpus, memory, network, shared dirs, port forwards, console pipes.
- **`internal/vm/darwin`** — Apple Virtualization.framework implementation. Configures boot loader, serial console, NAT, vsock, virtiofs, block devices.
- **`internal/vsock`** — protocol types (`ExecRequest`, `ExecResponse`, `ConnectRequest`, `SetTokenRequest`, `OutputChunk`, `ExecDone`) and `WriteJSON`/`ReadJSON` with 4-byte big-endian length prefix.
- **`internal/detect`** — registry-based project detection. `Detector` interface with `Match(dir)` and `Detect(dir)`. Registers `nodeDetector` and `pythonDetector`; extensible for Go/Rust.
- **`internal/services`** — predefined service configs for `--with` flag (postgres, redis, mysql, mongo, minio). `NerdctlRunCmd()` generates container run commands.
- **`internal/daemon`** — Unix socket at `~/.local/state/dew/default.sock` for cross-process exec. `dew start` opens it; `dew exec` connects to it.
- **`internal/session`** — in-process VM session (`Create/Exec/Destroy`). Token handshake via vsock ping.
- **`internal/serialexec`** — fallback exec via serial console (sentinel-based framing). Used when vsock is unavailable.

### Build tags

- `//go:build darwin` — host CLI and darwin VM backend
- `//go:build linux` — guest agent only
- Guest agent: `CGO_ENABLED=0` static binary (runs on Alpine musl)

### Init system (two-stage)

`initramfs/build.sh` generates both `/init` and `/init-stage2`:

1. **`/init`** — early boot: mount filesystems, modprobe, detect disk. If disk present → `mkfs.ext4` (first boot) → `switch_root /mnt/root /init-stage2`. If no disk → exec `/init-stage2` directly.
2. **`/init-stage2`** — networking (DHCP + DNS fallback), virtiofs mounts (from kernel cmdline `dew.share=tag:/path`), cgroup limits, Node.js install (node profile, first boot only), containerd start (standard profile), dew-agent start.

Standard/node profiles use `switch_root` to ext4 disk because containerd's overlayfs needs a real filesystem (not tmpfs).

### Profiles and defaults

| Profile | `resolveAssets` defaults |
|---|---|
| minimal | 512MB RAM, no disk |
| node | 1GB RAM, 4GB auto disk at `~/.local/share/dew/node.img` |
| python | 1GB RAM, 4GB auto disk at `~/.local/share/dew/python.img` |
| standard | 2GB RAM, 10GB auto disk at `~/.local/share/dew/standard.img` |

`dew up` auto-selects profile via `detect.Detect()`. Assets auto-download from GitHub Releases on first use.

### Kernel strategy

ARM64 (Apple Silicon): Kata pre-built kernel (vmlinux-6.18.15-186, 15.4MB, 30ms boot, monolithic). Download from kata-static release.

x86_64 (Intel Mac): Debian cloud kernel (11MB + 22MB modules) for now. Future: custom monolithic from `kernel/config-dew-x86_64.fragment`.

Never use Alpine virt kernel with non-Alpine modules — CONFIG_MODVERSIONS causes struct mismatch. Monolithic kernels (CONFIG_MODULES off) avoid this entirely.

Apple VZ format: ARM64 = uncompressed Image (4K pages). x86_64 = bzImage.

See `product-planning/dew-kernel-strategy-2026-05.md` for full benchmark data.

### x86_64 emulation (Rosetta)

On Apple Silicon, `--rosetta` lets the guest run amd64 binaries/containers.
Host side (`internal/vm/darwin`, arm64 only): `Config.EnableRosetta` attaches a
`LinuxRosettaDirectoryShare` (virtiofs tag `rosetta`) and the CLI adds
`dew.rosetta=1` to the kernel cmdline. Guest side (`init-stage2`): mounts the
`rosetta` share and registers the x86_64 ELF magic with `binfmt_misc` using the
`F` (fix-binary) flag so translation works inside container namespaces.

Two kernel modules are required and easy to miss in the initramfs allowlist:
`binfmt_misc` (CONFIG_BINFMT_MISC=m, not built-in) and the VirtIO RNG. Mind the
dash/underscore split: the **allowlist must use the on-disk filename**
`virtio-rng` (dash) because the prune step matches module basenames from
`modules.dep` (a `virtio_rng` entry would be pruned out and the entropy fix
silently lost; it also pulls `rng-core` transitively). The **boot-time loader**
modprobes `virtio_rng` then falls back to `virtio-rng` — kmod normalises the
two, but the fallback covers modprobe implementations that don't. Without the
RNG the guest starves /dev/random and getrandom() callers block at
"crypto/rand: blocked".

## Conventions

- Commit messages: English, objective facts, no competitor brand names, no Co-Authored-By lines
- Shared dirs default to read-only (security); explicit `:rw` for write
- Never print unmasked env values or tokens
- Turbo kernel only supports minimal profile (ext4/overlay are modules, not built-in)
- Alpine kernel modules must be decompressed (`gunzip *.ko.gz`) for `insmod` compatibility
- Node profile first boot runs `apk upgrade musl` before `apk add nodejs` to prevent musl version mismatch segfault on second boot
