#!/usr/bin/env bash
# notarize.sh — submit signed macOS binaries to Apple's notarization service.
# Skips silently when notarization credentials aren't configured.
#
# Inputs (env):
#   APPLE_ID            — Apple ID email
#   APPLE_TEAM_ID       — 10-character Team ID
#   APPLE_APP_PASSWORD  — app-specific password from account.apple.com
#
# Usage:
#   notarize.sh <binary> [<binary> ...]
#
# Note: bare binaries (not .app/.dmg/.pkg) cannot be stapled; the
# notarization ticket lives in Apple's servers. Gatekeeper will check
# online for quarantined downloads (browsers); curl/npm downloads
# bypass Gatekeeper entirely, so this primarily proves the chain.

set -euo pipefail

# Skip if credentials aren't provided (forks, dev branches).
if [ -z "${APPLE_ID:-}" ] || [ -z "${APPLE_TEAM_ID:-}" ] || [ -z "${APPLE_APP_PASSWORD:-}" ]; then
  echo "notarize.sh: notarization credentials not set, skipping"
  exit 0
fi

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
        --apple-id "$APPLE_ID" \
        --team-id "$APPLE_TEAM_ID" \
        --password "$APPLE_APP_PASSWORD" \
        --wait

  rm -f "$ZIP"
done
