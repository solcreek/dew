---
name: dew-upgrade
description: |
  Upgrade dew and refresh cached VM assets. Use when: user is on an outdated dew version,
  release notes mention a fix the user needs, `dew doctor` flags a stale kernel asset, or
  the user is about to file a bug (verify they're on latest first). Do NOT use for:
  installing dew the first time (use install.sh or `npm install -g dew`), upgrading
  unrelated tools, or as a generic "fix it" response — narrow this to actual version /
  asset staleness.
metadata:
  author: solcreek
  version: "1.0.0"
---

# dew upgrade — keep binary + assets in sync

dew is a binary + VM assets (kernel, initramfs). Both can go stale
independently. This skill covers refreshing each.

## When to use this

- User reports a bug fixed in a newer release (check CHANGELOG vs their
  `dew --version` output)
- `dew doctor` shows `Kernel format` failing with stale-EFI-stub hint
- About to file a bug — always verify latest version reproduces first
- New release ships a security fix or notable feature

## When NOT to use this

- User is installing dew for the first time — see install.sh URL on
  dewvm.dev, or `npm install -g dew`
- "Just upgrade and try again" as a blind first response — diagnose first
  via dew-diagnose, then upgrade only if version is the cause
- User is asking how to upgrade an unrelated tool

## Two independent upgrade surfaces

### 1. Upgrade the dew binary

Pick ONE based on how the user installed:

```
# npm dispatcher (most common)
npm install -g dew@latest

# Or use dew's own self-update (replaces cached native binary)
dew update
```

The npm path updates the dispatcher; next `dew` invocation downloads the
matching native binary from GitHub Releases. `dew update` updates the
native binary in place but doesn't touch the npm dispatcher.

### 2. Refresh VM assets (kernel + initramfs)

Since 0.7.33, assets are content-addressed
(`~/.local/share/dew/vmlinuz-aarch64.<sha8>`) so a binary upgrade auto-
downloads the matching assets on next `dew up`. Stale legacy files are
left in place but ignored.

To force a refresh manually (e.g. doctor flagged a stale kernel):

```
dew assets pull --force
```

`--force` is required since 0.7.32 to bypass the existence cache. Without
it, `dew assets pull` skips files that already exist regardless of
correctness.

## Steps

1. Show current version:
   ```
   dew --version
   ```
2. Compare against latest release: https://github.com/solcreek/dew/releases/latest
3. Upgrade binary:
   ```
   npm install -g dew@latest
   ```
4. Verify upgrade applied:
   ```
   dew --version
   ```
5. If the user is on arm64 and was hitting boot failures, force-refresh
   assets to be safe:
   ```
   dew assets pull --force
   ```
6. Re-run the originally failing command. If still failing, escalate to
   dew-diagnose.

## Verifying via `dew doctor` after upgrade

```
dew doctor
```

Should report `Codesign signature integrity ✓` and `Kernel format: raw
ARM64 Linux Image` on arm64. If `Kernel format` still fails after upgrade,
run `dew assets pull --force` to refresh the on-disk file.

## Common pitfalls

- **`dew update` from a different shell session** — the running shell
  may have the old `dew` cached in PATH lookup; new shell or `hash -r`
  to pick up the new binary.
- **`npm install -g dew@latest` permission error** — user installed via
  sudo originally; suggest fixing npm prefix (`npm config set prefix ~/.local`)
  rather than running sudo npm install.
- **Asking the user to "delete `~/.local/share/dew` and re-install"** —
  too destructive. Content-addressed storage (since 0.7.33) makes the
  legacy files harmless; just force-refresh the kernel if doctor flags it.
- **Confusing the dispatcher version (npm `dew` package) with the binary
  version** — they should match on a fresh install. If `npm view dew@latest
  version` differs from `dew --version`, the dispatcher hasn't run yet
  (first run downloads the matching binary).
