# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
make sign          # build + codesign with virtualization entitlement (required)
make agent         # cross-compile guest agent for linux/amd64 + linux/arm64
make test          # go test ./...
go test ./internal/detect/ -run TestDetect_Vite -v   # single test

# Initramfs profiles (run from repo root). Sizes are approximate.
bash initramfs/build.sh minimal    # ~5MB, exec-only
bash initramfs/build.sh node       # ~31MB, Node.js + npm + build-base
bash initramfs/build.sh python     # python3 + pip
bash initramfs/build.sh standard   # node tier + larger RAM/disk; OCI via crun (no daemon)

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
- **`internal/services`** — predefined service configs for `--with` flag in `Registry` (postgres, redis, mysql, mongo, minio). Each entry is an OCI `Image` run on demand via crun/`dew-oci-run` (no nerdctl). Helpers: `Lookup`, `Names`, `ListenProbeCmd` (readiness via `/proc/net/tcp[6]`, no `ss`), `ConnString`.
- **`internal/daemon`** — Unix socket at `~/.local/state/dew/<name>.sock` (default VM → `default.sock`) for cross-process exec. The VM-owning process (`dew vm start` / `dew up` / `dew run`) opens it; `dew exec` connects to it.
- **`internal/session`** — in-process VM session (`Create/Exec/Destroy`). Token handshake via vsock ping.
- **`internal/serialexec`** — fallback exec via serial console (sentinel-based framing). Used when vsock is unavailable.

### Build tags

- `//go:build darwin` — host CLI and darwin VM backend
- `//go:build linux` — guest agent only
- Guest agent: `CGO_ENABLED=0` static binary (runs on Alpine musl)

### Init system (two-stage)

`initramfs/build.sh` generates both `/init` and `/init-stage2`:

1. **`/init`** — early boot: mount filesystems, modprobe, detect disk. If disk present → `mkfs.ext4` (first boot) → `switch_root /mnt/root /init-stage2`. If no disk → exec `/init-stage2` directly.
2. **`/init-stage2`** — networking (DHCP + DNS fallback), virtiofs mounts (from kernel cmdline `dew.share=tag:/path`), cgroup limits, Node.js install (node profile, first boot only), dew-agent start. There is no in-guest container daemon: OCI images run on demand via crun (the host-pull + overlay `dew-oci-run` path), not containerd/nerdctl.

Standard/node profiles use `switch_root` to ext4 disk because crun's overlay rootfs needs a real filesystem for its upperdir (tmpfs is rejected as an overlay upperdir).

### Kernel cmdline contract (`dew.*` params)

The kernel cmdline is the one-way host→guest config channel read by the init
scripts (host writes via `vm.Config.CmdLine`; guest parses in `init` /
`init-stage2`). Auth tokens never go here (see vsock `SetTokenRequest`). Current
params:

| Param | Set by (host) | Read by (guest) | Meaning |
|---|---|---|---|
| `dew.share=tag:/path[:ro]` | `cmdRun`/`cmdUp` per shared dir | init-stage2 | virtiofs mount (`dew up` adds `project:/app`) |
| `dew.disk=1` | host when a data disk is attached | init | wait for `/dev/vda` (diskless skips the ~1s probe) |
| `dew.rosetta=1` | `--rosetta` (arm64) | init-stage2 | mount rosetta share + register binfmt_misc |
| `dew.cpu_quota=` / `dew.mem_limit=` / `dew.pids_max=` | `--cgroup` (and `--confine`) | init-stage2 | cgroup `cpu.max` / `memory.max` / `pids.max` on `/sys/fs/cgroup/dew` |
| `dew.cmd=` | `cmdStart` only | init-stage2 | base64 boot-time command (`dew run` delivers over vsock instead) |

When adding a param: keep the `dew.` prefix, match it as a whole cmdline token,
and add/extend the build-script drift-guard test (see Conventions).

### Profiles and defaults

Defaults are filled by `applyProfileDefaults` (`cmd/dew/main.go`) and only when
the caller left the global defaults (1 CPU / 512MB / no disk); explicit
`--cpus` / `--memory` / `--disk` win.

| Profile | CPUs | RAM | Disk |
|---|---|---|---|
| minimal | 1 | 512MB | none (diskless, `dew run`'s default) |
| node | 4 | 2GB | 4GB auto at `~/.local/share/dew/node.img` |
| python | 4 | 2GB | 4GB auto at `~/.local/share/dew/python.img` |
| standard | 4 | 2GB | 10GB auto at `~/.local/share/dew/standard.img` |

`dew up` auto-selects profile via `detect.Detect()`. Assets auto-download from GitHub Releases on first use.

### Kernel strategy

**Shipped default (both arches): Alpine `linux-virt` 6.12.95-0-virt.**
`initramfs/build.sh` downloads the `linux-virt` APK (version pinned per-arch at
the top of the script), extracts the kernel, and releases it as the
`vmlinuz-<arch>` asset (`kernelAssetName()` in `cmd/dew/assets_path.go`). This
kernel is **modular** (CONFIG_MODULES on) — `build.sh` ships only the modules in
its allowlist plus their transitive closure (resolved from `modules.dep`), and
the two-stage init `modprobe`s them at boot.

Because it's modular, two rules matter:

- Never pair the Alpine virt kernel with non-Alpine modules — CONFIG_MODVERSIONS
  causes struct mismatch.
- Alpine kernel modules must be decompressed (`gunzip *.ko.gz`) for `insmod`.

**Optional turbo kernel:** `kernel/build.sh` (Docker) builds a monolithic
(CONFIG_MODULES off) `vmlinuz-turbo`. It's faster to boot but supports the
`minimal` profile only — ext4/overlay are modules there, not built-in — so
disk-bearing profiles can't use it.

Apple VZ format: ARM64 = uncompressed Image (4K pages). x86_64 = bzImage. Doctor
verifies the ARM64 kernel is a "raw ARM64 Linux Image".

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
modprobes `virtio-rng` (the allowlisted dash name) then falls back to
`virtio_rng` — kmod normalises the two, but leading with the dash name also
keeps the modprobe/allowlist drift guard (TestInitramfsBuildScript_Modprobe-
MatchesAllowlist) green. Without the RNG the guest starves /dev/random and
getrandom() callers block at "crypto/rand: blocked".

## Conventions

- Commit messages: English, objective facts, no competitor brand names, no Co-Authored-By lines. Prefer atomic commits (one logical change each).
- Shared dirs default to read-only (security); explicit `:rw` for write
- Never print unmasked env values or tokens
- Node profile first boot runs `apk upgrade musl` before `apk add nodejs` to prevent musl version mismatch segfault on second boot
- `initramfs/build.sh` is guarded by Go tests in `cmd/dew` (e.g. `TestInitramfsBuildScript_ModprobeMatchesAllowlist`, `TestInitramfsBuildScript_DiskWaitGatedOnDewDisk`). Editing the script's modprobe/allowlist or cmdline-parsing lines means updating these guards in the same change — run `make test` before committing.
