# Releasing dew

`v*` tag push triggers `.github/workflows/release.yml`, which produces signed
darwin binaries + cross-compiled linux binaries + initramfs/kernel/agent
auxiliary assets, attaches them to a GitHub Release, signs the checksum file
with cosign keyless, attaches SLSA L3 provenance, opens a PR against the
Homebrew tap, and publishes the npm dispatcher via OIDC trusted publisher.

## Required GitHub repository secrets

| Secret | Purpose | Where it lives |
|---|---|---|
| `APPLE_CERT_P12_BASE64` | Developer ID Application cert (`.p12`), base64 | macOS signing |
| `APPLE_CERT_P12_PASSWORD` | password for the `.p12` | macOS signing |
| `APPLE_DEVELOPER_ID` | full identity string e.g. `Developer ID Application: Kaik, Inc. (74ZKT4P4QB)` | macOS signing |
| `APPLE_ID` | Apple account email | notarytool |
| `APPLE_TEAM_ID` | Apple Team ID (`74ZKT4P4QB`) | notarytool |
| `APPLE_APP_PASSWORD` | app-specific password from appleid.apple.com | notarytool |
| `HOMEBREW_TAP_APP_ID` | GitHub App ID for `solcreek-tap-publisher` | brew formula PR (token mint) |
| `HOMEBREW_TAP_APP_PRIVATE_KEY` | GitHub App private key (`.pem` contents) | brew formula PR (token mint) |
| `GORELEASER_KEY` | goreleaser-pro license key (Startup tier) | enables `builder: prebuilt` for signed+notarized darwin binaries |

`GITHUB_TOKEN` is provided automatically; no action needed.

npm publish uses **OIDC trusted publisher** (no `NPM_TOKEN` required).

## One-time setup (do these before the first v0.7.0 tag)

### 1. Create the Homebrew tap repo

```bash
# Create empty repo
gh repo create solcreek/homebrew-tap --public \
  --description "Homebrew tap for SolCreek tools (dew, creekd, future CLIs + Tauri casks)"

# Initial commit so the default branch exists for goreleaser to PR against
git clone https://github.com/solcreek/homebrew-tap.git
cd homebrew-tap
mkdir -p Formula Casks
cat > README.md <<'EOF'
# solcreek/homebrew-tap

Official Homebrew tap for SolCreek tools.

## Install

```bash
brew install solcreek/tap/dew
```

## Available formulas

- `dew` — Ultra-lightweight VM + deploy tool

## Casks (coming soon)

- `marina` — Dev container GUI
- `grove` — Desktop app installer
EOF
git add README.md Formula/.gitkeep Casks/.gitkeep
git commit -m "Initial commit"
git push
```

### 2. Provision the brew-tap write token (GitHub App)

We use an org-owned GitHub App to mint short-lived (~1h) write tokens
scoped to `solcreek/homebrew-tap`. No PAT, no user-binding, no long-lived
credentials.

1. Go to https://github.com/organizations/solcreek/settings/apps →
   **New GitHub App**
2. Configure:
   - Name: `solcreek-tap-publisher`
   - Homepage URL: `https://github.com/solcreek`
   - **Webhook → Active: uncheck**
   - Permissions → Repository:
     - **Contents: Read and write**
     - **Pull requests: Read and write**
     - Metadata: Read (auto-included)
   - Where can this be installed: **Only on this account**
3. Create the App. Note the **App ID**. Generate + download a private key
   (`.pem`).
4. Left sidebar → **Install App** → `solcreek` org → **Only select
   repositories: `solcreek/homebrew-tap`** → Install.
5. Set org secrets (scoped to `solcreek/dew` only):
   ```bash
   echo "<app-id>" | gh secret set HOMEBREW_TAP_APP_ID \
     --org solcreek --visibility selected --repos solcreek/dew

   gh secret set HOMEBREW_TAP_APP_PRIVATE_KEY \
     --org solcreek --visibility selected --repos solcreek/dew \
     < /path/to/solcreek-tap-publisher.private-key.pem
   ```
6. Delete the local `.pem` file once secrets are set. App private key is
   recoverable only by generating a new one — there's no need to keep the
   downloaded `.pem` once it's in GitHub secrets.
7. `release.yml` already mints the token via `actions/create-github-app-token@v2`
   and passes it to goreleaser as `HOMEBREW_TAP_GITHUB_TOKEN`.

### 3. Configure npm OIDC trusted publisher

`release.yml` publishes the same dispatcher to two npm names:

- **`dew`** (unscoped) — primary, all docs and install scripts
  point here.
- **`@solcreek/dew`** — mirror, kept alive for back-compat with
  users who installed via the scoped name before we acquired
  the unscoped one. Same bytes, same version.

Each name has its own OIDC trusted-publisher entry on npmjs.com.
Set up BOTH so each publish step succeeds:

1. https://www.npmjs.com/package/dew → **Settings** → **Trusted publishers**
2. https://www.npmjs.com/package/@solcreek/dew → **Settings** → **Trusted publishers**

For each, add:
- Repository: `solcreek/dew`
- Workflow filename: `release.yml`
- Environment: leave blank

No `NPM_TOKEN` needed once both are configured. node 24 in the
workflow gives us npm 11.x, which supports OIDC.

If only one of the two trusted-publisher entries is configured,
the primary publish still succeeds; the mirror step surfaces a
warning (not a failure) so the release isn't blocked. The mirror
catches up on the next release once OIDC is set up.

### 4. Configure tag protection on `solcreek/dew`

Prevents anyone with push access from tagging a malicious release that
goreleaser will faithfully sign + ship.

1. Repository settings → **Rules** → **Rulesets** → **New ruleset**
2. Name: "Signed release tags"
3. Targets: tag pattern `v*`
4. Rules:
   - **Require signed commits** (so the tag itself is GPG/SSH signed)
   - **Restrict creations** to maintainers only (allowed actors list)
5. Apply

Future `git tag v0.7.0` must be signed: `git tag -s v0.7.0 -m "..."`.
Maintainers' SSH/GPG keys must be registered on github.com.

### 5. Host install.sh at dewvm.dev

The `install.sh` in the repo root is the source of truth. To serve it at
https://dewvm.dev/install.sh:

```bash
cd /Users/linyiru/Projects/creek/dew/website
cp ../install.sh public/install.sh
pnpm run build && pnpm exec wrangler deploy
```

Or wire up a CI job in this repo that pushes install.sh to the Astro site
on each tag — TBD.

## Tagging a release

```bash
# Ensure CHANGELOG.md has a section for the new version (above Unreleased)
git tag -s v0.7.0 -m "v0.7.0 — distribution architecture overhaul"
git push origin v0.7.0
```

The release workflow runs ~10 minutes:
1. `build-macos` (~3 min) — signs + notarizes darwin binaries
2. `build-linux` (~5 min) — auxiliary assets
3. `goreleaser` (~2 min) — archives + cosign + brew PR + GH Release
4. `provenance` (~1 min) — SLSA L3 attestation
5. `npm-publish` (~1 min) — dispatcher to npm via OIDC

If npm publish fails, look for `ENEEDAUTH` or `E409` in logs:
- `ENEEDAUTH` → trusted publisher config mismatch
- `E409 / cannot publish over` → version already published (expected on rerun)

## Dry-run (catches config errors on PR)

`.github/workflows/release-dry-run.yml` runs on every PR that touches the
release shape:
- `goreleaser check` — config syntax
- `goreleaser build --snapshot --id dew-linux` — catches build breakage
- `npm test` — dispatcher tests
- `shellcheck install.sh`
