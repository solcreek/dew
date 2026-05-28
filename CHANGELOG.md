# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-05-28

### Added

- `dew up` — zero-config project detection and dev environment startup (Vite, Next.js, Astro, Nuxt, SvelteKit)
- `dew start` — persistent VM with daemon socket for cross-process exec
- `dew run` — ephemeral boot, exec, exit
- `dew exec` — execute commands in a running VM from any terminal
- `dew session create/exec/destroy` — in-process session management
- `dew assets pull/list/path` — VM image management with auto-download on first use
- Three profiles: minimal (5MB/850ms), node (31MB/3s), standard (129MB/5s)
- Apple Virtualization.framework backend with 850ms cold boot
- vsock exec protocol with `--json`, `--events` (NDJSON lifecycle), and `--stream` output
- Port forwarding via vsock proxy (`--forward`)
- Persistent disk with auto-format (`--disk`, auto per profile)
- virtiofs shared directories (`--share`, read-only by default)
- Hot reload: live file sync between macOS and VM
- Three network isolation modes: fully isolated (no NIC), host-only (vsock), NAT (`--network`)
- Security: vsock auth token handshake, capability drop, cgroup limits, per-exec timeout
- containerd + nerdctl in standard profile (switch_root to ext4)
- Node.js + npm + build-base in node profile (installed at first boot, cached on disk)
- Custom turbo kernel build (Dockerfile, 8-line diff from Alpine virt)
- npm package with postinstall codesign (`@solcreek/dew`)
- Homebrew formula (`brew tap solcreek/dew`)
- GitHub Actions CI (tests + cross-compile) and kernel build workflow
- Smoke test script for pre-release validation
- 73 unit tests across 8 packages

[0.1.0]: https://github.com/solcreek/dew/releases/tag/v0.1.0
