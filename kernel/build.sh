#!/bin/bash
set -euo pipefail

# Build the Dew turbo kernel via Docker (reproducible).
# Output: kernel/vmlinuz-turbo
#
# Usage:
#   ./kernel/build.sh
#   ./kernel/build.sh --push   # build + push to ghcr.io

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_TAG="ghcr.io/solcreek/dew-kernel:turbo"
OUT="${SCRIPT_DIR}/vmlinuz-turbo"

echo "=== Building Dew turbo kernel ==="

docker build -t "$IMAGE_TAG" "$SCRIPT_DIR"

# Extract vmlinuz from the scratch image
CONTAINER_ID=$(docker create "$IMAGE_TAG")
docker cp "${CONTAINER_ID}:/vmlinuz-turbo" "$OUT"
docker rm "$CONTAINER_ID" >/dev/null

echo ""
echo "Output: $(ls -lh "$OUT")"
echo "SHA256: $(shasum -a 256 "$OUT" | awk '{print $1}')"

if [ "${1:-}" = "--push" ]; then
    echo ""
    echo "Pushing $IMAGE_TAG"
    docker push "$IMAGE_TAG"
fi

echo ""
echo "=== Done ==="
