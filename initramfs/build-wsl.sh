#!/bin/bash
set -euo pipefail

# Build a WSL2 rootfs tarball for Dew on Windows.
# Output: dew-rootfs-x86_64.tar.gz
#
# This is a complete Alpine Linux rootfs with:
# - dew-native binary (Linux dew CLI, no VM needed)
# - dew-agent (exec protocol)
# - containerd + nerdctl + runc + CNI
# - Node.js, npm, build-base, python3, pip
#
# Usage: bash initramfs/build-wsl.sh

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="${SCRIPT_DIR}/wsl-work"
OUT="${SCRIPT_DIR}/dew-rootfs-x86_64.tar.gz"
CACHE="${SCRIPT_DIR}/cache"

ALPINE_VERSION="3.21"
ALPINE_MINOR="3.21.3"

echo "=== Building Dew WSL2 rootfs ==="

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR" "$CACHE"

# --- Alpine minirootfs ---
echo "--- Alpine minirootfs ---"
TARBALL="${CACHE}/alpine-minirootfs-${ALPINE_MINOR}-x86_64.tar.gz"
if [ ! -f "$TARBALL" ]; then
    curl -fSL -o "$TARBALL" "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/x86_64/alpine-minirootfs-${ALPINE_MINOR}-x86_64.tar.gz"
fi
tar xzf "$TARBALL" -C "$WORK_DIR"

# --- Configure apk repos ---
mkdir -p "$WORK_DIR/etc/apk"
echo "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main" > "$WORK_DIR/etc/apk/repositories"
echo "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/community" >> "$WORK_DIR/etc/apk/repositories"

# --- Build dew-native (Linux CLI, runs directly, no VM) ---
echo "--- Build dew-native ---"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o "${WORK_DIR}/usr/local/bin/dew-native" \
    "${REPO_DIR}/cmd/dew-agent/"
echo "dew-native: $(ls -lh "${WORK_DIR}/usr/local/bin/dew-native" | awk '{print $5}')"

# --- Init script for WSL2 ---
cat > "${WORK_DIR}/etc/profile.d/dew.sh" << 'DEWSH'
export PATH="/usr/local/bin:$PATH"
alias dew='dew-native'
DEWSH

# --- DNS ---
echo "nameserver 1.1.1.1" > "$WORK_DIR/etc/resolv.conf"

# --- Pack ---
echo "--- Packing rootfs ---"
(cd "$WORK_DIR" && tar czf "$OUT" .)
echo "Output: $(ls -lh "$OUT" | awk '{print $5}') $OUT"

rm -rf "$WORK_DIR"
echo "=== Done ==="
