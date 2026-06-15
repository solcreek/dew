# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.40] - 2026-06-15

### Added

- **`dew services`**: lists the predefined services (postgres, redis,
  mysql, mongo, minio) with ready-to-use connection strings, and — when
  a VM is running — marks which are live and on what host port. Replaces
  digging through `/proc/*/environ` to recover credentials. `--json`
  emits a structured envelope.
- **`dew logs <service>`**: prints a `--with` service's container log
  (crun writes it to `/var/log/dew-oci-<name>.log` in the guest) so you
  don't need to know the path or that services run under crun.
- **`dew up --services-only`** (alias `--no-dev`): boot only the
  `--with` services, skipping project detection and the dev server — so
  running just a database no longer needs a fake `package.json`. Emits a
  top-level `{ready,mode:services-only}` once services are up.

### Fixed

- **Service `started` events no longer lie**: `dew up --with` emitted
  `{service,started}` as soon as `dew-oci-run` reported the crun
  container "running", even if the service then died or bound IPv6-only —
  so the forwarded port hit a dead backend ("Connection terminated
  unexpectedly"). Startup is now health-gated: dew polls the guest's
  IPv4 LISTEN socket and only emits `started` once the port truly
  accepts connections, otherwise `{service,failed}` with the container's
  crun log captured at failure time. The started event also reports the
  actual forwarded host port and a ready-to-use connection string.
- **mysql is reachable over the forwarded port**: the mysql service now
  starts with `--bind-address=0.0.0.0` (via a new `ocistage` Append
  option that preserves the image entrypoint) so it listens on IPv4
  instead of the image's IPv6-only default, which made `127.0.0.1:3306`
  hang.
- **`--json`/`--events` output is pure NDJSON**: `dew up` no longer
  interleaves the VM serial console (kernel/boot lines) into the
  lifecycle stream on stdout — the console moves to stderr in
  machine-readable modes, matching `dew vm start`.
- **Port-forward conflicts fall back instead of failing**: when the
  requested host port is busy (e.g. a local postgres on 5432),
  `dew vm forward add` and the `--with` auto-forward now bind a free
  port and report the actual one rather than erroring out.
- **Dead forward backends are no longer silent**: a failed guest-side
  backend connection logs the reason before closing instead of leaving
  the client to hang.
- **`dew exec` has a reliable PATH**: guest commands always run with a
  guaranteed PATH (sbin + `/usr/local/bin` + busybox dirs), fixing
  intermittent `ss: not found` and empty output when the agent booted
  with an empty environment.
- **`dew vm start --help` (and other two-level commands)** print help
  instead of `unknown flag`; subcommand help lookup is now
  namespace-aware.
- **Release build**: bump the x86_64 Alpine `linux-virt` apk to
  `6.12.93-r0`; the previously pinned `6.12.92-r0` was rotated off the
  Alpine CDN, 404-ing the initramfs build.

## [0.7.39] - 2026-06-13

### Added

- **`dew run --image <ref>`** runs any OCI image in a fresh VM. The image
  is pulled and flattened on the host (go-containerregistry), shared into
  the guest over virtiofs, and run with `crun` over an overlay rootfs. Any
  `-- <cmd>` overrides the image entrypoint; `--platform <os/arch>` (with
  `--rosetta`) runs an amd64 image on Apple Silicon. `dew run` is
  exec-oriented and does not auto-forward ports — to reach a long-running
  server, add `--forward <host:guest>`, or use `dew up --with` for managed
  services with automatic forwarding.
- **Content-addressed image cache** (`~/Library/Caches/dew/oci`), keyed on
  the platform-specific manifest digest, with a short-lived tag→digest
  cache. Repeat stages skip the registry pull and flatten. The cache is
  dew's own and fully separate from any Docker image store on the host.
- **`--with` services now persist their data.** Each service's data dir is
  bind-mounted from `/var/lib/dew/services/<name>/data` on the guest ext4
  disk, surviving restarts.

### Changed

- **~22× faster guest disk I/O.** The VM disk is now attached Cached+Fsync
  (was Automatic+Full); Full sync mapped to `F_FULLFSYNC` on macOS/APFS and
  forced every guest flush all the way to physical media. Heavy-fsync
  workloads (image-layer unpack, `npm install`, db writes, first-boot rootfs
  populate) measured ~350 KB/s → ~7.8 MB/s. Committed data stays safe against
  a guest crash (fsync still honored); only an abrupt host power loss risks the
  last unflushed writes — the standard dev-VM trade-off (Lima/Colima do the same).
- **The guest flushes filesystem buffers every 10s.** With the Cached+Fsync
  disk attachment, non-fsync'd writes otherwise sit in the host page cache
  until something fsyncs; the periodic sync bounds what an abrupt VM stop (or
  `dew down`) can lose to at most ~10s, for every stop path. Committed DB data
  was already safe via fsync.
- **`dew up --with` services now run via `crun`, not containerd/nerdctl.**
  Images are pulled on the host and run in the guest through a 2.8MB static
  `crun` over overlayfs. `--with` no longer forces the `standard` profile —
  services run on the project's own profile (a diskless `minimal` is
  upgraded to `node`). Cold-VM container start is ~3× faster.

### Removed

- **In-guest containerd / nerdctl / runc / CNI are gone.** They are no
  longer installed in any profile; the `standard` profile shrinks from
  ~122MB to a `node`+`crun` tier (kept for its larger RAM/disk defaults).
  Breaking: `dew exec nerdctl …` / `containerd` inside the VM no longer
  work. The remote `dew deploy` / `dew serve` runtime is unaffected.

## [0.7.38] - 2026-06-10

### Fixed

- **`dew run` no longer hangs against an unresponsive guest.** vz's
  vsock connect never returns when the guest has no vsock transport
  (e.g. a kernel/initramfs module mismatch); `dew run` blocked on it
  indefinitely — in pipe mode an agent saw zero output until it
  killed the process. Every host-side wait is now bounded by a
  wall-clock deadline (vsock connect, token handshake, exec response,
  serial fallback), failures point at `dew doctor`, and a vz
  typed-nil conn no longer panics the reaper. The serial fallback
  also stops racing two readers on one console and no longer leaks
  boot logs into captured stdout.
- **Second boot of a disk profile no longer kernel-panics after an
  abrupt stop.** First-boot writes (rootfs populate, runtime
  install) sat in the guest page cache; tearing the VM down lost
  them, leaving zero-length `/bin/busybox`/`ld-musl` on disk and
  `switch_root: Exec format error` on every later boot. The init
  scripts now `sync` after populate and after first-boot installs.
- Asset download creates the destination directory instead of
  failing with "no such file or directory" when `--kernel` points
  somewhere fresh.

### Added

- **`dew run --timeout DUR`**: one wall-clock budget for the whole
  run (boot + agent wait + exec). On expiry the VM is stopped and
  dew exits with code 104. Without the flag, behavior is unchanged.
- **`dew vm start --timeout DUR`** bounds the path to readiness, and
  **`--json`/`--events`** emit a one-line NDJSON ready event
  (`{"type":"ready","socket",...,"pid","profile","elapsed_ms"}`)
  on stdout once `dew exec` is available; the guest console moves to
  stderr in these modes so stdout stays parseable.
- **`dew vm status` sees transient states**: while a VM is booting
  (or an ephemeral `dew run` is in flight — it never opens a daemon
  socket) status reports it, with `phase`/`pid`/`mode`/`profile`/
  `started_at` in `--json`. Crash leftovers (dead PID) are ignored
  and cleaned up.
- Smoke test hang guard: a deliberately mute guest must produce a
  bounded non-zero exit, never a hang.

### Changed

- Install docs recommend `npm install -g dew` over `npx dew` — npx
  always resolves the npm package and ignores an installed dew on
  PATH, adding dispatcher overhead on every call.
- Guest kernel bumped to Alpine linux-virt 6.12.92 (x86_64) /
  6.12.93 (aarch64) — the previously pinned apks were rotated off
  the Alpine CDN.

## [0.7.37] - 2026-06-06

### Fixed

- **macOS 26 disk attachment**: `dew run --profile node|standard`
  no longer fails with `VZErrorDomain Code=2 "storage device
  attachment is invalid"` on fresh disk images. The disk path now
  uses the cache+sync attachment API explicitly (still macOS 12+).
  Pre-existing disk images created by older VZ versions are
  incompatible with macOS 26 VZ — when this case is detected the
  error message now includes the exact `rm` command to recover
  (resets VM state for that profile).
- **macOS 26 NAT outbound regression**: `dew run --network` on
  macOS 26 prints a warning explaining the guest can reach the
  host gateway but not the public internet. Apple deprecated the
  legacy NAT attachment we use; replacement is
  `VZVmnetNetworkDeviceAttachment` which the upstream `Code-Hex/vz`
  library hasn't shipped yet (tracking issue #218). VMs still boot;
  `--share <hostdir>` and vsock work for moving bytes between host
  and guest in the meantime.
- **`--share` flag parser** accepts both documented shapes:
  `--share <hostdir>[:rw|:ro]` (host-first, tag derived from
  basename) and `--share <tag>:<hostdir>[:rw|:ro]` (tag-first).
  The help text showed host-first; the parser only accepted
  tag-first. Following the docs produced a confusing
  `stat ro: no such file or directory`.
- **`DEW_DEBUG=1` kernel format hint** no longer false-flags
  x86_64 kernels as "EFI/PE without ARM64 boot header". The
  offset-0x38 ARM64 magic check is now gated by host arch in the
  dump, mirroring the gate already in `dew doctor`.

## [0.7.36] - 2026-06-05

### Added

- Four [agentskills.io](https://agentskills.io)-compatible skill files
  shipped under `skills/`. Coding agents (Claude Code, Cursor, Codex,
  Copilot, Gemini CLI, ~70 others) can install dew's invariants with
  `npx skills add solcreek/dew`. Skills cover the flows that
  historically generated the most field-report confusion: provisioning
  a server, building + deploying, diagnosing a failure, and upgrading.
  Each description includes an explicit "Do NOT use for" clause to
  prevent activation overreach — an agent that loaded all four
  won't reach for `dew-deploy` when the user is on Cloudflare Workers.

### Fixed

- Background update check is now silent on local dev builds (binary
  built without `-ldflags '-X main.version=…'`). The previous code
  printed `Update available: vX.Y.Z (current: vdev)` on every
  invocation — confusing because a dev binary often contains commits
  *ahead* of the released version. Release-built binaries are
  unaffected.

## [0.7.35] - 2026-06-05

### Added

- `dew server create --ssh-key <value>` provisions the new server with
  the supplied public key seeded into `/root/.ssh/authorized_keys`,
  then locks root password auth (`passwd -l root` +
  `PasswordAuthentication no`) in the same boot. Value form is
  auto-detected so agents and humans both have a natural path:
    - inline literal: `--ssh-key 'ssh-ed25519 AAAA... user@host'`
    - stdin: `--ssh-key -`
    - file path: `--ssh-key ~/.ssh/id_ed25519.pub`
  Env equivalents: `DEW_SSH_KEY` (literal) and `DEW_SSH_KEY_FILE`
  (path). With no flag and no env, falls back to auto-discovering
  `~/.ssh/id_ed25519.pub` then `~/.ssh/id_rsa.pub` — secure default
  for the most common interactive case. `--no-ssh-key` opts out
  explicitly for the rare "I want the provider's emailed password"
  flow.
- `dew deploy` SSE stream now includes the server-side stderr on
  extract failures. Previously the client saw only opaque
  `exit status 2` and the actual tar / hook output (the part that
  tells you the cause) only reached the server's journal. The
  symlink-linkname or out-of-space line now appears indented under
  the failure in the client output, no SSH needed.

### Changed

- `dew build` with `type=static` ships ONLY the build output dir
  (`dist`, `build`, `out`, `.next/standalone`, `public` — first match
  wins) instead of the entire project tree. Tarballs shrink
  dramatically, source no longer leaks to the server, and repo-root
  files (lockfiles, `vercel.json`, symlinks like
  `CLAUDE.md → AGENTS.md`) that have no place in a static deploy
  are excluded by construction.
- `dew build --dry-run` now actually skips the user build command
  and tarball write. It runs detection only, prints what would be
  built and shipped, and exits clean — lets framework detection be
  verified without paying the build cost.

## [0.7.34] - 2026-06-05

### Fixed

- `dew server create` no longer hardcodes `Image: "debian-12"`, which
  worked only on Hetzner. The default now comes from each provider's
  capstan spec (`spec.DefaultImage`) — DigitalOcean, Linode, and
  Vultr stop failing at Create time with the cryptic 422
  "specified an invalid image". The new `--image <slug>` flag
  overrides per call for any provider.
- `dew build` writes valid symlink entries into the deploy tarball.
  The previous code passed an empty `linkname` to
  `tar.FileInfoHeader`, which BSD tar on macOS silently downgraded to
  a 0-byte file (masking the bug locally) while GNU tar on every
  Linux deploy target fatally rejected with
  `Cannot create symlink to '': No such file or directory`. Field
  report: a `CLAUDE.md -> AGENTS.md` symlink in a project root broke
  every Linux deploy with an opaque `exit status 2` only readable via
  journalctl on the server.
- `dew deploy <server-name>` now resolves the name to its IP via the
  local server registry before auth lookup. Previously the name path
  failed loudly with `no deploy token for <name>` even when the user
  had auth saved against the actual IP, forcing every deploy through
  the IP form by hand.
- `dew server create` health probe targets `https://` instead of
  `http://`. dew serve listens HTTPS with a self-signed cert at boot;
  the previous probe always timed out, printed a spurious
  "cloud-init may still be running" warning, and burned ~3 minutes
  per create even when the server was ready in 30 seconds.

### Added

- `dew build` publishes a canonical `.dew/build.tar.gz` pointer at
  the project root in addition to the legacy `<appname>.tar.gz` path.
  `dew deploy` auto-detect now reaches the canonical pointer first,
  so a retry from any cwd inside the project resolves to the same
  bytes — addresses the "no tarball found" retry friction reported
  after extract failures.
- `dew deploy` error message when auto-detect fails now lists which
  paths were tried and points at `--tarball <path>` as the explicit
  escape hatch.

## [0.7.33] - 2026-06-05

### Added

- Content-addressed asset cache. Kernel + initramfs files now live at
  `~/.local/share/dew/<asset>.<sha8>` instead of a shared un-suffixed
  filename. Multiple dew binaries (different versions → different
  SHAs) coexist without fighting; downgrade + re-upgrade is free
  (both versions' bytes stay on disk).
- Release builds embed the SHA256 of every asset into the binary at
  build time. Downloads verify against the embedded SHA before
  installing at the destination path; a CDN drift or mid-transit
  corruption fails loudly here rather than letting Apple VZ reject
  the bytes later with `Code=1`. Dev / local builds (no embedded
  manifest) skip verification — the user is expected to know what
  they built.

### Changed

- The 2026-06-04 M4 Max bug class — stale asset from a previous
  install reused silently on upgrade — is now structurally impossible
  on release builds. Legacy files at the pre-0.7.33 un-suffixed path
  are left in place untouched (no destructive cleanup); a new content-
  addressed file gets downloaded alongside. Users can reclaim the
  disk on their own schedule.

## [0.7.32] - 2026-06-05

### Added

- `dew assets pull --force` re-downloads kernel + initramfs even when
  cached files exist. The hint that doctor and the debug dump emit
  ("run dew assets pull --force") now lands somewhere real.
- `dew doctor` reads the cached kernel's ARM64 boot header magic on
  arm64 hosts and reports format as a first-class check. The 2026-06
  M4 Max report — multi-day debug of a 9MB stale EFI-stub-only
  kernel left by an earlier install — would now surface as a single
  failing check with the actionable remediation in the same line.

### Fixed

- `install.sh` no longer aborts on systems that ship `shasum` but not
  `sha256sum` (default macOS install). The previous fallback pattern
  exited on the first miss before the `||` could fire.
- `DEW_DEBUG=1` kernel format hint no longer false-flags every valid
  ARM64 Linux kernel as "EFI/PE bad". Real ARM64 kernels start with
  an MZ EFI stub that doubles as a valid ARM64 branch — the
  authoritative check is the `ARM\x64` magic at offset 0x38, which
  the heuristic now reads and reports. The genuinely broken case
  (stale EFI-stub-only kernel from an earlier install, no ARM64 boot
  header) is now classified as "EFI/PE without ARM64 boot header" and
  comes with the actionable hint `run dew assets pull --force`.

## [0.7.31] - 2026-06-04

Diagnostic improvements for the opaque `VZErrorDomain Code=1` class
of VM boot failures, plus the Windows installer arch fix.

### Added

- `DEW_DEBUG=1` env var dumps the VM config (CPU, memory, kernel path
  + size + format magic, cmdline, devices, host model, macOS version)
  to stderr before `machine.Start()`. Apple's `VZErrorDomain Code=1`
  rarely surfaces the underlying cause; the dump lets bug reports
  show whether the config or the platform is at fault. Kernel format
  heuristic flags gzip/zstd/ELF/PE wrappers, which Apple VZ rejects
  for ARM64.
- `dew doctor --verbose` runs the boot test with `DEW_DEBUG=1`
  enabled and surfaces the captured dump alongside the check result.
  When a check fails, the report now suggests re-running with this
  flag so the user can attach the full diagnostic to a bug report.
- `dew doctor` runs `codesign --verify --strict` against the binary.
  Entitlement presence is necessary but not sufficient — npm / tar
  extraction can leave the entitlement readable while invalidating
  the signed CodeDirectory hashes; VZ then refuses to boot with the
  same opaque `Code=1`.
- `dew server create` pre-checks plan + region orderability against
  the provider's availability catalog and fails fast with a clear
  message before hitting a cryptic provider 422.

### Changed

- VM start failure messages include the Mac model and macOS version
  so bug reports surface hardware specifics without the user needing
  to remember `sysctl hw.model`.

### Fixed

- Windows installer picks the correct binary for the host CPU
  (`PROCESSOR_ARCHITECTURE` → `arm64` / `x86_64`) instead of always
  downloading x86_64. ARM hosts no longer run the dispatcher through
  emulation. Installer also prints an upgrade hint footer.
- Website install URLs unified under `dewvm.dev/install.{sh,ps1}`
  for every platform.

## [0.7.30] - 2026-06-03

### Changed

- **npm: install via `npm i -g dew`** (unscoped, primary). The
  scoped `@solcreek/dew` continues to publish the same dispatcher
  bytes at parity so existing installers keep working without
  silent break, but every user-facing surface now points at the
  unscoped name. CLI tool convention is overwhelmingly unscoped
  (vercel, bun, pnpm, wrangler, astro, typescript); the shorter
  form is friendlier for word-of-mouth, demos, and scripts.

  Both names will continue to ship at parity for the foreseeable
  future. Eventually `@solcreek/dew` will gain an npm deprecation
  message nudging new installs to `dew`, but never be unpublished
  (defensive ownership against typosquatting).

## [0.7.29] - 2026-06-03

### Fixed

- **Windows: `dew up` no longer eats backslashes in the project
  path.** v0.7.28 invoked `wslpath -a C:\Users\foo\proj` and
  wslpath errored out because the path arrived as `C:Usersfooproj`
  with the backslashes stripped. The agent's bug report pinned
  the cause: `wsl.exe -- COMMAND ARGS` dispatches via /bin/sh -c
  inside the distro even when the host calls exec.Command with
  separate argv elements, and that shell strips unquoted
  backslashes. Normalize the Windows path to forward slashes
  with filepath.ToSlash before handing it off — Windows APIs
  accept both spellings, Linux paths use /, and the shell passes
  / through untouched.

## [0.7.28] - 2026-06-03

### Added

- **Windows: `dew up` for Node-style projects.** Detects
  package.json, ensures the WSL2 distro is running, translates
  the Windows project path to its /mnt/<drive>/... mount via
  `wslpath`, runs `npm install` if node_modules is missing, then
  `npm run dev` (or `start`) inside the distro with stdout /
  stderr streamed straight to the user's terminal. The dev
  server's port (Vite :5173 etc.) is reachable on the Windows
  host through WSL2's mirrored networking.

  Scope is intentionally narrow — Node only, no framework-
  specific port detection, no streaming health probe. Heavier
  project-aware behavior (port redetection, multi-runtime
  profiles) can land iteratively as Windows users hit specific
  gaps. The dev server's own banner gives the user the URL it
  printed (Vite always logs `Local: http://localhost:5173/`).

## [0.7.27] - 2026-06-03

### Fixed

- **Windows: `dew vm start` / `dew exec` work without an in-distro
  helper binary.** The previous design forwarded every command to a
  `dew-native` binary inside the WSL2 distro that never existed in
  the shipped rootfs (and wouldn't have understood `vm start` /
  `exec` semantics if it did — `cmd/dew-linux` only handles
  serve/update/version). The wrapper now translates commands
  directly into WSL2 operations:

      dew vm start    → ensure the distro is running (wsl auto-starts on use)
      dew vm stop     → wsl --terminate dew
      dew vm status   → check whether the distro is registered + alive
      dew exec <cmd>  → wsl -d dew -- <cmd>
      dew down        → alias for vm stop

  Aligns with the actual WSL2 model: Microsoft manages the kernel
  and VM lifecycle; the rootfs is the only thing we control.
  Project-aware `dew up` stays macOS-only for now (no Windows
  story yet for the project-detect + containerd-orchestrate path).

## [0.7.26] - 2026-06-03

### Fixed

- **Windows: distro presence check probes `wsl -d dew -- true`
  directly** instead of parsing `wsl -l -q` output. v0.7.25's
  UTF-16LE decoder worked in theory but field reports from
  Windows-on-ARM showed the parse still missed the distro under
  some WSL versions where the same command behaves differently
  through Go's exec.Command than through an interactive shell.
  Probing exits 0 iff the distro is registered and can start,
  which is the actual signal we want — no encoding to misjudge.

## [0.7.25] - 2026-06-03

### Fixed

- **Windows: `dew setup` now auto-downloads the rootfs.** Previous
  builds errored "rootfs not found — download from GitHub Releases"
  (the auto-download was a TODO). Setup now fetches
  `dew-rootfs-{x86_64,aarch64}.tar.gz` from the latest release
  based on the wrapper's GOARCH, streams to a `.part` file, then
  atomic-renames so a partial download can't leave a corrupt
  archive that subsequent runs trust.
- **Windows: `dew vm start` / `dew exec` now find the imported
  distro.** The lookup parsed `wsl -l -q` output as UTF-8, but
  wsl.exe emits UTF-16LE with a BOM; the partial NUL-strip
  workaround left the BOM bytes attached to the first line so a
  freshly-imported distro could read as absent. Decode UTF-16LE
  explicitly with BOM detection (also handles UTF-16BE and plain
  ASCII fallbacks for older WSL builds).

Both bugs blocked the unattended Windows-on-ARM flow after
v0.7.24 shipped the matching aarch64 rootfs. With both fixed,
`dew setup → dew vm start → dew exec` runs end-to-end without
manual intervention.

## [0.7.24] - 2026-06-03

### Added

- **aarch64 WSL2 rootfs** — `dew-rootfs-aarch64.tar.gz` ships
  alongside the x86_64 rootfs so `dew setup` on Windows-on-ARM
  imports a kernel-matching distro. Without this, the aarch64
  Windows binary from v0.7.23 had nowhere to land — WSL2 on ARM
  runs an aarch64 kernel and rejects an x86_64 rootfs. The new
  asset closes the loop end-to-end on Windows ARM.

## [0.7.23] - 2026-06-03

### Added

- **Windows ARM64 binary** — `dew-windows-arm64.exe` ships alongside
  the existing x86_64 wrapper. Runs natively on Windows-on-ARM
  laptops and inside emulated Windows-ARM VMs, where the x86_64
  binary can't be exercised end-to-end (Microsoft's x64 emulator
  needs hardware virtualization that emulated guests don't have).
- **`dew exec --timeout`** — override the guest agent's 30 s default
  for long-running commands (`dew exec --timeout 10m sh -c '...'`).
  Without this, image pulls and other slow guest work were silently
  cut off at 30 s with an empty-stderr failure.

### Changed

- **Node / Python / standard profiles default to 4 vCPU + 2 GB RAM**
  (was 1 vCPU + 1 GB). Single-vCPU was the bottleneck on real-world
  install workloads (npm install reify, bundler transforms); the
  bump brings TanStack-class workloads close to host parity. The
  minimal profile is unchanged at 1 vCPU + 512 MB so ephemeral
  `dew run` commands stay light.
- **`dew up` redetects the dev-server's actual port** and adds a
  runtime forward when frameworks override the manifest default
  (e.g. Vite picking a different port). Provisional forward also
  shifts off occupied host ports automatically.
- **Runtime port forwards bind both IPv4 and IPv6** (127.0.0.1 +
  ::1) so `localhost:PORT` resolves regardless of which family the
  client picks.
- **Hint messages name the current command form** — `dew vm status`
  and other places that prompt the user to start the VM now say
  `dew vm start --profile standard` rather than the legacy
  `dew start`, so copy-paste doesn't trigger the deprecation
  warning.

## [0.7.22] - 2026-06-02

The VM-lifecycle commands move under a `dew vm` namespace and
networking is on by default for fresh VM boots. The legacy top-level
commands keep working with a deprecation hint and are scheduled for
removal in v0.9.x.

### Added

- **`dew vm` namespace** — `dew vm start | stop | status | forward`
  group VM-primitive commands distinct from `dew up` (project dev
  workload). The root help now surfaces both side by side.
- **`dew status`** — query whether a VM is running without side
  effects. Distinguishes a clean stopped state from a stale socket
  (crash leftover). Always exits 0.
- **`dew forward add | remove | list`** — manage host→guest port
  forwards on a running VM without restarting it. Initial forwards
  from `--forward` flags go through the same daemon path.

### Changed

- **`dew vm start` enables networking by default.** The help text
  always claimed this; the implementation now matches. Pass
  `--network-policy=restricted` to lock down outbound.

### Deprecated

- Top-level `dew start | stop | status | forward` — use the
  `dew vm` namespace forms. Aliases keep working for one release
  cycle and print a stderr hint pointing at the new path.

## [0.7.21] - 2026-06-02

`dew share` gains an NDJSON event stream for tools and agents that
want to react to tunnel lifecycle in real time. One JSON object per
line on stdout; existing `--json` single-shot output is unchanged.

### Added

- **`dew share --events <port>`** — emit lifecycle events as NDJSON.
  Events: `starting`, `tunnel-url`, `established`, `probe-timeout`,
  `closed`. Each carries `event` + `ts` (RFC3339Nano) plus event-
  specific fields. Help text and contract pinned by unit tests so
  downstream parsers can rely on the shape.

## [0.7.20] - 2026-06-02

The pre-packaged apps surface — deprecated in v0.7.19 — is removed.
dew is now a pure sandboxed-Linux-compute primitive: it boots VMs,
runs commands in them, mounts directories, deploys tarballs. The
curated app catalog moved to a standalone tool.

Also: global no-value flags (`--json`, `--events`, `--stream`,
`--dry-run`) now work before *or* after the subcommand
(`dew --json apps`-style ordering, common in agent one-liners),
and `dew run` / `dew up` help text now documents ephemeral
semantics and the `/app` mount path.

### Removed

- **`dew apps`** — was: browse pre-packaged catalog. Now: usage
  error pointing at `dew run` for arbitrary container workloads.
- **`dew app run <name> / stop / list`** — same removal.
- **`dew install <name>`** — same removal.
- The internal `appManifest` struct, `registryBase` URL, and
  `fetchManifest` helper.

A user who runs any removed subcommand gets:

```
dew: dew apps was removed in v0.7.20.
The pre-packaged apps catalog now lives in a separate tool.
For arbitrary container workloads in dew, use:
  dew run --network -- <cmd>
```

Exit code 2 (CodeUsage).

### Added

- **Global flag position freedom** — `dew --json apps`,
  `dew --events up`, `dew --dry-run up` etc. all work. Previously
  the dispatcher took `--json` literally as a subcommand and
  errored. Flags that take values still go in their command-
  specific position.
- **`dew run --help`** now documents that state is ephemeral
  across invocations and points at `dew start` + `dew exec` for
  persistent VMs.
- **`dew up --help`** now documents the `/app` virtiofs mount
  path so `dew exec` users know where to `cd`.

### Fixed

- **`brew info solcreek/tap/dew`** description now matches the
  current positioning ("Sandbox Linux compute on macOS — no
  Docker, no VPN, agent-friendly") instead of the pre-v0.7.16
  tagline.

## [0.7.19] - 2026-06-01

Deprecates the pre-packaged apps surface. dew is settling on its
identity as a sandboxed Linux compute primitive; the curated app
installer is a different product shape and will move to a separate
tool. All existing functionality keeps working through the
deprecation window.

### Changed

- **`dew apps` / `dew install` / `dew app run/stop/list`** now print
  a one-line deprecation notice on stderr before doing their work:

      dew: the pre-packaged apps catalog will move to a separate
           tool in a future release.
           Existing apps keep working until then; see
           github.com/solcreek/dew ROADMAP for details.

  Behavior is unchanged; `--json` suppresses the notice so
  machine-readable output stays parseable.

- **`dew --help`** main usage no longer lists the Apps block. The
  per-subcommand `dew app --help` carries the same deprecation note
  up top so anyone who finds the surface learns immediately.

- README front-page example, "Run open-source apps" section,
  architecture diagram, and profile table updated to reflect the
  primitive-first identity.

### Notes for tool authors

- The deprecation notice deliberately does not name the target
  tool, per dew's repo-independence policy. Watch the ROADMAP for
  the migration plan once the new repo lands.

## [0.7.18] - 2026-06-01

UX correctness + agent discoverability pass. Closes seven items
flagged by a four-agent fresh-eyes test of dew. No new features —
this release is about making the existing surface honest.

### Fixed

- **`dew up --dry-run` actually doesn't boot the VM.** It used to
  parse the flag and silently ignore it, then run a full boot +
  install. Now it prints the plan (project, profile, install/dev
  commands, ports) and exits.

- **`dew run -- sh -c '...'` no longer eats output.** Argv passed
  after `--` is now sent straight to the guest agent; previously
  it was joined with spaces and re-wrapped in `/bin/sh -c`, so an
  outer shell parsed the user's inner `sh -c` and ate the first
  argument. The same fix applies to `dew exec`. Single-string
  invocations (`dew run "echo a; echo b"`) keep their legacy
  shell wrap.

- **Per-subcommand `--help`** works for `up`, `run`, `exec`,
  `start`, `down`, `build`, `deploy`, `share`, `app`, `apps`.
  Used to error with `unknown flag "--help"`. Each block lists
  the flags + 2–3 representative invocations.

- **Flags after the positional argument** no longer get silently
  dropped. `dew up <dir> --dry-run` and `dew up <dir> --json`
  both work now.

- **`dew apps --json`** emits one JSON envelope with per-app
  manifest data (name, version, runtime, port, image, tags).
  Used to ignore `--json` and print the human catalog.

- **`dew app run --dry-run --json`** emits the resolved plan as
  JSON (app, version, runtime, image, port, host_port). The
  human-mode "Would pull " line for node-runtime apps now reads
  "No image to pull (node runtime — built from source)" instead
  of leaving a blank.

### Added

- **`{"type":"ready", "url", "port", "framework", "elapsed_ms"}`**
  event for `dew up`. Single grep target for agents tracking
  readiness — no more parsing `http://` out of mixed human +
  kernel-dmesg output.

- **`--network-policy=restricted` warns when `--allow-host` is
  empty.** Previously the most opaque failure mode in dew: apk /
  npm / pip would silently fail with no output because outbound
  traffic was blocked. The warning fires before VM boot so the
  user can ^C and retry.

### Removed

- **`dew session create / exec / destroy`** removed. The CLI
  stored VM handles in an in-process map, so the next process
  couldn't reach the VM — `session exec` always errored with
  "sessions are in-process only." Running `dew session ...` now
  exits 2 with a migration hint pointing at `dew up` and
  `dew start`. The internal `internal/session` package stays for
  the in-process callers (`dew up`, `dew run`).

## [0.7.17] - 2026-06-01

`dew up` now skips `npm install` when nothing has changed.

### Added

- **`node_modules` cache** for `dew up`. The first time a project
  boots, dependencies install as before and the result is stamped
  with the lockfile's hash. On subsequent boots, if the lockfile
  hasn't changed, the install step is skipped entirely — boot
  becomes <500 ms warm vs. the cold install time.

  The cache is keyed by the project directory (so each project
  has its own `node_modules` — no cross-project pollution) and
  invalidates automatically when the lockfile changes. Symlinked
  project paths resolve to the same cache entry as their target.

  Supported lockfiles: `pnpm-lock.yaml`, `bun.lock`, `bun.lockb`,
  `yarn.lock`, `package-lock.json`. Projects without a supported
  lockfile fall back to the pre-0.7.17 behavior (install on every
  boot) — without a stable input hash, caching isn't safe.

  No new flags. No new CLI commands. The cache lives inside the
  existing node-profile VM disk and survives `dew down`.

### Fixed

- A `dew up` that crashed mid-install no longer leaves a
  half-populated `node_modules` that the next boot would treat as
  valid. The cache uses a write-ahead pointer
  (`.dew-stamp.inprogress.json`) that the next boot detects and
  wipes before letting a new install run.

## [0.7.16] - 2026-06-01

Agent-native hardening: typed errors, classified exit codes, and a
versioned `--json` envelope. The change is additive — shell scripts
that only branched on `$? == 0` keep working, and the only behavior
shift in shell mode is that `dew bogus` and unknown-command paths
now exit 2 (usage) instead of 1 (generic).

### Added

- **`pkg/dewerr`** — typed error package with stable `Code` constants
  and `Slug()` mapping. Public under `pkg/` so the future agent SDK
  can pattern-match on errors without depending on the dew binary.
- **`docs/exit-codes.md`** — the public contract. Codes never get
  re-mapped; new categories take a new code from the reserved 106-119
  range; the `--json` envelope is versioned via `schema_version`.

### Changed

- **Exit codes are now classified:**
  - 0 success
  - 1 generic
  - 2 usage / validation
  - 100 auth (token expired, unauthorized)
  - 101 network (DNS, connection refused — retryable by default)
  - 102 not_found (resource doesn't exist)
  - 103 conflict (state mismatch, precondition failed)
  - 104 timeout (retryable by default)
  - 105 unavailable (rate-limited, disk full — retryable)
  - 106-119 reserved, append-only
  - 120-127 deliberately unused (timeout(1) / chroot tradition)
  - 128-255 untouched (POSIX signal range)
- **`--json` error envelope is now stable + versioned:**
  ```json
  {
    "ok": false,
    "schema_version": "1.0",
    "error": {
      "code": "auth",
      "exit_code": 100,
      "message": "...",
      "retryable": false,
      "hint": { ... }
    }
  }
  ```
- **`dew run` and `dew exec` no longer collide exit-code spaces with
  the guest under `--json`:**
  - shell mode (no flag): guest's exit code passes through to the
    host shell as before
  - `--json` mode: dew exits 0 if dew itself was fine, and the guest
    code lives in `data.guest_exit_code`. Agents can stop
    disambiguating "did dew fail or did the guest fail" from `$?`.

### Fixed

- The previous error-classification (`strings.Contains(msg, "unauthorized")`)
  is gone. Codes are carried in typed `errs.Error` values that survive
  arbitrary wrapping via `errors.As`, so renaming an error message
  no longer silently re-routes it.

### Removed

- `internal/jsonerr` — replaced by the typed `pkg/dewerr` package.

## [0.7.15] - 2026-06-01

### Fixed

- `dew rollback` no longer silently calls a server-side stub that
  returns "success" while doing nothing. The CLI now refuses with
  `dew rollback is not yet implemented — the deploy receiver
  doesn't persist version history. Tracked in ROADMAP. Workaround:
  re-deploy the previous build tarball with dew deploy <target>`.
  Exit code is non-zero. `--json` returns
  `{"ok":false,"error":"not_implemented","workaround":...}`.

### Changed

- README and `dew --help` no longer advertise rollback as a shipped
  feature. The architecture diagram and the deploy command list both
  reflect what `dew serve` actually does today (containerd, TLS,
  health checks — no rollback).
- ROADMAP gains a "Restore previous version after deploy (rollback)"
  entry under Mid-term, describing what the receiver needs to gain
  (version history retention + atomic switch endpoint) before the
  CLI can do anything useful.

## [0.7.14] - 2026-06-01

### Changed

- Each spinner step on `dew up` now shows how long it took:
  `installing deps ✓ 12.3s` instead of `installing deps ✓`. Format
  adapts from `380ms` for sub-second work up to `1m12s` for the
  long-running ones (apk fetch on a slow network, big monorepo
  installs).

### Fixed

- Build-tools install failures (network unreachable, apk repo error)
  now surface a one-line reason in the spinner instead of being
  swallowed: `installing build tools (sharp) ✗ 4.1s — apk install
  failed — DNS/network unreachable`. The full apk stderr is preserved
  in the `--events` stream.
- The "install failed" summary now picks the most useful suggestion
  for the failure mode: peer-dep conflicts get "try
  --legacy-peer-deps"; node-gyp errors after the build tools were
  already installed get "build tools installed but compile still
  failed; check stderr above"; node-gyp errors when build tools were
  not installed get "looks like a missing-toolchain failure dew
  didn't catch; please file an issue with the package name".

### Tests

- Smoke test gains two regression guards for the v0.7.13 lockfile
  scanner: a stock Vite+React project must NOT trigger the
  build-tools install (false-positive guard), and a project that
  pins sharp MUST trigger build tools before npm install (correctness
  + ordering guard).

## [0.7.13] - 2026-06-01

### Changed

- `dew up` now installs the native-build toolchain (gcc, make, python3)
  only when the project's lockfile lists a package that needs it
  (sharp, sqlite3, bcrypt, canvas, node-pty, node-sass and friends).
  Earlier versions ran that apk install in the background on every
  cold boot of the node profile, regardless of whether the project
  needed it — that's ~50 MB of disk and ~30 s of activity that 80 %
  of Vite/Next/Astro users never used. Projects that *do* need it
  now see a clear progress line: `installing build tools (sharp)…`.
- If npm install fails with a node-gyp / g++ / python error and the
  lockfile-scan missed (transitive dep, alias), `dew up` automatically
  installs the toolchain and retries the install once.
- The trailing `dew: background install done: build-base python3`
  message that used to appear well after the dev server started no
  longer occurs.

## [0.7.12] - 2026-06-01

### Fixed

- **Apple Silicon: `dew up` first-boot Node install no longer blocks for
  30-60 s.** The aarch64 initramfs is now built with `nodejs`+`npm` baked
  in, matching what the x86_64 release has had since 0.7.x. Previously the
  aarch64 initramfs was built on the macOS runner where `apk-tools-static`
  can't run, so the bake silently no-op'd and every cold `dew up` on
  Apple Silicon had to fetch the full Node runtime from Alpine repos
  before the dev server could even start. Cross-built from the Linux
  runner now, so both arches ship with the runtime preinstalled.
- Release pipeline staging step matches the new artifact layout.

## [0.7.11] - 2026-06-01 — withdrawn

Failed at the goreleaser staging step before any release artifacts
were published. Re-shipped as 0.7.12 with the same scope plus a CI
fix.

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
