#!/usr/bin/env bash
# notarize.sh — submit signed macOS binaries to Apple notarization.
#
# Uses a keychain-stored credential profile so the app-specific password
# is never visible on the command line (avoiding leakage via `ps`).
# Skips silently when notarization credentials aren't configured.
#
# Inputs (env):
#   APPLE_ID            — Apple ID email
#   APPLE_TEAM_ID       — 10-character Team ID
#   APPLE_APP_PASSWORD  — app-specific password from account.apple.com
#   KEYCHAIN_PATH       — optional; reuses the build keychain from
#                          setup-signing-keychain.sh. Falls back to default.
#
# Usage:
#   notarize.sh <binary> [<binary> ...]
#
# Note: bare binaries (not .app/.dmg/.pkg) cannot be stapled; the
# notarization ticket lives on Apple's servers. Gatekeeper checks online
# for quarantined downloads (browsers). curl/npm downloads bypass
# Gatekeeper entirely, so notarization primarily proves the chain.

set -euo pipefail

# Skip if credentials aren't provided (forks, dev branches).
if [ -z "${APPLE_ID:-}" ] || [ -z "${APPLE_TEAM_ID:-}" ] || [ -z "${APPLE_APP_PASSWORD:-}" ]; then
  echo "notarize.sh: notarization credentials not set, skipping"
  exit 0
fi

PROFILE="dew-notarize-${GITHUB_RUN_ID:-local}"
KEYCHAIN_ARGS=()
if [ -n "${KEYCHAIN_PATH:-}" ] && [ -f "$KEYCHAIN_PATH" ]; then
  KEYCHAIN_ARGS=(--keychain "$KEYCHAIN_PATH")
fi

# Store credentials in the keychain so submit/wait/info never see the
# password on the command line. Idempotent — overwrites if profile exists.
echo "notarize.sh: storing credentials in keychain profile '$PROFILE'..."
xcrun notarytool store-credentials "$PROFILE" \
      --apple-id "$APPLE_ID" \
      --team-id "$APPLE_TEAM_ID" \
      --password "$APPLE_APP_PASSWORD" \
      "${KEYCHAIN_ARGS[@]}" >/dev/null

# Make sure the profile is removed even if a submit hangs.
cleanup() {
  xcrun notarytool delete-credentials "$PROFILE" \
        "${KEYCHAIN_ARGS[@]}" 2>/dev/null || true
}
trap cleanup EXIT

for BIN in "$@"; do
  if [ ! -f "$BIN" ]; then
    echo "notarize.sh: $BIN not found, skipping"
    continue
  fi

  echo "notarize.sh: packaging $BIN for submission..."
  ZIP="$BIN.zip"
  rm -f "$ZIP"
  ditto -c -k --keepParent "$BIN" "$ZIP"

  echo "notarize.sh: submitting $BIN (waits for Apple response)..."
  xcrun notarytool submit "$ZIP" \
        --keychain-profile "$PROFILE" \
        "${KEYCHAIN_ARGS[@]}" \
        --wait

  rm -f "$ZIP"
done
