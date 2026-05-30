#!/usr/bin/env sh
# dew installer.
#
#   curl -fsSL https://dewvm.dev/install.sh | sh
#
# Detects OS + arch, fetches the latest GitHub release tarball,
# verifies its SHA256 against checksums.txt, optionally verifies a
# cosign keyless signature on checksums.txt, and drops the dew binary
# into a sensible bin directory.
#
# Env overrides:
#   DEW_VERSION       pin a specific tag (default: latest)
#   DEW_PREFIX        install dir (default: /usr/local/bin if root,
#                     else $HOME/.local/bin)
#   DEW_VERIFY_COSIGN "1" → hard-require cosign keyless signature on
#                     checksums.txt against the expected signer
#                     identity (release.yml in this repo). Defaults
#                     to soft-attempt: verifies if cosign is on PATH
#                     AND .sig/.pem assets exist on the release.

set -eu

REPO="solcreek/dew"
VERSION="${DEW_VERSION:-}"
PREFIX="${DEW_PREFIX:-}"
VERIFY_COSIGN="${DEW_VERIFY_COSIGN:-}"

# OIDC subject identity for cosign verification. Pinned so an attacker
# cannot substitute a different workflow and have their signature pass.
# Dots in literal positions are escaped; tag suffix is semver-shaped so
# branch refs can never satisfy the matcher.
COSIGN_IDENTITY_REGEX="^https://github\.com/${REPO}/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+.*\$"
COSIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"

log() { printf '%s\n' "$*" >&2; }
err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || err "$1 is required but not installed"
}

need curl
need tar
need uname
need sha256sum 2>/dev/null || need shasum

# --- detect OS / arch -------------------------------------------------

uname_s="$(uname -s)"
case "$uname_s" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *)      err "unsupported OS: $uname_s (dew supports linux + darwin)" ;;
esac

uname_m="$(uname -m)"
case "$uname_m" in
    x86_64|amd64)   arch=amd64 ;;
    arm64|aarch64)  arch=arm64 ;;
    *)              err "unsupported arch: $uname_m (need amd64 or arm64)" ;;
esac

# --- resolve version --------------------------------------------------

if [ -z "$VERSION" ]; then
    log "==> resolving latest release tag"
    final_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/${REPO}/releases/latest")"
    VERSION="${final_url##*/}"
    case "$VERSION" in
        v*) : ;;
        *)  err "could not parse latest release tag from $final_url" ;;
    esac
fi
log "==> installing dew $VERSION ($os/$arch)"

# --- pick install dir -------------------------------------------------

if [ -z "$PREFIX" ]; then
    if [ "$(id -u)" -eq 0 ]; then
        PREFIX=/usr/local/bin
    else
        PREFIX="$HOME/.local/bin"
        mkdir -p "$PREFIX"
        case ":$PATH:" in
            *":$PREFIX:"*) : ;;
            *) log "note: $PREFIX is not on PATH — add it to your shell rc" ;;
        esac
    fi
fi
[ -d "$PREFIX" ] || err "install dir $PREFIX does not exist"
[ -w "$PREFIX" ] || err "install dir $PREFIX is not writable"

# --- download tarball + checksums -------------------------------------

# goreleaser archive name_template: dew_<version-without-v>_<os>_<arch>.tar.gz
ver_nov="${VERSION#v}"
tar_name="dew_${ver_nov}_${os}_${arch}.tar.gz"
tar_url="https://github.com/${REPO}/releases/download/${VERSION}/${tar_name}"
sha_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

log "==> downloading $tar_name"
curl -fsSL -o "$tmp/$tar_name" "$tar_url" \
    || err "download failed: $tar_url"

log "==> verifying SHA256"
curl -fsSL -o "$tmp/checksums.txt" "$sha_url" \
    || err "checksum download failed: $sha_url"

# Pick whichever sha256 tool is available
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/$tar_name" | awk '{print $1}')
else
    actual=$(shasum -a 256 "$tmp/$tar_name" | awk '{print $1}')
fi
expected=$(grep "  ${tar_name}\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || err "checksums.txt has no entry for $tar_name"
[ "$actual" = "$expected" ] || err "checksum mismatch
    expected: $expected
    actual:   $actual"

# --- optional cosign verification -------------------------------------

cosign_ok=0
if command -v cosign >/dev/null 2>&1; then cosign_ok=1; fi

if [ "$VERIFY_COSIGN" = "1" ] || [ $cosign_ok -eq 1 ]; then
    sig_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.sig"
    pem_url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt.pem"

    if [ $cosign_ok -eq 0 ]; then
        [ "$VERIFY_COSIGN" = "1" ] && err "DEW_VERIFY_COSIGN=1 but cosign is not on PATH"
    else
        log "==> verifying cosign signature"
        if curl -fsSL -o "$tmp/checksums.txt.sig" "$sig_url" 2>/dev/null \
           && curl -fsSL -o "$tmp/checksums.txt.pem" "$pem_url" 2>/dev/null; then
            if cosign verify-blob \
                --certificate "$tmp/checksums.txt.pem" \
                --signature   "$tmp/checksums.txt.sig" \
                --certificate-identity-regexp "$COSIGN_IDENTITY_REGEX" \
                --certificate-oidc-issuer     "$COSIGN_OIDC_ISSUER" \
                "$tmp/checksums.txt" >/dev/null 2>&1; then
                log "    cosign: OK"
            else
                [ "$VERIFY_COSIGN" = "1" ] && err "cosign verification failed"
                log "    cosign: WARN (sig present but verification failed)"
            fi
        else
            [ "$VERIFY_COSIGN" = "1" ] && err "cosign signature assets not found at release"
            log "    cosign: skipped (signature assets absent — older release?)"
        fi
    fi
fi

# --- extract + install ------------------------------------------------

log "==> extracting"
tar -xzf "$tmp/$tar_name" -C "$tmp"

[ -f "$tmp/dew" ] || err "extracted tarball missing dew binary"
chmod 0755 "$tmp/dew"

log "==> installing to $PREFIX/dew"
mv "$tmp/dew" "$PREFIX/dew"

# --- post-install hint ------------------------------------------------

log ""
log "installed: $PREFIX/dew"
log "run: dew --version"
if [ "$os" = "darwin" ]; then
    log ""
    log "note: dew uses Apple Virtualization.framework. The release"
    log "binary is signed with Developer ID + virtualization entitlement;"
    log "Gatekeeper will not prompt. If you see VZErrorDomain Code=1,"
    log "run \`dew doctor\` to diagnose."
fi
