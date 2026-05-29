#!/usr/bin/env bash
# codesign.sh — sign a macOS binary with Developer ID, or fall back to ad-hoc.
#
# Inputs (env):
#   APPLE_DEVELOPER_ID  — full identity string (e.g. "Developer ID Application: Foo Inc. (TEAMID)")
#                          When unset/empty, falls back to ad-hoc signing.
#   ENTITLEMENTS        — path to entitlements plist (default: ./entitlements.plist)
#
# Usage:
#   codesign.sh <binary> [<binary> ...]

set -euo pipefail

ENTITLEMENTS="${ENTITLEMENTS:-./entitlements.plist}"
DEV_ID="${APPLE_DEVELOPER_ID:-}"

if [ ! -f "$ENTITLEMENTS" ]; then
  echo "codesign.sh: $ENTITLEMENTS not found" >&2
  exit 1
fi

for BIN in "$@"; do
  if [ ! -f "$BIN" ]; then
    echo "codesign.sh: $BIN not found, skipping"
    continue
  fi

  if [ -n "$DEV_ID" ]; then
    echo "codesign.sh: signing $BIN with Developer ID..."
    codesign --options runtime \
             --entitlements "$ENTITLEMENTS" \
             --sign "$DEV_ID" \
             --timestamp \
             --force \
             "$BIN"
    # Verify chain is correct
    codesign -dv --verbose=4 "$BIN" 2>&1 | grep -E "Authority|TeamIdentifier|Runtime"
  else
    echo "codesign.sh: APPLE_DEVELOPER_ID not set, falling back to ad-hoc signing of $BIN"
    codesign --entitlements "$ENTITLEMENTS" --force -s - "$BIN"
  fi
done
