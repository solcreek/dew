#!/usr/bin/env bash
# conformance.sh — run the guest-agent conformance suite (test/conformance)
# against one booted image, on a Mac whose arch matches the image.
#
# Usage:
#   scripts/conformance.sh [--agent BIN] [--disk-gb N] KERNEL INITRD [go test flags...]
#
#   KERNEL, INITRD  the image to boot, e.g. from
#                   `DEW_AGENT=rust TARGET_ARCH=aarch64 bash initramfs/build.sh minimal`
#                   (Linux) or the cached release assets in ~/.local/share/dew.
#   --agent BIN     bake BIN in as /usr/local/bin/dew-agent: appended as a cpio
#                   overlay, whose entries replace the image's. For trying a
#                   local agent build on macOS, where build.sh can't run.
#   --disk-gb N     attach a fresh N GB disk, as dew does for the node, python
#                   and standard profiles (they switch_root onto it).
#
# Extra flags go to the test binary, e.g. -test.run 'TestConfine'.
# Env: DEW_CONFORMANCE_CPUS (2), DEW_CONFORMANCE_MEMORY_MB (1024),
#      DEW_CONFORMANCE_INTERLEAVE_REPS (25), DEW_CONFORMANCE_BOOT_S (60).
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
AGENT_BIN="" DISK_GB=0
while [ $# -gt 0 ]; do
  case "$1" in
    --agent)   AGENT_BIN="$2"; shift 2 ;;
    --disk-gb) DISK_GB="$2"; shift 2 ;;
    -h|--help) sed -n '2,19p' "$0"; exit 0 ;;
    *) break ;;
  esac
done
if [ $# -lt 2 ]; then
  sed -n '2,19p' "$0" >&2
  exit 2
fi
KERNEL="$1" INITRD="$2"
shift 2
for f in "$KERNEL" "$INITRD" ${AGENT_BIN:+"$AGENT_BIN"}; do
  [ -f "$f" ] || { echo "conformance.sh: $f not found" >&2; exit 1; }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/dew-conformance.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

if [ -n "$AGENT_BIN" ]; then
  mkdir -p "$WORK/overlay/usr/local/bin"
  install -m 0755 "$AGENT_BIN" "$WORK/overlay/usr/local/bin/dew-agent"
  (cd "$WORK/overlay" && find usr | cpio -o -H newc -R 0:0 2>/dev/null | gzip -9) > "$WORK/overlay.cpio.gz"
  cat "$INITRD" "$WORK/overlay.cpio.gz" > "$WORK/initrd.cpio.gz"
  INITRD="$WORK/initrd.cpio.gz"
fi

# The Virtualization framework refuses an unsigned process: build the test
# binary, then sign it ad hoc with the repo's entitlement.
(cd "$REPO" && go test -c -o "$WORK/conformance.test" ./test/conformance/)
codesign --entitlements "$REPO/entitlements.plist" --force -s - "$WORK/conformance.test" 2>/dev/null

DEW_CONFORMANCE_KERNEL="$KERNEL" DEW_CONFORMANCE_INITRD="$INITRD" DEW_CONFORMANCE_DISK_GB="$DISK_GB" \
  "$WORK/conformance.test" -test.v "$@"
