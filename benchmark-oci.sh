#!/bin/bash
set -uo pipefail

# Benchmark: Variant C (host-pull + crun overlay, no containerd) vs the current
# containerd/nerdctl baseline. Both run a single container in a fresh VM and
# exit — the realistic "ephemeral dew VM" pattern.
#
# Usage: ./benchmark-oci.sh [IMAGE] [CMD...]
#   ./benchmark-oci.sh docker.io/library/alpine:3.21 cat /etc/os-release
#   ./benchmark-oci.sh docker.io/library/redis:7-alpine redis-server --version

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEW="$SCRIPT_DIR/dew"
OCIPOC="${OCIPOC:-/tmp/dew-ocipoc}"
CRUN="${CRUN:-/tmp/crun-arm64}"
STAGE="${STAGE:-/tmp/oci-poc}"
RUNS="${RUNS:-3}"

IMAGE="${1:-docker.io/library/alpine:3.21}"
shift || true
CMD="${*:-cat /etc/os-release}"

ms() { python3 -c "import time; print(int(time.time()*1000))"; }

echo "=== Variant C vs containerd baseline ==="
echo "Date:  $(date -u)"
echo "Image: $IMAGE"
echo "Cmd:   $CMD"
echo "Runs:  $RUNS"
echo ""

# --- Sizes ---
echo "--- Sizes (runtime footprint shipped in the guest) ---"
NODE_SZ=$(ls -lh "$SCRIPT_DIR/initramfs/initramfs-node.cpio.gz" 2>/dev/null | awk '{print $5}')
STD_SZ=$(ls -lh "$SCRIPT_DIR/initramfs/initramfs-standard.cpio.gz" 2>/dev/null | awk '{print $5}')
CRUN_SZ=$(ls -lh "$CRUN" 2>/dev/null | awk '{print $5}')
echo "  node initramfs (Variant C base):       $NODE_SZ"
echo "  crun (Variant C runtime):              $CRUN_SZ"
echo "  standard initramfs (containerd stack):  $STD_SZ"
echo ""

# --- Variant C: host stage cost, cold vs warm cache ---
echo "--- Variant C: host-side stage (cold vs warm content-addressed cache) ---"
CACHE="${CACHE:-$HOME/Library/Caches/dew/oci}"
report_stage() { # $1=label  reads /tmp/ocipoc.json, $2=wall_ms
    python3 -c "import json;d=json.load(open('/tmp/ocipoc.json'));print(f\"  $1: wall=$2ms cache={'HIT' if d['cached'] else 'miss'} pull={d['pull_ms']}ms flatten={d['flatten_ms']}ms rootfs={d['rootfs_bytes']/1e6:.1f}MB\")"
}
# cold: wipe just this image's cache entry by clearing the whole cache root
rm -rf "$CACHE"
rm -rf "$STAGE" && mkdir -p "$STAGE"
T1=$(ms); "$OCIPOC" -stage "$STAGE" -crun "$CRUN" -platform linux/arm64 -cmd "$CMD" -json "$IMAGE" > /tmp/ocipoc.json; T2=$(ms)
report_stage "cold " $((T2-T1))
# warm (tag): cache HIT, but still one manifest HEAD to resolve tag->digest
rm -rf "$STAGE" && mkdir -p "$STAGE"
T1=$(ms); "$OCIPOC" -stage "$STAGE" -crun "$CRUN" -platform linux/arm64 -cmd "$CMD" -json "$IMAGE" > /tmp/ocipoc.json; T2=$(ms)
report_stage "warm(tag)   " $((T2-T1))
# warm (digest-pinned): cache HIT with NO network at all — local hardlink + spec gen
DIGEST=$(python3 -c "import json;print(json.load(open('/tmp/ocipoc.json'))['digest'])")
REPO="${IMAGE%:*}"
rm -rf "$STAGE" && mkdir -p "$STAGE"
T1=$(ms); "$OCIPOC" -stage "$STAGE" -crun "$CRUN" -platform linux/arm64 -cmd "$CMD" -json "$REPO@$DIGEST" > /tmp/ocipoc.json; T2=$(ms)
report_stage "warm(digest)" $((T2-T1))
echo "  (warm-tag pays one manifest HEAD to resolve the tag; pinning by digest needs no network)"
echo ""

# --- Variant C: per-run VM boot + assemble + crun ---
echo "--- Variant C: cold VM boot -> overlay assemble -> crun run ---"
rm -f /tmp/dew-poc-c.img
for i in $(seq 1 $RUNS); do
    T1=$(ms)
    OUT=$("$DEW" run --profile node --disk /tmp/dew-poc-c.img --share oci:"$STAGE":ro --timeout 120s -- "sh /oci/run.sh" 2>&1)
    T2=$(ms)
    BOOT=$(echo "$OUT" | grep -o 'VM running ([0-9]*ms)' | grep -o '[0-9]*' || echo "?")
    EXT=$(echo "$OUT"  | grep -o 'extract_ms=[0-9]*'   | cut -d= -f2 || echo "?")
    OVL=$(echo "$OUT"  | grep -o 'overlay_ms=[0-9]*'   | cut -d= -f2 || echo "?")
    CR=$(echo "$OUT"   | grep -o 'crun_run_ms=[0-9]*'  | cut -d= -f2 || echo "?")
    RC=$(echo "$OUT"   | grep -o 'exit=[0-9-]*'        | cut -d= -f2 || echo "?")
    echo "  Run $i: wall=$((T2-T1))ms boot=${BOOT}ms extract=${EXT}ms overlay=${OVL}ms crun=${CR}ms exit=${RC}"
done
echo ""

# --- Baseline: containerd/nerdctl on the standard profile ---
# Image is loaded from the host-staged docker-archive over virtiofs (offline,
# no guest egress) so this is a fair warm-image run: boot + containerd start +
# nerdctl load + nerdctl run, all in one fresh VM.
echo "--- Baseline: cold VM boot (+containerd) -> nerdctl load -> nerdctl run ---"
# Build the docker-archive once (benchmark-only; Variant C never needs it).
"$OCIPOC" -stage "$STAGE" -crun "$CRUN" -platform linux/arm64 -cmd "$CMD" -archive "$IMAGE" >/dev/null
BASE_SH='set -e; nerdctl load -i /oci/image.tar >/dev/null 2>&1; nerdctl run --rm --net=host '"$IMAGE $CMD"
for i in $(seq 1 $RUNS); do
    T1=$(ms)
    OUT=$("$DEW" run --profile standard --share oci:"$STAGE":ro --timeout 180s -- "$BASE_SH" 2>&1)
    T2=$(ms)
    BOOT=$(echo "$OUT" | grep -o 'VM running ([0-9]*ms)' | grep -o '[0-9]*' || echo "?")
    LASTLINE=$(echo "$OUT" | grep -v '^dew:' | tail -1)
    echo "  Run $i: wall=$((T2-T1))ms boot=${BOOT}ms  [$LASTLINE]"
done
echo ""
echo "=== Done ==="
echo "Note: Variant C total-to-container = host(pull+flatten, one-time/cached) + wall."
echo "      Baseline wall includes containerd startup each boot + nerdctl warm run."
