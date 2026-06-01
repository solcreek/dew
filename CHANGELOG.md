# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.11] - 2026-06-01

### Fixed

- **Apple Silicon: `dew up` first-boot Node install no longer blocks for
  30-60 s.** The aarch64 initramfs is now built with `nodejs`+`npm` baked
  in, matching what the x86_64 release has had since 0.7.x. Previously the
  aarch64 initramfs was built on the macOS runner where `apk-tools-static`
  can't run, so the bake silently no-op'd and every cold `dew up` on
  Apple Silicon had to fetch the full Node runtime from Alpine repos
  before the dev server could even start. Cross-built from the Linux
  runner now, so both arches ship with the runtime preinstalled.

## [0.7.10] - 2026-06-01

### Fixed

- **Apple Silicon: `dew up` / `dew run` failed at VM start** with
  `VZErrorDomain Code=1 "Internal Virtualization error"` on every cold
  invocation. Cause: Alpine 3.21's `linux-virt` ARM64 kernel ships in EFI
  zboot format (PE32+ wrapper around a gzip-compressed payload), and
  Apple's Virtualization framework only accepts the raw ARM64 Image. The
  release pipeline now detects the wrapper and extracts the payload
  before publishing. Affected v0.7.7, v0.7.8 and v0.7.9.

  Intel Macs were unaffected (Alpine ships x86_64 as plain bzImage,
  which VZ does accept).

## [0.7.9] - 2026-05-31

### Fixed

- `dew up` end-to-end aha works again. v0.7.8 fixed DHCP but a deeper
  layer was still broken: the guest agent's exec timeout was 30 s
  while `npm install` of a fresh Vite + React project legitimately
  takes 30-60 s, so install was being killed mid-flight and reported
  as "install failed" with an empty error message. The dev server
  then never had `node_modules` to start from. Install now has a
  10-minute timeout, matching the bounded-but-long nature of the
  call.
- First-boot apk install of `build-base` and `python3` no longer
  blocks the guest agent from coming up. The released node-profile
  initramfs bakes node + npm, so only the optional native-build
  tooling is fetched at first boot; that fetch now runs in the
  background while `dew up`'s `npm install` proceeds in parallel.
  Cold-start `dew up` on a Vite project went from "never reaches the
  URL" to a reliable 40-65 s end-to-end. Verified over 10 fresh
  cold boots, 10/10 ✓.

### Tests

- New smoke-test entry runs the actual `dew up` aha — scaffolds a
  Vite + React project, boots, asserts `curl http://localhost:5173/`
  returns the React HTML within 180 s. Catches end-to-end layered
  failures that the per-component tests can't reach.

## [0.7.8] - 2026-05-31

Same user-visible scope as 0.7.7 plus two regressions found right
after that release.

### Fixed

- First-boot DHCP is reliably fast again. On the prior release the
  pruned initramfs booted so quickly it could race Apple VZ's NAT
  startup. busybox `udhcpc` was being invoked in a way that, on
  failure, retried the 3-packet discovery burst indefinitely without
  exiting, blocking init for up to ~90 s while printing
  "broadcasting discover" repeatedly. Now a single patient attempt
  covers the slow case and exits cleanly either way. Validated over
  23 cold boots: every one reached an IP within ~32 s end-to-end.
- `dew exec ping` and other raw-socket tools work again. The previous
  release tightened the in-guest agent's capability set, which
  silently removed `CAP_NET_RAW` (raw sockets) and `CAP_SYS_ADMIN`
  (the bind-mount path used by `dew up`). Both are now restored —
  the VM is the isolation boundary, so in-guest capability
  restrictions do not buy real safety and have only blocked
  legitimate work.

### Tests

- New smoke-test entry boots a fresh node-profile VM three times and
  asserts each cold boot acquires a DHCP lease within 60 s. Guards
  against the regression from 0.7.7.

## [0.7.7] - 2026-05-31 — withdrawn

Shipped two regressions vs. 0.7.6:

- First-boot DHCP could take ~80–90 s on a fraction of cold boots
  due to a busybox `udhcpc` retry loop racing Apple VZ NAT startup.
- `dew exec` lost `CAP_NET_RAW` (and `CAP_SYS_ADMIN`), breaking
  `ping` and the bind-mount path used by `dew up`.

Re-shipped as 0.7.8 with both fixes plus a regression guard.

The original 0.7.7 release notes (as shipped) covered: identical
user-visible scope to the withdrawn 0.7.6 plus a Linux-initramfs
build-pipeline fix (apk's per-package post-install scripts needed a
capability the GitHub-hosted runner doesn't grant; skipped, since the
files those scripts would have touched are already provided by the
base rootfs).

## [0.7.6] - 2026-05-31 — withdrawn

Did not produce binaries: the Linux initramfs build step failed on
the release runner. Re-shipped as 0.7.7 with the same changes plus
the build-pipeline fix.

### Changed

- First-time `dew up` on a Node or Python project is much faster on
  installs from official releases. The runtime ships ready-to-use
  inside the profile instead of installing over the network on the
  user's first boot. Native-build tooling (`build-base`) and Python
  alongside Node still install lazily on first boot if the user needs
  them — no functional regression.
- First-time asset download is noticeably quicker. The kernel and VM
  image now fetch in parallel, and the image itself is much smaller:
  the minimal and node profiles dropped from ~30 MB to ~6 MB; the
  standard profile from ~128 MB to ~105 MB. Existing installs reuse
  cached assets — this only affects fresh installs and `dew update`.
- `dew up` in a directory without a detected project now suggests only
  commands that work today (`dew up --profile minimal`,
  `dew start --profile minimal`, `dew app run`). The earlier list
  pointed at a planned command, which would have produced a misleading
  "command not found" follow-up for anyone copying it.
- `dew --help` opens with the project's actual one-line description.
- New `--network-policy restricted` plus `--allow-host HOST`. With
  these on, the guest's outbound traffic is default-deny — only
  loopback, DNS, and the explicitly allowed IPs are reachable. The
  default remains open for now; the restricted mode is intended for
  callers running untrusted code who want to constrain egress.
  Hostname-aware allowlist (versus the current IP-only form) is on
  the roadmap.

### Fixed

- `dew run` now defaults to the minimal profile, boots within seconds,
  and reliably reaches the guest. The earlier default (the largest
  profile) made every casual one-off command download 135 MB of
  assets and then time out before the guest finished its first boot.
  `dew run -- uname -a` now completes in ~15 s on a fresh install.
- `dew --help` and the post-install hint point at this one-liner as
  the first thing to try, instead of the slow demo command.
- `dew run --share` and `dew run --network-policy=restricted` now
  actually take effect. Both flags were parsed but never reached the
  guest, so the share never mounted and the policy was a no-op.
- `dew up` reliably serves the dev URL of a Vite, Next.js, Astro,
  Nuxt or SvelteKit project end-to-end from the host. The dev server
  used to die a few seconds after launch (it was attached to the
  short-lived spawn shell), and many frameworks bound to a guest-only
  loopback address that the host port forward couldn't reach. Both
  are fixed; `curl http://localhost:<port>/` from the host now returns
  the rendered page.
- `dew up` no longer carries stale install state from earlier runs.
  Each cold boot starts with a fresh guest-local `node_modules`, so
  rare crashes caused by inconsistent native bindings left over from
  a previous install no longer happen on subsequent runs of the same
  project.

## [0.7.5] - 2026-05-30

### Changed

- `dew up` in a directory without a detected project now surfaces
  three suggested next steps and a docs link, rather than a flat
  error.

### Security

- Hardened guest-agent request authorization.

## [0.7.4] - 2026-05-30

### Changed

- Cold first-run completes reliably on slower connections and shows
  in-progress phase messages. Subsequent runs remain sub-second.

## [0.7.3] - 2026-05-30

### Fixed

- VM start is more reliable when local assets get out of sync with
  the installed binary version. Missing profile assets now auto-fetch
  cleanly instead of falling back to an incompatible bundle.

## [0.7.2] - 2026-05-30

### Fixed

- `dew app run` no longer fails when an unrelated process on the host
  is already listening on a common port.

## [0.7.1] - 2026-05-30

### Fixed

- `brew install solcreek/tap/dew` is now actually populated on release.
  The v0.7.0 release shipped binaries but the tap formula didn't
  publish.

## [0.7.0] - 2026-05-30

**Distribution overhaul.** Three install paths, one set of binaries
under the hood.

### Added

- `brew install solcreek/tap/dew`
- `curl -fsSL https://dewvm.dev/install.sh | sh`
- `npm install -g @solcreek/dew` — small dispatcher; the actual binary
  is fetched from the release on first run and cached
- Build provenance attestation published with each release

### Changed

- The npm install no longer touches binary bytes at install time.

## [0.6.0] - 2026-05-30 — **WITHDRAWN**

> **Note:** v0.6.0 was unpublished from npm shortly after release
> because `npm install @solcreek/dew@0.6.0` failed. v0.7.0 ships the
> same intended functionality on a different distribution model and
> is the recommended upgrade. Direct binary downloads from the v0.6.0
> GitHub Release remain available.

### Added

- `dew server start/stop/restart/status`
- `--json` output for all `dew server` subcommands

### Fixed

- `dew app run` now reliably starts containers on first use, and
  remains reliable across upgrades.

### Removed

- `cmd/dew-serve/` — duplicated `cmd/dew` after the v0.4 unification;
  systemd unit renamed to `dew.service`

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
