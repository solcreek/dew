# Roadmap

## v0.1 — VM Foundation

macOS. Apple Virtualization.framework. Alpine Linux.

- `dew up` — zero-config project detection (Vite, Next.js, Astro, Nuxt, SvelteKit)
- Three profiles: minimal, node, standard (containerd)
- vsock exec with `--json`, `--events`, `--stream`
- Port forwarding, persistent disk, network isolation
- Auto-download assets on first use

## v0.2 — Cross-platform

- Windows support (WSL2 backend)
- Python profile (Django, Flask, FastAPI, Streamlit)
- `dew up --with postgres,redis` — services alongside app
- `dew down`, CLI spinner, release workflow

## v0.3 — Deploy Pipeline

- `dew build` — package app for deployment
- `dew deploy` — tarball + image mode, SSE progress
- `dew serve` — production deploy receiver with TLS
- `dew share` — temporary public HTTPS URL
- `dew server create/list/destroy` — VPS provisioning via capstan
- `dew auth`, `dew env`, `dew rollback`
- Self-signed TLS, token hash, constant-time comparison

## v0.4 (current) — Apps + CLI Restructure

- **`dew app run/stop/list`** — run open-source apps from registry
- **`dew apps`** — browse 11 pre-packaged apps (dew-apps catalog)
- **`dew-serve` standalone binary** — cross-compiled Linux (7.1MB)
- **dew-virt kernel** — custom monolithic x86_64 (11MB, zero modules). ARM64 via Kata pre-built (15.4MB, 30ms boot)
- **CLI restructure** — Dev → Share → Apps → Deploy → Infrastructure → Advanced
- **Containers inside Dew VM** — no host Docker dependency
- **Static site detector** — index.html with busybox httpd
- **npm @solcreek/dew** — auto-download binary via postinstall

## v0.5 — Production Ready

- **`dew logs`** — stream app logs from `dew serve`
- **`dew app list --json`** — structured output for agents
- **ACME / TLS** — automatic Let's Encrypt certificates via certmagic
- **`dew domain add/remove`** — custom domains with auto cert issuance
- **`dew.toml`** — project config (port, env, services, volumes)
- **Reverse proxy** — hostname-based routing to app containers
- **dew-apps tarball builds** — pre-packaged tarballs (10x smaller than Docker images)

### `dew vm` — explicit multi-VM management

Today Dew supports two implicit VM models depending on the command:
`dew up` boots an ephemeral per-project VM; `dew app run` reuses a
long-lived "default" VM for all registry apps. Both behaviours stay,
but v0.5 promotes the distinction to a first-class concept.

- **`dew vm list`** — show all known VMs (name, profile, state, resource usage)
- **`dew vm create <name> [--profile]`** — create a named, long-lived VM
- **`dew vm start/stop <name>`** — lifecycle control independent of app commands
- **`dew vm destroy <name>`** — clean up disk image and config
- **`dew vm shell <name>`** — interactive shell into a specific VM
- **`dew app run <app> --vm <name>`** — opt into running an app inside a specific VM (default remains the shared one)
- **`dew up --vm <name>`** — attach a dev environment to a named VM instead of spawning a new one

This gives users an explicit choice between **shared** (fast subsequent
starts, one kernel, container-level isolation — the default for `dew app
run`) and **isolated** (one VM per workload, VM-level isolation,
deployable as a unit — the default for `dew up`) without forcing either
model.

## v0.6 — Dashboard + Observability + npm Repackaging

- **Dashboard** — built-in web UI (Go templates + htmx)
- **`dew app list`** — resource usage (CPU, RAM, network)
- **Service provisioning** — postgres, redis as containers via `dew.toml`
- **Resource limits** — per-app CPU and memory via cgroups
- **`dew clone <url>`** — git clone + detect + up
- **Cross-VM service discovery** — DNS-based reachability between named VMs (`my-api.vm.dew.local`), so isolated VMs can still talk when configured
- **VM-level resource limits** — declarative CPU/RAM caps per VM in `dew.toml`

### npm packaging refactor — `optionalDependencies` per platform

Today the npm package is a 5 KB metadata shell that downloads the
correct binary in a `postinstall` script. This works but has cost us
twice already: once when codesign-failure-paths silently broke VM
support, and once at v0.5.0 when the postinstall re-signed a properly
notarized binary with ad-hoc and stripped the Developer ID. Move to
the pattern used by every modern Rust/Go-binary-on-npm tool:

```
@solcreek/dew                      (main package, ~25 lines, no postinstall)
  bin/dew                          (Node dispatcher → require.resolve + spawnSync)
  optionalDependencies:
    "@solcreek/dew-darwin-arm64": "x.y.z"
    "@solcreek/dew-darwin-x64":   "x.y.z"
    "@solcreek/dew-linux-x64":    "x.y.z"
    "@solcreek/dew-linux-arm64":  "x.y.z"
    "@solcreek/dew-windows-x64":  "x.y.z"

@solcreek/dew-darwin-arm64         (signed + notarized binary inside)
  package.json:  os=darwin, cpu=arm64, no scripts
  bin/dew                          (chmod 755'd at build time in CI)
```

Adopting this fixes the signing-strip class of bugs by removing the
postinstall surface entirely — there is no code path on the user's
machine that touches the binary. npm 7+ ships `optionalDependencies`
by default, so the typical install is fast and offline-resilient
after the first run.

Reference patterns to mine for code:
- **Biome's `bin/biome` dispatcher** — the cleanest 25-line CLI
  shape, exactly the model we want (CLI not a library)
- **esbuild's `pkgForSomeOtherPlatform()` error** — best-in-class
  diagnostic when wrong-arch binary is detected (Docker → host arch
  mismatch, Rosetta cross-platform copy, etc.)
- **rolldown's `NAPI_RS_ENFORCE_VERSION_CHECK`** — one-line lockfile-
  skew defense for when the binding package version drifts from the
  dispatcher version
- **biome's `generate-packages.mjs`** — CI step that scaffolds each
  `@solcreek/dew-<triple>` directory from the built binary, including
  the critical `chmod 0755` at *publish* time (not at install time)

Notarization detail worth remembering: **npm tarball extraction strips
macOS extended attributes**, including the `com.apple.quarantine` xattr
and any externally-attached notarization tickets. For bare Mach-O
binaries, the notarization staple is embedded **inside the binary**
via `xcrun stapler staple`, so it survives the round-trip. The CI
already does the right thing here (notarize then staple); the refactor
just preserves that property by keeping the binary file untouched in
the platform package.

Single env override: `DEW_BINARY=/path/to/custom/dew` for local builds
and testing. No SHA-256 ceremony — npm provenance attestations (already
enabled) handle supply-chain trust without the per-binary hash table
that esbuild maintains for its npm-install-fallback path.

## v0.7 — Agent + Ecosystem

- **MCP server** — Dew as tool provider for AI agents
- **Go / Rust / Deno detection**
- **`dew build --workspace`** — monorepo support
- **`dew share` stable subdomain** — Creek Cloud relay
- **Grove desktop app** — Tauri GUI for non-developers
- **Official website** — dewvm.dev
