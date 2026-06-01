# Roadmap

`dew` is sandboxed Linux compute — agent-native and human-friendly.
This page is what's planned next. Order is rough priority, not a
schedule. For shipped work, see [CHANGELOG.md](./CHANGELOG.md).

## Near-term

### `dew shell`

A generic Linux environment that doesn't require a detected project
in the current directory. `dew up` stays project-driven; `dew shell`
becomes the entry point for "I just want a shell" cases.

Real interactive shell — TTY-allocating, with signal forwarding and
terminal resize — not a pipe wrapper around the existing one-shot
exec path. Sits in the Advanced section of `dew --help` rather than
the lead, to keep the headline aligned with the agent-first surface.

### `dew.toml` as a first-class project descriptor

Auto-detection from `package.json` / `requirements.txt` works for
common cases but doesn't survive across machines, doesn't pin
runtime versions, and doesn't compose multiple runtimes in one
project. `dew.toml` will be the canonical project descriptor;
auto-detection remains the no-config fallback. `dew up --init`
materializes a starting `dew.toml` for the detected runtime.

### Cold first-run under 30s

Today a fresh install plus `dew up` on a Node project takes about a
minute. The target is under 30 seconds. Hot subsequent runs already
complete in under 5 seconds.

### Hostname-aware egress allowlist

`--network-policy restricted` already drops outbound traffic by
default and accepts only loopback, DNS, and IPs added through
`--allow-host`. The current form is IP/CIDR only, which is brittle
for CDN-backed package registries. Planned: a DNS-aware proxy in the
guest that filters by hostname (e.g. `--allow-host registry.npmjs.org`
keeps working even as the upstream CDN rotates IPs). After that, the
default mode flips from open to restricted.

## Mid-term

### Linux dev parity

The macOS path is the most polished. Linux users get a `dew` binary
but lifecycle and integration are thinner. Parity means picking the
right local runtime and making `dew up` / `dew exec` behave the same
on both hosts.

### Larger app catalog

`dew app run` ships a small curated set today. The path to a broader
catalog: stable manifest spec, contribution guide, automated update
flow against the registry.

### Auto-update polish

Smoother defaults for `dew update`: silent background check, opt-in
channels (stable / nightly / pre-release), per-project version
pinning via `dew.toml`.

### Restore previous version after deploy (rollback)

`dew deploy` writes the new version and starts it; the receiver
keeps no history of the previous one. Rollback needs the receiver
to retain the last N versions on disk, expose them via a list
endpoint, and switch the active version atomically without a
re-deploy. Today there is no `dew rollback` — the command exists
but returns "not yet implemented" with a workaround pointing back at
`dew deploy <prior-tarball>`.

## Considering

Items whose scope depends on decisions still in progress.

- **Command-shape cleanup.** Some current verbs overlap or are
  ambiguous; a deprecation cycle may consolidate them.
- **Headline rewrite.** The current tagline conflates several
  distinct use cases; clearer positioning is a documentation task
  more than a code one.

## Out of scope

These are deliberately *not* `dew`'s direction:

- **Hosted / managed sandboxes.** Sub-second cold-start ephemeral
  compute is a different architecture and a different product.
- **General container engine.** `dew` runs containers for
  `dew app run`, but is not a general workflow replacement.
- **Kubernetes / orchestration.** No cluster, no CRDs, no operators.
