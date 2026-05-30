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
| `HOMEBREW_TAP_GITHUB_TOKEN` | PAT or GitHub App token with write access to `solcreek/homebrew-tap` | brew formula PR |

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

### 2. Provision the brew-tap write token

**Option A (simpler, less secure)** — fine-grained PAT:

1. Go to https://github.com/settings/personal-access-tokens
2. Create a fine-grained PAT, expiry 1 year
3. Repository access: `solcreek/homebrew-tap` only
4. Repository permissions: **Contents: Read and write**, **Pull requests: Read and write**
5. Add as `HOMEBREW_TAP_GITHUB_TOKEN` secret on `solcreek/dew` repo:
   ```bash
   gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo solcreek/dew
   # paste the PAT
   ```

**Option B (preferred long-term)** — GitHub App:

1. Create a GitHub App owned by `solcreek` org with permissions:
   - Repository: Contents (write), Pull requests (write)
   - Subscribe to: nothing
2. Install the App only on `solcreek/homebrew-tap`
3. Generate a private key, base64 it
4. Use `actions/create-github-app-token` in the release workflow to mint a
   short-lived token. Add `APP_ID` and `APP_PRIVATE_KEY` secrets.
5. Wire it up in `release.yml`:
   ```yaml
   - uses: actions/create-github-app-token@v2
     id: app-token
     with:
       app-id: ${{ secrets.APP_ID }}
       private-key: ${{ secrets.APP_PRIVATE_KEY }}
       repositories: homebrew-tap
   - uses: goreleaser/goreleaser-action@v6
     env:
       HOMEBREW_TAP_GITHUB_TOKEN: ${{ steps.app-token.outputs.token }}
   ```

### 3. Configure npm OIDC trusted publisher

For `@solcreek/dew` (already exists on npm):

1. Go to https://www.npmjs.com/package/@solcreek/dew → **Settings** → **Trusted publishers**
2. Add a new Trusted Publisher:
   - Repository: `solcreek/dew`
   - Workflow filename: `release.yml`
   - Environment: leave blank

No `NPM_TOKEN` needed once this is configured. node 24 in the workflow gives us npm 11.x, which supports OIDC.

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
