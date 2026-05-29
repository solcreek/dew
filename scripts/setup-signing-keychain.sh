#!/usr/bin/env bash
# setup-signing-keychain.sh — create a temporary keychain on a CI runner,
# import the Developer ID .p12, and add the Developer ID intermediate CA.
# Idempotent: writes a cleanup hint to $GITHUB_ENV so a later step can
# delete the keychain.
#
# Inputs (env):
#   APPLE_CERT_P12_BASE64    — base64-encoded .p12 export of (cert + key)
#   APPLE_CERT_P12_PASSWORD  — password used when exporting the .p12
#
# Side effects:
#   Creates $RUNNER_TEMP/build.keychain and adds it to the user search list.
#   Sets KEYCHAIN_PATH in $GITHUB_ENV for downstream cleanup.

set -euo pipefail

if [ -z "${APPLE_CERT_P12_BASE64:-}" ] || [ -z "${APPLE_CERT_P12_PASSWORD:-}" ]; then
  echo "setup-signing-keychain.sh: certificate secrets not set, skipping"
  exit 0
fi

KEYCHAIN_PATH="${RUNNER_TEMP:-/tmp}/build.keychain"
KEYCHAIN_PWD="ci-keychain-${GITHUB_RUN_ID:-local}-$$"
CERT_PATH="${RUNNER_TEMP:-/tmp}/dew-signing.p12"

# Create + unlock temp keychain
security create-keychain -p "$KEYCHAIN_PWD" "$KEYCHAIN_PATH"
security set-keychain-settings -lut 7200 "$KEYCHAIN_PATH"   # auto-lock after 2h
security unlock-keychain -p "$KEYCHAIN_PWD" "$KEYCHAIN_PATH"

# Prepend to user search list (preserving existing entries)
EXISTING=$(security list-keychains -d user | sed 's/[[:space:]]*"\(.*\)"[[:space:]]*/\1/')
security list-keychains -d user -s "$KEYCHAIN_PATH" $EXISTING

# Import .p12 (cert + private key)
echo "$APPLE_CERT_P12_BASE64" | base64 --decode > "$CERT_PATH"
security import "$CERT_PATH" \
                -k "$KEYCHAIN_PATH" \
                -P "$APPLE_CERT_P12_PASSWORD" \
                -T /usr/bin/codesign \
                -T /usr/bin/security
rm -f "$CERT_PATH"

# Download + import Developer ID intermediate CAs (both G1 + G2) into the
# same keychain so the chain validates without depending on the runner's
# pre-installed trust store.
curl -fsSL -o "${RUNNER_TEMP:-/tmp}/DeveloperIDCA.cer" \
     https://www.apple.com/certificateauthority/DeveloperIDCA.cer
curl -fsSL -o "${RUNNER_TEMP:-/tmp}/DeveloperIDG2CA.cer" \
     https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
security import "${RUNNER_TEMP:-/tmp}/DeveloperIDCA.cer"   -k "$KEYCHAIN_PATH" -T /usr/bin/codesign || true
security import "${RUNNER_TEMP:-/tmp}/DeveloperIDG2CA.cer" -k "$KEYCHAIN_PATH" -T /usr/bin/codesign || true

# Grant codesign access without UI prompt
security set-key-partition-list \
         -S apple-tool:,apple:,codesign: \
         -s -k "$KEYCHAIN_PWD" \
         "$KEYCHAIN_PATH" >/dev/null 2>&1

# Verify identity is usable
echo "setup-signing-keychain.sh: identities found:"
security find-identity -v -p codesigning "$KEYCHAIN_PATH"

# Persist path for cleanup step
if [ -n "${GITHUB_ENV:-}" ]; then
  echo "KEYCHAIN_PATH=$KEYCHAIN_PATH" >> "$GITHUB_ENV"
fi
