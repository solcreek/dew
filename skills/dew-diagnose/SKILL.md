---
name: dew-diagnose
description: |
  Debug `dew up` / `dew run` VM boot failures or `dew deploy` failures using `dew doctor`,
  `DEW_DEBUG=1`, and the captured server-side stderr. Use when: dew operations fail with
  errors like `VZErrorDomain Code=1`, `exit status 2`, "VM start failed", or any time the
  user reports dew isn't working. Do NOT use for: provisioning new servers (see
  dew-server-create), routine deploys (see dew-deploy), or generic Linux / kernel debugging
  unrelated to dew.
metadata:
  author: solcreek
  version: "1.0.0"
---

# dew diagnose — turn an opaque failure into a clear cause

dew has three diagnostic layers. Start with the cheapest (doctor) and
escalate to dump output only if needed.

## When to use this

- User reports `dew up`, `dew run`, or `dew deploy` failing
- Error message mentions `VZErrorDomain` (macOS Virtualization framework)
- Error message is opaque: `exit status 1`, `exit status 2`, "VM start failed"
- User pastes `dew --json` output with `"ok": false`
- User says "it worked yesterday, today it doesn't"

## When NOT to use this

- User wants help installing dew — that's a different problem (point at
  install.sh URLs in dewvm.dev)
- User asks generic kernel / Linux questions unrelated to dew's VM
- User is debugging their app's logic — dew's diagnostics don't help with
  application-level bugs (their app's logs are in `dew logs`)

## Layer 1: `dew doctor --verbose`

Runs all environment checks (macOS version, codesign integrity,
entitlement, asset presence, kernel format) and attempts a boot smoke
test with the full VM config dump enabled.

```
dew doctor --verbose
```

What to look for:

| Check | If it fails | Likely fix |
|---|---|---|
| `macOS version` | macOS < 13 | Upgrade macOS |
| `Codesign signature integrity` | binary damaged in transit | `npm install -g dew@latest` |
| `Virtualization entitlement present` | binary lost entitlement | Re-install |
| `Ad-hoc signature with restricted entitlement` (warn) | local-built binary | Use the official install |
| `Asset: vmlinuz` / `initramfs.cpio.gz` | missing | `dew assets pull` |
| `Kernel format` (arm64 only, since 0.7.32) | EFI/PE without ARM64 boot header — stale 9 MB EFI-stub kernel from older install | **`dew assets pull --force`** |
| `VM boot test` | actual boot failure | Read the dumped VM config + error chain printed under it |

## Layer 2: `DEW_DEBUG=1` for a single invocation

Skips doctor's environment checks, runs the user's actual command, and
prints the VM config + host model + macOS version before `machine.Start()`.

```
DEW_DEBUG=1 dew up
```

Look at the `kernel: ... format=` line:
- `raw ARM64 Linux Image` → kernel is fine; failure is elsewhere
- `EFI/PE without ARM64 boot header` → stale asset; `dew assets pull --force`
- `gzip / zstd / ELF` → release pipeline shipped a wrong format; file a bug

Look at the `host: Mac1X,Y arch=arm64 macOS X.Y.Z` line:
- Mac model is in the failure error too (since 0.7.31) — include verbatim
  in any bug report

## Layer 3: deploy-side failures (server-side stderr surfaced since 0.7.35)

If `dew deploy` fails at `extract` with `exit status 2`, the server-side
tar / hook stderr is now included in the SSE failure event:

```
✗ deploy failed at extract: exit status 2
  ── extract stderr ──
  tar: app/CLAUDE.md: Cannot create symlink to '': No such file or directory
  ────────────────
```

Read that stderr block — it's the actual cause. Common patterns:

| stderr message | cause |
|---|---|
| `Cannot create symlink to ''` | symlink in source with empty target — pre-0.7.34 tar bug, upgrade dew |
| `No space left on device` | disk full on the server |
| `Permission denied` | app dir permissions; check `dew apps` and the path on server |
| `Decompression error` | corrupt tarball; rebuild + redeploy |

## Common pitfalls

- **Suggesting "wipe ~/.local/share/dew and reinstall"** — too aggressive.
  Most failures resolve with `dew assets pull --force` (kernel) or
  `npm install -g dew@latest` (binary).
- **Asking the user for the full stack trace** — dew failures rarely have
  one. The verbose dump and the SSE stderr ARE the diagnostics.
- **Ignoring the host model line** — if Mac is `Mac16,*` (M4) or newer,
  flag it explicitly in any bug report; we have less field data on those.
