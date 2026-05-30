# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.4] - 2026-05-30

### Fixed

- **`dew app run` no longer times out on cold first run.** The
  ensureDewVM polling timeout was 60s, which is roughly the time
  needed JUST to download the 146MB VM assets (vmlinuz + standard
  initramfs) from GH Release on first use — before the VM even starts
  booting. Cold-start total is typically 60-120s (download + first-boot
  disk init), which exceeded the timeout and surfaced as
  `VM did not start within 60s` even though the VM was on its way up.
  Bumped to 300s ceiling — covers cold + slow networks; hot-start
  (sock already exists) returns sub-5s as before.

### Changed

- **Cold-start now shows what's happening.** ensureDewVM streams
  heuristic progress messages to the spinner during the wait:
  *Downloading VM assets (~146MB, first run only)* at +15s,
  *Booting VM* at +60s, *First-time disk init (formatting + populating
  rootfs)* at +120s, *Still working — slow network or first-boot setup*
  at +200s. Subsequent runs (sock immediate) skip these entirely.

## [0.7.3] - 2026-05-30

### Fixed

- **No more silent fallback to a wrong-profile initramfs.** When
  `--profile X` was specified and `initramfs-X.cpio.gz` was missing
  from the asset dir, `resolveAssets` quietly substituted the
  unprefixed `initramfs.cpio.gz` (typically the minimal profile,
  bundled by older versions or left over from a different run). The
  VM then kernel-panicked early in boot: `/init: mkfs.ext4: not
  found` (because minimal has no e2fsprogs), the disk init failed,
  `switch_root` exited, init died. The user saw a generic "VM boot
  failed: exited early" with no hint at the asset mismatch.

  Now the profile-specific path is pinned. If the file is missing,
  the existing auto-download block fetches `initramfs-X-<arch>.cpio.gz`
  from the GH Release matching this binary's version, or fails with
  a clear error.

  Hits anyone who's manually deleted `~/.local/share/dew/initramfs-standard.cpio.gz`
  or installed dew via an older bundler that only shipped the minimal
  asset. Fresh installs were already fine (both files missing → auto-download).

## [0.7.2] - 2026-05-30

### Fixed

- **`dew app run` no longer fails when an unrelated host process holds
  one of the speculative pre-forward ports.** `ensureDewVM` was eagerly
  forwarding 3000/3001/3002/3003/3004/3005/8000/8080/7456/5230/2368
  on every VM start "in case the user runs another app later" — but if
  *any* of those was already taken on the host (a bun/vite dev server
  on 3000, for instance), `dew start --forward N:N` failed strict and
  the entire VM boot exited with a generic "exited early" error that
  pointed at entitlements / macOS version / port conflicts among other
  unrelated VMs. None of those was the actual cause.

  The pre-forward loop now calls a new `hostPortInUse(port)` helper
  (TCP listen probe on 127.0.0.1) and skips taken ports best-effort.
  The user's explicit `--port hostPort` is still added unconditionally
  — if that one is taken, the error surfaces immediately.

## [0.7.1] - 2026-05-30

### Fixed

- **Homebrew tap formula is actually published** on release. v0.7.0
  goreleaser's `brews:` block silently skipped the push (root cause not
  pinned — same App+key+install creates refs fine via direct API call,
  but goreleaser's REST POST `/git/refs` returned 403). Replaced with a
  custom workflow step that templates `Formula/dew.rb` directly from
  the goreleaser-produced `checksums.txt`, then pushes + opens PR using
  the App-minted token via `Authorization: Bearer` header — token never
  embedded in URLs.

### Changed

- `.goreleaser.yaml` drops the `brews:` block. Brew formula generation
  + push is handled entirely by the new "Bump brew formula on tap" step
  in `.github/workflows/release.yml`. Simpler debug surface, no
  goreleaser-internal env-var inheritance to wrestle with.

## [0.7.0] - 2026-05-30

**Headline:** distribution architecture overhaul. One Homebrew tap, one
thin npm dispatcher, one signed checksum file, one tag triggers everything.

### Added

- **`brew install solcreek/tap/dew`** — formula PR'd automatically to
  `solcreek/homebrew-tap` on every tag, by goreleaser
- **`curl -fsSL https://dewvm.dev/install.sh | sh`** — POSIX installer
  that detects OS/arch, fetches the release tarball, verifies SHA256, and
  optionally verifies a cosign keyless signature against the
  `release.yml` signer identity
- **cosign keyless signature on `checksums.txt`** — install.sh + npm
  dispatcher both verify the chain against the GitHub Actions OIDC issuer
- **SLSA Level 3 build provenance** for every release artifact, via
  `slsa-framework/slsa-github-generator`
- **`.github/workflows/release-dry-run.yml`** — runs on PRs touching
  release shape; checks goreleaser config + linux snapshot build,
  npm dispatcher tests, shellcheck on install.sh, and the install.sh
  sync mechanism (see below)
- **install.sh sync mechanism** — `website/scripts/sync-install-sh.mjs`
  runs as Astro's `prebuild` step on every `pnpm run build`, copying
  `/install.sh` → `website/public/install.sh` (gitignored). Single
  source of truth at the repo root; deploys can never serve a stale
  copy. `release-dry-run.yml` exercises the script + diffs the built
  artifact against the source to catch regressions.
- **`docs/RELEASING.md`** — one-time setup for the tap repo, brew-tap
  token, npm trusted publisher, and signed-tag protection

### Changed

- **npm packaging — collapsed to a single thin dispatcher** (`@solcreek/dew`).
  Drops the five `@solcreek/dew-<triple>` per-platform packages added in
  v0.6.0. The dispatcher downloads the matching binary from this release's
  GitHub Release tarball on first run, verifies SHA256 + cosign, caches
  in `DEW_CACHE_DIR` (defaults under `~/.local/share/dew/bin/<version>/`).
  Removes the OIDC pending-publisher coordination needed for 6 packages
  and unifies bytes-on-disk across npm / brew / install.sh / direct
  download.
- **Release pipeline is goreleaser-driven.** `goreleaser` archives
  prebuilt + notarized darwin binaries, cross-compiles linux, produces
  `checksums.txt`, signs with cosign, opens the brew formula PR, and
  creates the GitHub Release. Auxiliary assets (initramfs / kernel /
  dew-agent / dew-windows / dew-rootfs) are uploaded alongside.
- **Release jobs are atomic-per-channel** with grep-specific idempotency
  checks. Replaces the `|| echo "Skipped"` mask that silently hid
  `ENEEDAUTH` in v0.6.0.

### Removed

- `@solcreek/dew-darwin-arm64`, `-darwin-x64`, `-linux-x64`,
  `-linux-arm64`, `-win32-x64` per-platform packages — never published
  (v0.6.0 was unpublished within the 72h window). No migration needed.
- `npm/scripts/generate-packages.mjs` + `npm/scripts/sync-version.mjs`
  — only needed for the multi-package world

### Fixed

- `npm install @solcreek/dew@0.6.0` failed with `platform package is
  missing` because OIDC trusted-publisher pending entries weren't
  configured for the 5 per-platform packages. The v0.7.0 single-package
  model eliminates the failure mode entirely.

## [0.6.0] - 2026-05-30 — **WITHDRAWN**

> **Note:** v0.6.0 was unpublished from npm on 2026-05-30 within the
> 72h window. The five per-platform packages
> (`@solcreek/dew-darwin-arm64`, etc.) were never created on npm
> because OIDC trusted-publisher pending entries weren't configured,
> causing `npm install @solcreek/dew@0.6.0` to fail with
> `platform package is missing`. v0.7.0 removes the per-platform
> packaging entirely. GitHub Release v0.6.0 with binaries remains
> available; install via `curl ... install.sh | sh` or direct download
> still works.

**Headline:** `dew app run` actually starts containers; npm install never
touches binary bytes.

### Added

- **`dew server start/stop/restart/status`** — power management for
  provisioned servers, mirroring `dew server create/destroy`
- **`--json` output for all `dew server` subcommands** — structured
  responses for agent-driven workflows
- `npm/scripts/generate-packages.mjs` — scaffolds the 5 per-platform
  publish directories from CI-built signed + notarized binaries
- `npm/scripts/sync-version.mjs` — locks main + every `optionalDependency`
  to one version on tag push
- `npm/test/` — 14 tests covering the dispatcher (`DEW_BINARY` override,
  missing platform package, exit code propagation), package generator
  (all-present, partial, zero, byte-exact binary copy), and version sync

### Changed

- **npm packaging — per-platform via `optionalDependencies`.** The main
  `@solcreek/dew` package becomes a small Node dispatcher (no postinstall).
  Each platform binary ships in its own package
  (`@solcreek/dew-darwin-arm64`, `@solcreek/dew-darwin-x64`,
  `@solcreek/dew-linux-x64`, `@solcreek/dew-linux-arm64`,
  `@solcreek/dew-win32-x64`) listed under `optionalDependencies`; npm
  installs only the one matching `process.platform` / `process.arch`.

  This removes the install-time code path that touched the binary, which
  caused two incidents in earlier releases (codesign failure silently
  breaking VM support; v0.5.0 re-signing stripping the Developer ID).
  The dispatcher does `require.resolve` + `spawnSync`; the bytes inside
  the Mach-O — including the notarization staple — arrive byte-for-byte
  from the platform package tarball.

- **`DEW_BINARY=/path/to/dew`** environment variable for local builds and
  testing, bypassing platform-package resolution.
- **`install.sh` no longer re-signs binaries with Developer ID signatures.**
  Detects the existing signature before ad-hoc-signing, matching the
  v0.5.1 fix in the npm path.
- **`dew server` internals on capstan v0.5** — power actions, factory
  constructor, refreshed provider catalogs

### Fixed

- **`dew app run` failed on first container start with
  `failed to create bridge "nerdctl0": operation not supported`.**
  Two compounding causes:
  - The standard-profile init script never loaded `bridge`,
    `br_netfilter`, `veth`, `iptable_nat`, `nf_nat`, or `xt_MASQUERADE`
    before starting containerd. CNI's bridge plugin needs all of them.
  - `/lib/modules` on the persistent disk was only populated on **first
    boot**. When the initramfs shipped an updated kernel, the cached
    modules went stale and every `modprobe` silently failed (calls used
    `|| true`), leaving only downstream OCI/CNI errors with no
    kernel-version hint.

  Init-stage2 now `modprobe`s the CNI module set before launching
  containerd, and `/lib/modules` is rsync'd from the initramfs on every
  boot. Two Go tests guard both invariants
  (`cmd/dew/initramfs_modules_test.go`); `smoke-test.sh` adds an
  `ip link add … type bridge` integration check.

### Removed

- `npm/scripts/postinstall.js` — replaced by the new platform-package
  resolution model; nothing on the user's machine touches the binary
- `.github/workflows/npm-publish.yml` — folded into the Release workflow
  as a single source of truth for publishing all 6 packages together
- `cmd/dew-serve/` — orphan binary entrypoint that duplicated `cmd/dew`
  after the v0.4 unification; systemd unit renamed to `dew.service`

## [0.5.0] - 2026-05-30

**Headline:** the VM actually works.

Previous releases shipped ad-hoc-signed binaries. macOS refuses to honor
the `com.apple.security.virtualization` restricted entitlement on
ad-hoc signatures, so `dew app run`, `dew up`, `dew run`, and `dew exec`
all failed with `VZErrorDomain Code=1` on a fresh install. The host
Docker fallback masked this for `dew app run`, but the rest of the CLI
was effectively non-functional. This release fixes the root cause.

### Added

- **Proper Developer ID Application signing** for `dew-darwin-arm64`
  and `dew-darwin-amd64` release binaries, with hardened runtime and
  the virtualization entitlement embedded
- **Apple notarization** for both architectures via `notarytool`
- **`scripts/setup-signing-keychain.sh`** — CI helper that creates a
  temp keychain, imports the Developer ID `.p12`, adds the G1/G2
  intermediate CAs, and grants codesign access without UI prompts
- **`scripts/codesign.sh`** — signs binaries with Developer ID +
  hardened runtime + entitlements; falls back to ad-hoc when run
  without `APPLE_DEVELOPER_ID` (so fork PRs still build)
- **`scripts/notarize.sh`** — submits via notarytool with a
  keychain-stored credential profile, so the app-specific password
  never appears on the process command line

### Changed

- Release workflow now runs sign + notarize as discrete steps and
  cleans up the keychain in an `if: always()` step

### Required org secrets (set under `solcreek`, visibility = selected)

- `APPLE_DEVELOPER_ID`
- `APPLE_CERT_P12_BASE64`
- `APPLE_CERT_P12_PASSWORD`
- `APPLE_ID`
- `APPLE_TEAM_ID`
- `APPLE_APP_PASSWORD`

## [0.4.7] - 2026-05-30

### Added

- **`dew doctor --json`** — structured output with stable error codes
  (`ad_hoc_entitlement`, `boot_failed`, `missing_asset`, …) so agents
  can gate on environment checks before attempting VM operations
- **`dew app run --events`** — NDJSON lifecycle stream (`preparing`,
  `vm_failed`, `fallback`, `started`, `health`, `done`) so callers see
  *which* backend actually ran (VM vs host docker fallback) and *why*
- **`dew app run --no-fallback`** — strict mode that fails closed when
  the VM cannot start, instead of silently switching to host docker
- **`backend` field** in `dew app run --json` summary output
- npm postinstall prints invocation hint (npx / local / global) so
  users who installed via `npx @solcreek/dew` see the correct command
  to use, not just bare `dew` which only exists with `npm i -g`

### Fixed

- `ensureDewVM` no longer leaks a raw `{"ok":false,...}` line into
  the human progress stream when the VM fails. Child stdout/stderr
  is captured; failure is surfaced via the new structured events
- Boot test is now skipped when ad-hoc-signed entitlement is detected
  (it would always fail with the same root cause)

### Notes

- 9 new tests (`doctor_test.go`, `events_test.go`); 114 total

## [0.4.4] - 2026-05-29

### Added

- **`dew update`** — self-update with semver comparison + SHA-256 checksum verification. Background check every 24h
- **`dew app run/stop/list`** — run OSS apps from registry, symmetric verbs
- **Agent safety** — input validation (path traversal, query injection, control chars), `--dry-run`, structured JSON errors (`{"ok":false,"error":"...","code":"..."}`)
- **Windows installer** — `install.ps1` (`irm ... | iex`)
- **install.sh cross-platform** — supports macOS + Linux
- **CI kernel boot verification** — real Apple VZ boot test on macos-15-intel (x86_64) and macos-latest (ARM64)
- **116 tests** across 13 packages

### Fixed

- Version injected from git tag via ldflags (single source of truth, no manual updates)
- npm version auto-synced from GitHub release tag
- TMPDIR on ext4 for overlay mount (dew-virt kernel)
- Update check only on user-facing commands (no duplicate notices via dew exec)
- Health check reports `✗ timed out` instead of false `✓`
- Port forwarding: pre-forward common ports for multi-app + correct host:container mapping
- npm publish waits for Release workflow (no race condition)

### Changed

- Linux binary `dew-serve` → `dew` (unified CLI name on all platforms)
- `dew up` is dev-only; registry apps moved to `dew app run`
- CLI help restructured: Dev → Share → Apps → Deploy → Infrastructure → Advanced
- Tagline: "run any app, anywhere"
- README rewritten for v0.4

## [0.4.0] - 2026-05-29

### Added

- **CLI restructure** — commands organized into Dev, Share, Apps, Deploy, Infrastructure, Advanced sections
- **`dew app run/stop/list`** — run open-source apps from dew-apps registry. Separate from `dew up` (dev)
- **`dew apps`** — browse available apps (11 apps: Excalidraw, Uptime Kuma, Vaultwarden, Ghost, etc.)
- **`dew build`** — package app for deployment with manifest.json. Static site detection. Skips node_modules, lock files, .claude/, .agents/, *.db
- **`dew deploy`** — upload tarball or `--image` to remote `dew serve`. SSE progress streaming
- **`dew serve`** — production deploy receiver with self-signed TLS, process management, static file server, rollback endpoint
- **`dew share`** — temporary public HTTPS URL via Cloudflare Quick Tunnel. Readiness verification before printing URL
- **`dew server create/list/destroy`** — provision VPS via capstan (Hetzner, DigitalOcean, Linode, Vultr). Zero-SSH cloud-init setup
- **`dew auth set/list/remove`** — credential management (JSON storage)
- **`dew env set/list/remove`** — remote environment variable management
- **`dew rollback`** — restore previous deploy version
- **`dew-serve` standalone binary** — cross-compiled for Linux (7.1MB), no Apple VZ dependency
- **`dew install` runs containers inside Dew VM** — no host Docker dependency. Falls back to host Docker if VM unavailable
- **Static site detector** — `index.html` detection with busybox httpd
- **dew-virt kernel** — custom monolithic x86_64 kernel config (11MB, zero modules). ARM64 uses Kata pre-built kernel (15.4MB, 30ms boot)
- **Self-signed TLS** — ECDSA cert auto-generated, constant-time token verification, cert fingerprint pinning
- **dew-apps registry** — 11 pre-packaged apps (solcreek/dew-apps, MIT)

### Fixed

- Cloud-init stores token hash (SHA-256), not plaintext
- Deploy token masked in terminal output (prefix + last 4 chars)
- Tarball crash on `.claude/`/`.agents/` directories
- Build output dirs (dist/, build/) preserved despite .gitignore
- `dew share` verifies tunnel reachability before printing URL
- Credential storage migrated from flat files to JSON
- npm postinstall downloads binary from GitHub Releases
- Release workflow: npm publish waits for Release to complete
- ARM64 kernel + initramfs included in releases
- Download URL uses `/latest/download/` instead of hardcoded version

### Changed

- Tagline: "run any app, anywhere" (was "ultra-lightweight VM")
- `dew up` is dev-only (local project). Registry apps moved to `dew app run`
- Help text reorganized with sections and quick-start hint

## [0.2.0] - 2026-05-28

### Added

- **Python profile + detection** — Django, Flask, FastAPI, Streamlit. Supports pip, poetry, pipenv, uv
- **`dew down`** — explicit VM stop with `--json` support, symmetric with `dew up`
- **`dew up --with postgres,redis`** — start services alongside app. Service registry: postgres, redis, mysql, mongo, minio
- **Windows support (WSL2)** — `dew.exe` wrapper delegates to custom WSL2 distro. `dew setup` handles install
- **Release workflow** — GitHub Actions builds macOS/Windows/Linux binaries, initramfs profiles, WSL2 rootfs, install script, checksums on tag push

### Fixed

- sudoers.d directory not existing on Alpine (error noise on every boot)
- Node.js segfault on second boot (musl libc version mismatch after initramfs rootfs copy)
- virtiofs mount not surviving switch_root (fuse + virtiofs modules not loaded in init-stage2)
- `dew up` ignoring `--kernel`/`--initrd` flags (parsed config was discarded)
- Daemon socket connection refused on slow first boot (token handshake retry increased to 30s)

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

[0.4.0]: https://github.com/solcreek/dew/releases/tag/v0.4.0
[0.2.0]: https://github.com/solcreek/dew/releases/tag/v0.2.0
[0.1.0]: https://github.com/solcreek/dew/releases/tag/v0.1.0
