#!/bin/bash
set -euo pipefail

# Build Dew initramfs.
#
# Usage:
#   ./build.sh              # standard profile (default)
#   ./build.sh minimal      # exec-only, no container runtime
#   ./build.sh node          # Node.js + npm + build-base (React/Vite ready)
#   ./build.sh standard     # node + crun (larger RAM/disk tier)

PROFILE="${1:-standard}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="${SCRIPT_DIR}/work"
OUT_INITRD="${SCRIPT_DIR}/initramfs-${PROFILE}.cpio.gz"
OUT_KERNEL="${SCRIPT_DIR}/vmlinuz"
CACHE="${SCRIPT_DIR}/cache"

ALPINE_VERSION="3.21"
ALPINE_MINOR="3.21.3"

# Two distinct architectures:
#   HOST_ARCH   - the machine running this script (determines which
#                 apk-tools-static binary we need to execute, and what
#                 the Go toolchain runs natively)
#   TARGET_ARCH - the arch the initramfs is being built FOR; the kernel
#                 APK, package install via `apk --arch`, and Go cross-
#                 compile target all key off this.
# Setting TARGET_ARCH=aarch64 on an x86_64 Linux runner cross-builds the
# aarch64 image (apk-tools-static --arch handles cross-arch package
# install since --no-scripts skips guest-arch hooks; Go cross-compiles
# trivially). This lets the Linux release runner produce both arches
# with full bake support, instead of leaving aarch64 to the macOS
# runner where apk-tools-static doesn't run.
HOST_ARCH="$(uname -m)"
TARGET_ARCH="${TARGET_ARCH:-$HOST_ARCH}"
case "$TARGET_ARCH" in
    x86_64|amd64)    ALPINE_ARCH="x86_64"; GO_ARCH="amd64" ;;
    aarch64|arm64)   ALPINE_ARCH="aarch64"; GO_ARCH="arm64" ;;
    *) echo "unsupported TARGET_ARCH: $TARGET_ARCH"; exit 1 ;;
esac
case "$HOST_ARCH" in
    x86_64|amd64)    HOST_ALPINE_ARCH="x86_64" ;;
    aarch64|arm64)   HOST_ALPINE_ARCH="aarch64" ;;
    *) echo "unsupported HOST_ARCH: $HOST_ARCH"; exit 1 ;;
esac

ROOTFS_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/${ALPINE_ARCH}/alpine-minirootfs-${ALPINE_MINOR}-${ALPINE_ARCH}.tar.gz"

# Alpine yanks old patch versions from main as new ones land, and the
# arches aren't synchronised — observed 2026-06-05: aarch64 had only
# 6.12.92 while x86_64 still had only 6.12.91. Per-arch pin lets the
# build land regardless of which side is ahead. The real fix is to
# parse APKINDEX for "current available" rather than pinning, tracked
# as a follow-up. Bump these in lockstep with a quick CDN probe before
# tagging:
#   for arch in x86_64 aarch64; do
#     curl -sIfo /dev/null \
#       "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/$arch/linux-virt-<VER>.apk" \
#       && echo "$arch: ok"
#   done
case "$ALPINE_ARCH" in
    x86_64)  KERNEL_PKG_VER="6.12.93-r0"; KERNEL_VER="6.12.93-0-virt" ;;
    aarch64) KERNEL_PKG_VER="6.12.93-r0"; KERNEL_VER="6.12.93-0-virt" ;;
esac
KERNEL_APK_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/linux-virt-${KERNEL_PKG_VER}.apk"

# crun: the lightweight OCI runtime used by dew's host-pull + overlay path
# (the dew-oci-run launcher). Static binary, runs on musl. Baked into every
# disk-bearing profile (node/python/standard).
CRUN_VERSION="1.20"

echo "=== Building Dew initramfs (${PROFILE}) ==="
echo "Arch: ${ALPINE_ARCH}"
echo ""

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR" "$CACHE"

# --- Step 1: Alpine minirootfs ---
echo "--- Step 1: Alpine minirootfs ---"
TARBALL="${CACHE}/alpine-minirootfs-${ALPINE_MINOR}-${ALPINE_ARCH}.tar.gz"
if [ ! -f "$TARBALL" ]; then
    curl -fSL -o "$TARBALL" "$ROOTFS_URL"
else
    echo "Using cached tarball"
fi
tar xzf "$TARBALL" -C "$WORK_DIR"
echo "Rootfs: $(du -sh "$WORK_DIR" | awk '{print $1}')"

# --- Step 2: Kernel + modules ---
echo "--- Step 2: Alpine virt kernel + modules ---"
KERNEL_APK="${CACHE}/linux-virt-${ALPINE_ARCH}.apk"
if [ ! -f "$KERNEL_APK" ]; then
    curl -fSL -o "$KERNEL_APK" "$KERNEL_APK_URL"
else
    echo "Using cached kernel APK"
fi
APK_EXTRACT="${CACHE}/apk-extract"
rm -rf "$APK_EXTRACT"
mkdir -p "$APK_EXTRACT"
tar xzf "$KERNEL_APK" -C "$APK_EXTRACT" 2>/dev/null

cp "$APK_EXTRACT/boot/vmlinuz-virt" "$OUT_KERNEL"

# Alpine 3.21's linux-virt aarch64 kernel ships in EFI zboot format
# (PE32+ wrapper around a gzip-compressed payload). Apple VZ on Apple
# Silicon rejects this with VZErrorDomain Code=1 "Internal Virtualization
# error" — it requires a raw ARM64 Linux Image. Detect and extract.
# x86_64 bzImage also starts with "MZ" but has no zimg marker, so the
# check is safe across both arches.
if [ "$(head -c2 "$OUT_KERNEL")" = "MZ" ] && \
   [ "$(dd if="$OUT_KERNEL" bs=1 skip=4 count=4 2>/dev/null)" = "zimg" ]; then
    echo "  Detected EFI zboot wrapper; extracting raw Image payload..."
    python3 - "$OUT_KERNEL" <<'PY'
import struct, gzip, sys, pathlib
p = pathlib.Path(sys.argv[1])
data = p.read_bytes()
if data[:2] != b'MZ' or data[4:8] != b'zimg':
    sys.exit("not zboot")
# zboot header: MZ(2) padding(2) 'zimg'(4) payload_offset(4 LE) payload_size(4 LE)
off, size = struct.unpack('<II', data[8:16])
payload = data[off:off+size]
if payload[:2] == b'\x1f\x8b':
    payload = gzip.decompress(payload)
# Raw ARM64 Image must have 'ARM\x64' magic at offset 0x38.
if payload[0x38:0x3c] != b'ARM\x64':
    sys.exit(f"extracted payload missing ARM64 magic: got {payload[0x38:0x3c].hex()}")
p.write_bytes(payload)
print(f"  -> {len(payload)} bytes raw ARM64 Image")
PY
fi
echo "Kernel: $(ls -lh "$OUT_KERNEL" | awk '{print $5}')"

# Kernel modules. Alpine ships ~900 .ko.gz files (~72MB decompressed)
# even though init/init-stage2 only modprobe ~18 of them. Copy the lot
# from the apk, then prune to the allowlist + its transitive dependency
# closure (resolved from modules.dep). Net effect: minimal/node initramfs
# drops from ~30MB to ~10MB compressed.
mkdir -p "$WORK_DIR/lib/modules"
cp -a "$APK_EXTRACT/lib/modules/${KERNEL_VER}" "$WORK_DIR/lib/modules/"

# Allowlist: everything init/init-stage2 modprobes.
# Base set is loaded by every profile (init.sh tolerates missing modules
# with `|| true`, so non-disk profiles like `minimal` simply skip the
# disk-related ones at boot). iptables modules are needed by every
# profile because `--network-policy=restricted` applies to all of them;
# without explicit modprobes the iptables nft backend silently fails
# with "Protocol not supported" and the policy is a no-op.
KMODS_BASE="af_packet virtio_net overlay mbcache jbd2 ext4 virtio_blk \
            vsock vmw_vsock_virtio_transport fuse virtiofs \
            nf_tables nft_compat ip_tables iptable_filter \
            crc32c_generic virtio-rng binfmt_misc"
# All profiles use the same module set now: crun runs containers with
# --net=host (no CNI bridge), so the standard profile no longer needs the
# bridge/veth/masquerade modules the old containerd path required.
KMODS_KEEP="$KMODS_BASE"

MODS_ROOT="$WORK_DIR/lib/modules/${KERNEL_VER}"
DEP_FILE="$MODS_ROOT/modules.dep"

# Walk modules.dep to build the transitive closure of the allowlist.
# modules.dep lines look like:
#   kernel/path/foo.ko.gz: kernel/lib/bar.ko.gz kernel/lib/baz.ko.gz
# Index it once into name → "path|deps" so the BFS is O(N+M) instead of
# launching grep per visited module.
prune_modules() {
    local keep_seed="$1"
    # Pipefail makes "grep | head" fail when grep matches nothing, which
    # corrupts variable assignments inside a set -e script. Scope a
    # relaxation to this function only.
    set +o pipefail

    # Index: name<TAB>path<TAB>deps_csv (one per line, no .ko/.ko.gz suffix on names)
    local index
    index=$(awk -F: '
        {
            path = $1
            n = split(path, parts, "/")
            base = parts[n]
            sub(/\.ko(\.gz)?$/, "", base)
            deps = $2
            gsub(/^ +| +$/, "", deps)
            # Strip extensions and dirs from each dep so we get bare names.
            split(deps, dlist, " ")
            depcsv = ""
            for (i = 1; i <= length(dlist); i++) {
                d = dlist[i]
                if (d == "") continue
                m = split(d, dparts, "/")
                dn = dparts[m]
                sub(/\.ko(\.gz)?$/, "", dn)
                depcsv = depcsv (depcsv == "" ? "" : ",") dn
            }
            print base "\t" path "\t" depcsv
        }
    ' "$DEP_FILE")

    # BFS using two-token tracking strings.
    local queue=" $keep_seed " keep=" " mod line path deps dep
    while [ -n "${queue// /}" ]; do
        # pop first non-empty token
        queue="${queue# }"
        mod="${queue%% *}"
        queue="${queue#"$mod"}"
        case "$keep" in *" $mod "*) continue ;; esac
        keep="$keep$mod "
        line=$(printf '%s\n' "$index" | awk -F'\t' -v m="$mod" '$1 == m { print; exit }')
        [ -z "$line" ] && continue
        deps="${line##*$'\t'}"
        for dep in ${deps//,/ }; do
            case "$keep$queue " in
                *" $dep "*) ;;
                *) queue="$queue $dep" ;;
            esac
        done
    done

    # Collect kept paths.
    local paths=" "
    for mod in $keep; do
        path=$(printf '%s\n' "$index" | awk -F'\t' -v m="$mod" '$1 == m { print $2; exit }')
        [ -n "$path" ] && paths="$paths$path "
    done

    # Delete every .ko/.ko.gz NOT in the keepset.
    local f rel
    find "$MODS_ROOT/kernel" -name '*.ko*' -type f | while read -r f; do
        rel="${f#$MODS_ROOT/}"
        case "$paths" in
            *" $rel "*) ;;
            *) rm -f "$f" ;;
        esac
    done
    find "$MODS_ROOT/kernel" -type d -empty -delete 2>/dev/null || true

    set -o pipefail
    echo "  Kept $(echo "$keep" | wc -w | xargs) modules (allowlist + deps)"
}

prune_modules "$KMODS_KEEP"

# Decompress survivors (Apple VZ insmod can't read .gz). Boot-time
# `depmod -a` regenerates modules.dep against the new .ko names.
find "$MODS_ROOT" -name "*.ko.gz" -exec gunzip {} \;
echo "Modules: $(du -sh "$MODS_ROOT" 2>/dev/null | awk '{print $1}')"

# --- Step 3: Build dew-agent ---
echo "--- Step 3: Build dew-agent ---"
mkdir -p "${WORK_DIR}/usr/local/bin"
GOOS=linux GOARCH="$GO_ARCH" CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o "${WORK_DIR}/usr/local/bin/dew-agent" \
    "${REPO_DIR}/cmd/dew-agent/"
echo "Agent: $(ls -lh "${WORK_DIR}/usr/local/bin/dew-agent" | awk '{print $5}')"

# --- Step 4a: Runtime pre-install (node / python / standard profiles) ---
# On Linux build hosts we use apk-tools-static to install the runtime
# directly into the initramfs rootfs at build time — moves the ~40-50s
# of first-boot `apk add` cost from every user's first VM start into our
# CI step that runs once per release. On non-Linux hosts (dev macOS),
# fall back to the older marker pattern: init-stage2 sees the marker on
# first boot and runs apk add over the network. End result for users
# of release artifacts is identical; local dev builds are a bit slower
# on first boot.

prepare_apk_static() {
    # apk-tools-static is a native binary, so it must match the HOST
    # arch — not the build target. With --arch=TARGET on the install
    # command, apk happily cross-installs guest-arch packages.
    local apk_static_bin="${CACHE}/apk.static-${HOST_ALPINE_ARCH}"
    if [ -x "$apk_static_bin" ]; then
        echo "$apk_static_bin"
        return 0
    fi
    local listing_url="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${HOST_ALPINE_ARCH}/"
    local apk_name
    apk_name=$(curl -sL "$listing_url" | grep -o 'apk-tools-static-[0-9][^"]*\.apk' | head -1)
    if [ -z "$apk_name" ]; then
        echo "  WARN: could not find apk-tools-static for ${ALPINE_ARCH}" >&2
        return 1
    fi
    local apk_tar="${CACHE}/${apk_name}"
    [ -f "$apk_tar" ] || curl -fSL -o "$apk_tar" "${listing_url}${apk_name}"
    local extract_dir="${CACHE}/apk-tools-static-extract"
    rm -rf "$extract_dir"
    mkdir -p "$extract_dir"
    tar xzf "$apk_tar" -C "$extract_dir" 2>/dev/null
    [ -x "${extract_dir}/sbin/apk.static" ] || {
        echo "  WARN: apk-tools-static.apk did not contain sbin/apk.static" >&2
        return 1
    }
    mv "${extract_dir}/sbin/apk.static" "$apk_static_bin"
    rm -rf "$extract_dir"
    chmod +x "$apk_static_bin"
    echo "$apk_static_bin"
}

ensure_apk_repos() {
    mkdir -p "$WORK_DIR/etc/apk"
    cat > "$WORK_DIR/etc/apk/repositories" <<REPO_EOF
https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main
https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/community
REPO_EOF
}

apk_install_pkgs() {
    local apk_static_bin="$1"; shift
    "$apk_static_bin" --root "$WORK_DIR" --arch "$ALPINE_ARCH" \
        --no-progress --no-network 2>/dev/null \
        --initdb add ca-certificates-bundle 2>/dev/null || true
    # --no-scripts skips per-package post-install/trigger hooks. They need
    # CAP_SYS_CHROOT, which the unprivileged GitHub-hosted runner can't
    # grant; on Linux CI the install would otherwise exit non-zero even
    # though every package's file content is correctly extracted. We
    # don't depend on those hooks (no busybox applet relinking, no CA
    # bundle rebuild — Alpine ships both pre-baked in the rootfs we
    # extracted in Step 1), so skipping them is safe.
    "$apk_static_bin" --root "$WORK_DIR" --arch "$ALPINE_ARCH" \
        --no-progress --allow-untrusted --update-cache --no-scripts \
        add "$@"
}

# Python profile — pre-install python3 + pip on Linux. Marker file is
# always set so init-stage2 can still install build-base on first boot
# if a user needs it (matches the pre-Opt-A user-facing behaviour).
if [ "$PROFILE" = "python" ]; then
    touch "${WORK_DIR}/.dew-python-profile"
    HOST_OS=$(uname -s)
    if [ "$HOST_OS" = "Linux" ] && APK_STATIC=$(prepare_apk_static); then
        echo "--- Step 4a: Python 3 + pip (baked) ---"
        ensure_apk_repos
        apk_install_pkgs "$APK_STATIC" python3 py3-pip
        if [ -x "$WORK_DIR/usr/bin/python3" ]; then
            echo "  Python: $($WORK_DIR/usr/bin/python3 --version 2>/dev/null)"
        fi
        rm -rf "$WORK_DIR/var/cache/apk"/* 2>/dev/null || true
    else
        echo "--- Step 4a: Python marker (first-boot install — build host = $HOST_OS) ---"
    fi
fi

# Node profile (also applied to standard, which builds on node + containers).
if [ "$PROFILE" = "node" ] || [ "$PROFILE" = "standard" ]; then
    touch "${WORK_DIR}/.dew-node-profile"
    HOST_OS=$(uname -s)
    if [ "$HOST_OS" = "Linux" ] && APK_STATIC=$(prepare_apk_static); then
        echo "--- Step 4a: Node.js + npm (baked) ---"
        ensure_apk_repos
        apk_install_pkgs "$APK_STATIC" nodejs npm
        if [ -x "$WORK_DIR/usr/bin/node" ]; then
            echo "  Node: $($WORK_DIR/usr/bin/node --version 2>/dev/null)"
        fi
        rm -rf "$WORK_DIR/var/cache/apk"/* 2>/dev/null || true
    else
        echo "--- Step 4a: Node.js marker (first-boot install — build host = $HOST_OS) ---"
    fi
fi

# e2fsprogs for any profile that uses disk (switch_root needs mkfs.ext4).
# iptables for the egress-policy plumbing (every profile, not just
# standard, because --network-policy=restricted applies to all).
EXTRA_PKGS=""
if [ "$PROFILE" != "minimal" ]; then
    EXTRA_PKGS="$EXTRA_PKGS e2fsprogs e2fsprogs-libs libcom_err libuuid libblkid libeconf"
fi
EXTRA_PKGS="$EXTRA_PKGS iptables libxtables libmnl libnftnl"
echo "Installing$EXTRA_PKGS..."
for pkg in $EXTRA_PKGS; do
    APK_FILE="${CACHE}/${pkg}-${ALPINE_ARCH}.apk"
    if [ ! -f "$APK_FILE" ]; then
        # Alpine ships an unrelated `iptables-0.7.x` placeholder alongside
        # the real `iptables-1.8.x`; require major>=1 so we don't pick
        # the wrong one. Generic packages keep the simple grep.
        case "$pkg" in
            iptables) PATTERN="iptables-[1-9][0-9]*\\.[0-9]+\\.[0-9]+" ;;
            *)        PATTERN="${pkg}-[0-9]" ;;
        esac
        URL=$(curl -sL "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/" \
              | grep -oE "${PATTERN}[^\"]*\\.apk" | head -1)
        [ -n "$URL" ] && curl -fSL -o "$APK_FILE" \
            "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/${URL}" 2>/dev/null || true
    fi
    [ -f "$APK_FILE" ] && tar xzf "$APK_FILE" -C "$WORK_DIR" 2>/dev/null || true
done

# --- Step 4c: crun OCI runtime + dew-oci-run launcher (disk-bearing profiles) ---
# crun is the runtime for dew's host-pull + overlay path: the host pulls/flattens
# an image and stages rootfs.tar + config.json over virtiofs; dew-oci-run
# assembles the overlay and runs it with crun. No containerd/nerdctl/CNI needed.
if [ "$PROFILE" != "minimal" ]; then
    echo "--- Step 4c: crun runtime ---"
    CRUN_BIN="${CACHE}/crun-${CRUN_VERSION}-${GO_ARCH}"
    if [ ! -f "$CRUN_BIN" ]; then
        echo "Downloading crun ${CRUN_VERSION}..."
        curl -fSL -o "$CRUN_BIN" \
            "https://github.com/containers/crun/releases/download/${CRUN_VERSION}/crun-${CRUN_VERSION}-linux-${GO_ARCH}"
    fi
    cp "$CRUN_BIN" "$WORK_DIR/usr/local/bin/crun"
    chmod 755 "$WORK_DIR/usr/local/bin/crun"
    echo "crun: $(ls -lh "$WORK_DIR/usr/local/bin/crun" | awk '{print $5}')"

    # dew-oci-run: given a bundle dir (rootfs.tar + config.json staged by the
    # host over virtiofs) and a name, assemble an overlay rootfs on the ext4
    # disk and run the container with crun. The overlay path must match
    # ocistage.GuestRootPath: /var/lib/dew/oci/<name>/merged. upper/work live on
    # the ext4 root (not tmpfs, which overlay rejects as an upperdir).
    cat > "$WORK_DIR/usr/local/bin/dew-oci-run" << 'OCIRUN_EOF'
#!/bin/sh
set -e
DETACH=0
DATA=""
while [ $# -gt 0 ]; do
    case "$1" in
        --detach) DETACH=1; shift ;;
        --data)
            # Require a value in the documented hostpath:contpath form, rather
            # than blindly reading $2 (a missing value trips `shift 2` under
            # set -e, and a colon-less value would mkdir an unintended path).
            case "${2:-}" in
                ?*:?*) DATA="$2" ;;
                *) echo "dew-oci-run: --data requires hostpath:contpath" >&2; exit 2 ;;
            esac
            shift 2 ;;
        *) break ;;
    esac
done
BUNDLE="$1"; NAME="$2"
if [ -z "$BUNDLE" ] || [ -z "$NAME" ]; then
    echo "usage: dew-oci-run [--detach] [--data hostpath:contpath] <bundle-dir> <name>" >&2
    exit 2
fi
# NAME is interpolated into RUN=/var/lib/dew/oci/$NAME and then `rm -rf`'d, so
# reject anything but a plain container id — no path separators or traversal.
case "$NAME" in
    *[!a-zA-Z0-9_-]*) echo "dew-oci-run: invalid name '$NAME' (allowed: a-z A-Z 0-9 _ -)" >&2; exit 2 ;;
esac

RUN="/var/lib/dew/oci/$NAME"
# Idempotent cleanup: a previous run of the same NAME may have left a crun
# container registered and/or an overlay still mounted at $RUN/merged. Without
# this, rm -rf hits a busy mountpoint and crun run fails with "container already
# exists" — leaving the service unable to restart without manual cleanup.
crun delete --force "$NAME" 2>/dev/null || true
umount "$RUN/merged" 2>/dev/null || true
rm -rf "$RUN"
mkdir -p "$RUN/lower" "$RUN/upper" "$RUN/work" "$RUN/merged" "$RUN/bundle"

# Ensure the persistent data dir (host side of the bind mount) exists so crun
# can bind it; the bind itself is already declared in config.json.
if [ -n "$DATA" ]; then
    mkdir -p "${DATA%%:*}"
fi

# Do not swallow tar's stderr: a truncated/corrupt rootfs must surface a real
# error, not a silently-empty lowerdir and a later opaque crun failure.
tar -xf "$BUNDLE/rootfs.tar" -C "$RUN/lower"
mount -t overlay overlay \
    -o "lowerdir=$RUN/lower,upperdir=$RUN/upper,workdir=$RUN/work" \
    "$RUN/merged"
cp "$BUNDLE/config.json" "$RUN/bundle/config.json"

# If init-stage2 backgrounded the DHCP lease, host.internal may not be in
# /etc/hosts yet (it's appended once the gateway is known). Snapshotting now
# would bake a hosts file without host.internal, and the container would miss
# it permanently. Wait for the bring-up to clear its marker so the snapshot
# below captures host.internal. The marker is absent on a network-less VM and
# clears in well under a second on a healthy lease, so this is a no-op in the
# common case. The ~30s cap only bounds how long a launch blocks on a
# pathologically slow lease instead of stalling toward udhcpc's ~90s window. If
# the cap is hit we proceed without host.internal and warn: the readiness probe
# (a LISTEN-socket check) is independent of DHCP, so a service can still come up
# while silently missing host.internal until the container is restarted.
i=0
while [ -e /run/dew-net-pending ] && [ "$i" -lt 300 ]; do
    sleep 0.1
    i=$((i + 1))
done
if [ -e /run/dew-net-pending ]; then
    echo "dew-oci-run: warning: DHCP lease still pending after ~30s; $NAME launched without host.internal (restart it once the lease lands to pick the alias up)" >&2
fi

# Give the container working name resolution. Many minimal images ship no
# /etc/hosts, so a Go/musl resolver sends `localhost` to DNS ([::1]:53, which
# is refused) instead of resolving it locally — breaking same-VM config like
# REDIS_URL=redis://localhost:6379. Write a Docker-style hosts file (into the
# per-run overlay upper, so the image is untouched) and hand the container the
# guest's resolver so its outbound DNS works too. The host alias lines from the
# guest's own /etc/hosts (host.internal → VZ NAT gateway, host.lo.internal →
# 127.0.0.2 reverse-forward, both set in init-stage2) are appended so a
# container can reach macOS host services the same way the guest can. Grouped
# with stderr silenced and `|| true` so it stays truly best-effort under
# `set -e`: an unwritable overlay must neither abort the launch nor print a
# redirect-open error (which the shell emits before a per-command 2>/dev/null
# would take effect).
{
    mkdir -p "$RUN/merged/etc" &&
    {
        printf '127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n'
        grep -F .internal /etc/hosts 2>/dev/null || true
    } > "$RUN/merged/etc/hosts" &&
    cp /etc/resolv.conf "$RUN/merged/etc/resolv.conf"
} 2>/dev/null || true

if [ "$DETACH" = "1" ]; then
    LOG="/var/log/dew-oci-$NAME.log"
    setsid crun run -b "$RUN/bundle" "$NAME" >"$LOG" 2>&1 &
    # crun run backgrounded with & exits 0 regardless of whether the container
    # actually came up, so confirm it did before reporting success. Without
    # this the host reports a service "started" that is actually dead.
    i=0
    while [ "$i" -lt 20 ]; do
        crun state "$NAME" 2>/dev/null | grep -q '"status": "running"' && break
        i=$((i + 1))
        sleep 0.1
    done
    if crun state "$NAME" 2>/dev/null | grep -q '"status": "running"'; then
        echo "dew-oci-run: $NAME started (detached)"
    else
        echo "dew-oci-run: $NAME failed to start" >&2
        cat "$LOG" >&2 2>/dev/null || true
        # Don't leave the overlay mounted / a half-created container behind, or
        # the next start of this NAME trips over the busy mount.
        crun delete --force "$NAME" 2>/dev/null || true
        umount "$RUN/merged" 2>/dev/null || true
        exit 1
    fi
else
    # Run in the foreground, then flush before returning. The host tears the
    # VM down ungracefully (no guest shutdown), so without this sync a -v
    # volume's writes can still be in the guest page cache and are lost — a
    # short `dew run --image ... -v vol:/data` that writes then exits would
    # silently drop the data. Not exec'd, so the sync runs after crun returns.
    crun run -b "$RUN/bundle" "$NAME"
    rc=$?
    sync
    exit "$rc"
fi
OCIRUN_EOF
    chmod 755 "$WORK_DIR/usr/local/bin/dew-oci-run"
fi

# --- Step 5: Helper scripts ---
echo "--- Step 5: Helpers ---"
cat > "${WORK_DIR}/usr/local/bin/dew-httpd" << 'HTTPD_EOF'
#!/bin/sh
PORT="${1:-8080}"
DIR="${2:-/tmp/www}"
mkdir -p "$DIR"
echo "<h1>Hello from Dew VM</h1><p>$(uname -a)</p>" > "$DIR/index.html"
RESP="HTTP/1.0 200 OK\r\nContent-Type: text/html\r\n\r\n$(cat "$DIR/index.html")"
echo "dew-httpd: listening on :$PORT"
while true; do echo -e "$RESP" | nc -l -p "$PORT" > /dev/null 2>&1; done
HTTPD_EOF
chmod 755 "${WORK_DIR}/usr/local/bin/dew-httpd"

# --- Step 6: Custom init ---
echo "--- Step 6: Init script ---"
cat > "${WORK_DIR}/init" << 'INIT_EOF'
#!/bin/sh
# Dew VM init
export PATH="/usr/local/bin:/usr/local/sbin:$PATH"

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm /run /tmp
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /dev/shm
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /tmp
chmod 1777 /tmp

hostname dew
ip link set lo up

# load kernel modules
# Alpine virt kernel modules are under 6.12.91-0-virt/ but Apple VZ
# reports uname -r as 6.12.91 (no suffix). Symlink so depmod/modprobe
# can find the modules.
KVER=$(uname -r)
if [ -d /lib/modules ] && [ ! -d "/lib/modules/$KVER" ]; then
    for d in /lib/modules/*/; do
        ln -sf "$(basename "$d")" "/lib/modules/$KVER"
        break
    done
fi
depmod -a 2>/dev/null || true
modprobe af_packet 2>/dev/null || true
modprobe virtio_net 2>/dev/null || true
modprobe overlay 2>/dev/null || true
modprobe mbcache 2>/dev/null || true
modprobe jbd2 2>/dev/null || true
modprobe ext4 2>/dev/null || true
modprobe virtio_blk 2>/dev/null || true
# virtio-rng feeds the host's hardware entropy into the guest. Without it an
# idle VM starves /dev/random, and getrandom()-based callers (containerd,
# nerdctl TLS) block for minutes at "crypto/rand: blocked ... waiting to read
# random data from the kernel". Load it early, before switch_root. Lead with
# the on-disk `virtio-rng` name (matches the allowlist/prune); the underscore
# alias is a fallback for modprobe implementations that don't normalise it.
modprobe virtio-rng 2>/dev/null || modprobe virtio_rng 2>/dev/null || true
modprobe vsock 2>/dev/null || true
modprobe vmw_vsock_virtio_transport 2>/dev/null || true

# ── switch_root to ext4 disk (node/standard profiles with --disk) ──
# Provides real filesystem for npm cache, node_modules, containerd.
# Wait for virtio_blk to create /dev/vda — but only when the host actually
# attached a data disk (dew.disk=1 on the kernel cmdline; set in lockstep with
# the block-device attach). A diskless profile (minimal) has no /dev/vda, so
# without this gate the loop runs its full ~1s timeout every boot waiting for a
# device that never appears — and minimal is `dew run`'s default profile.
if grep -q 'dew.disk=1' /proc/cmdline 2>/dev/null; then
    for i in 1 2 3 4 5 6 7 8 9 10; do
        [ -b /dev/vda ] && break
        sleep 0.1
    done
fi

if [ -b /dev/vda ]; then
    mkdir -p /mnt/root
    # Try mount first; format if it fails
    if ! mount /dev/vda /mnt/root 2>/dev/null; then
        echo "dew: formatting disk..."
        mkfs.ext4 -F -L dew /dev/vda
        mount /dev/vda /mnt/root
    fi

    # Populate rootfs from initramfs on first boot
    if [ ! -f /mnt/root/.dew-initialized ]; then
        echo "dew: initializing rootfs on disk (first boot)..."
        cp -a /bin /etc /lib /opt /sbin /usr /var /mnt/root/ 2>/dev/null || true
        cp /init-stage2 /mnt/root/init-stage2
        [ -f /.dew-node-profile ] && cp /.dew-node-profile /mnt/root/.dew-node-profile
        [ -f /.dew-python-profile ] && cp /.dew-python-profile /mnt/root/.dew-python-profile
        chmod 755 /mnt/root/init-stage2
        mkdir -p /mnt/root/dev /mnt/root/proc /mnt/root/sys \
                 /mnt/root/run /mnt/root/tmp /mnt/root/data \
                 /mnt/root/dev/pts /mnt/root/dev/shm
        touch /mnt/root/.dew-initialized
        # Flush the freshly-populated rootfs to the virtual disk NOW.
        # The host tears the VM down without a guest shutdown, so
        # anything still in the guest page cache is lost — observed as
        # zero-length /bin/busybox and ld-musl on the second boot,
        # which kernel-panics switch_root with "Exec format error".
        sync
    fi

    # Always refresh init-stage2 and markers from initramfs
    cp /init-stage2 /mnt/root/init-stage2
    chmod 755 /mnt/root/init-stage2
    [ -f /.dew-node-profile ] && cp /.dew-node-profile /mnt/root/.dew-node-profile
    [ -f /.dew-python-profile ] && cp /.dew-python-profile /mnt/root/.dew-python-profile
    sync

    # Refresh dew's shipped binaries every boot — DEW_REFRESH_LOCALBIN
    # crun, dew-oci-run, dew-agent and dew-httpd live in /usr/local/bin in the
    # initramfs, but the disk rootfs is only populated on FIRST boot (above).
    # Without this, upgrading the initramfs to add or replace one of them (as
    # happened when the crun OCI path first shipped) never reaches an
    # already-initialized disk: `dew run --image` and `dew up --with` then fail
    # with a bare "exit -1" because crun/dew-oci-run aren't on the disk. Same
    # class of bug the kernel-module refresh below guards against. No rm before
    # the copy: shipped binaries are refreshed in place (a same-named guest file
    # is overwritten by the shipped one, which is the intent) while extra files
    # the guest added under /usr/local/bin are left untouched.
    if [ -d /usr/local/bin ]; then
        mkdir -p /mnt/root/usr/local/bin
        cp -a /usr/local/bin/. /mnt/root/usr/local/bin/ 2>/dev/null || true
    fi
    sync

    # Refresh kernel modules every boot — DEW_REFRESH_KMODULES
    # Modules must match the running kernel. When the initramfs ships an
    # updated kernel (dew upgrades), previously-cached modules on the
    # persistent disk become invalid, every `modprobe` silently fails, and
    # the only symptom is mysterious downstream errors like CNI bridge
    # "operation not supported".
    if [ -d /lib/modules ]; then
        mkdir -p /mnt/root/lib/modules
        rm -rf /mnt/root/lib/modules/* 2>/dev/null
        cp -a /lib/modules/. /mnt/root/lib/modules/ 2>/dev/null || true
    fi

    # Bind-mount virtual filesystems into new root
    mount --move /dev /mnt/root/dev
    mount --move /proc /mnt/root/proc
    mount --move /sys /mnt/root/sys
    mount --move /run /mnt/root/run
    mount -t tmpfs tmpfs /mnt/root/tmp
    chmod 1777 /mnt/root/tmp

    # switch_root replaces initramfs with the disk rootfs
    exec switch_root /mnt/root /init-stage2
fi

# ── minimal profile: stay on tmpfs (no containerd) ──

# persistent disk as /data (if present but no containerd)
if [ -b /dev/vda ]; then
    mkdir -p /data
    if ! blkid /dev/vda >/dev/null 2>&1; then
        mkfs.ext4 -q /dev/vda 2>/dev/null || true
    fi
    mount /dev/vda /data 2>/dev/null || true
fi

exec /init-stage2
INIT_EOF
chmod 755 "${WORK_DIR}/init"

# Stage 2 init — runs on both tmpfs (minimal) and ext4 (standard)
cat > "${WORK_DIR}/init-stage2" << 'INIT2_EOF'
#!/bin/sh
export PATH="/usr/local/bin:/usr/local/sbin:$PATH"

# Re-mount essentials if not already mounted (after switch_root)
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys || mount -t sysfs sysfs /sys
mountpoint -q /dev || mount -t devtmpfs devtmpfs /dev
mountpoint -q /dev/pts || { mkdir -p /dev/pts; mount -t devpts devpts /dev/pts; }

# /etc/hosts: localhost and host.lo.internal resolve immediately — neither
# needs the network, and the reverse host-forward (host.lo.internal → 127.0.0.2,
# the vsock tunnel dew-agent serves for `dew up --expose-host`) must work even
# before DHCP. 127.0.0.2 (not .1) so the forwarder never shadows a container's
# own localhost services. The host.internal NAT-gateway line is appended later
# by bring_up_network, once a default route exists. dew-oci-run copies both
# .internal lines into each container's hosts file.
{
    printf '127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n'
    printf '127.0.0.2\thost.lo.internal\n'
} > /etc/hosts

# Egress policy is parsed up front because it decides whether the network
# bring-up may be backgrounded (see the dispatch below). dew.netpolicy=restricted
# means default-DROP OUTPUT plus loopback/DNS/dew.allow= accepts; unset means
# egress is open (opt-in for v1; default-deny + hostname allowlist is a follow-up
# needing a DNS-aware proxy).
NETPOLICY=""
ALLOW_IPS=""
for param in $(cat /proc/cmdline 2>/dev/null); do
    case "$param" in
        dew.netpolicy=*) NETPOLICY="${param#dew.netpolicy=}" ;;
        dew.allow=*)     ALLOW_IPS="${param#dew.allow=}" ;;
    esac
done

# apply_netpolicy enforces dew.netpolicy=restricted; a no-op otherwise.
apply_netpolicy() {
    [ "$NETPOLICY" = "restricted" ] && command -v iptables >/dev/null 2>&1 || return 0
    # Load the netfilter modules iptables needs. The nft backend is Alpine's
    # default; ip_tables/iptable_filter cover the legacy backend older user
    # scripts may use directly.
    modprobe nf_tables 2>/dev/null || true
    modprobe nft_compat 2>/dev/null || true
    modprobe ip_tables 2>/dev/null || true
    modprobe iptable_filter 2>/dev/null || true
    iptables -P INPUT   ACCEPT 2>/dev/null
    iptables -P FORWARD DROP   2>/dev/null
    iptables -P OUTPUT  DROP   2>/dev/null
    iptables -F OUTPUT          2>/dev/null
    iptables -A OUTPUT -o lo -j ACCEPT
    # DNS (UDP 53, TCP 53) — needed so name resolution still works.
    iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
    iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT
    # Explicitly allowed IPs.
    if [ -n "$ALLOW_IPS" ]; then
        OLD_IFS="$IFS"; IFS=','
        for ip in $ALLOW_IPS; do
            iptables -A OUTPUT -d "$ip" -j ACCEPT 2>/dev/null
        done
        IFS="$OLD_IFS"
    fi
    echo "dew: network policy = restricted (default DROP; ${ALLOW_IPS:-no extra hosts})"
}

# bring_up_network does the slow, network-coupled boot work: bring eth0 up, get a
# DHCP lease, publish host.internal → the gateway, then enforce the egress
# policy. Apple VZ's NAT is racy at first boot — usually answers in ~2 s,
# occasionally not for ~60-80 s; `-n -t 30 -T 3` makes ONE patient ~90 s attempt
# then proceeds pass-or-fail, instead of the busybox default of retrying the
# 3-packet discovery burst forever (the "broadcasting discover" spam).
bring_up_network() {
    if ip link show eth0 >/dev/null 2>&1; then
        ip link set eth0 up 2>/dev/null || true
        for i in 1 2 3 4 5 6 7 8 9 10; do
            [ "$(cat /sys/class/net/eth0/carrier 2>/dev/null)" = "1" ] && break
            sleep 0.1
        done
        udhcpc -i eth0 -s /usr/share/udhcpc/default.script -q -n -t 30 -T 3 || true
        echo "nameserver 1.1.1.1" > /etc/resolv.conf
        # Apple VZ's NAT gateway (the DHCP router — typically 192.168.64.1, but
        # VZ may renumber across reboots) IS the host, so a host service bound to
        # 0.0.0.0 is reachable there. Publish host.internal / host.dew.internal
        # (dew's host.docker.internal) so dev config never hardcodes the gateway.
        # A 127.0.0.1-bound host service stays unreachable this way — that's what
        # host.lo.internal (written above) is for.
        HOST_GW=$(ip route 2>/dev/null | awk '$1=="default" && $2=="via" {print $3; exit}')
        [ -n "$HOST_GW" ] && printf '%s\thost.internal host.dew.internal\n' "$HOST_GW" >> /etc/hosts
    fi
    apply_netpolicy
    # Network bring-up is done (host.internal written if we have a NIC, or
    # legitimately absent if not) — release any container launch waiting to
    # snapshot /etc/hosts. Harmless when the marker was never set (restricted
    # path, where the bring-up was synchronous before any container could run).
    rm -f /run/dew-net-pending
}

# Take DHCP off the critical path. dew-agent rides vsock (not eth0), OCI images
# are pulled on the host and overlaid in (no guest fetch), port forwards are
# vsock proxies, host.lo.internal is vsock, and co-located services talk over
# localhost — so nothing the host orchestrates right after boot needs the guest
# NIC. Backgrounding the lease lets the agent answer ~0.5-2 s sooner. EXCEPTION:
# under a restricted egress policy the OUTPUT DROP is a security barrier that
# must be in force before anything can egress (and would itself block an
# in-flight DHCP), so there we keep the original synchronous order. The
# network-dependent boot steps below (first-boot apk, dew.cmd) wait on NET_PID;
# the host-driven `npm install` relies on the lease's head start plus npm's own
# retries rather than a guest-side barrier.
NET_PID=""
if [ "$NETPOLICY" = "restricted" ]; then
    bring_up_network
else
    # Mark the lease in flight: host.internal isn't in /etc/hosts until the
    # background bring_up_network appends it, and dew-oci-run snapshots the
    # guest's .internal lines into each container at launch. A container
    # started in this window would permanently miss host.internal, so
    # dew-oci-run waits for this marker to clear before snapshotting.
    : > /run/dew-net-pending
    bring_up_network &
    NET_PID=$!
fi

# wait_network blocks on the backgrounded bring_up_network exactly once, then
# clears NET_PID so a later barrier can't wait (and mis-reap) a stale PID. A
# final call before handing off reaps the child if no network-dependent step
# needed it — otherwise the lease process lingers as a zombie under PID 1. A
# no-op under a restricted policy, where the bring-up ran synchronously and
# NET_PID is empty.
wait_network() {
    [ -n "$NET_PID" ] || return 0
    wait "$NET_PID" 2>/dev/null
    NET_PID=""
}

# virtiofs mounts (need fuse + virtiofs modules after switch_root)
modprobe fuse 2>/dev/null || true
modprobe virtiofs 2>/dev/null || true
for share in $(cat /proc/cmdline | tr ' ' '\n' | grep '^dew.share='); do
    tag="${share#dew.share=}"
    tag_name="${tag%%:*}"
    mount_point="${tag#*:}"
    if [ -n "$tag_name" ] && [ -n "$mount_point" ]; then
        mkdir -p "$mount_point"
        mount -t virtiofs "$tag_name" "$mount_point" || echo "virtiofs mount failed: $tag_name → $mount_point"
    fi
done

# Rosetta x86_64 translation (Apple Silicon): mount the translator the host
# exposed as virtiofs tag "rosetta", then register the x86_64 ELF magic with
# binfmt_misc so the kernel hands amd64 binaries to Rosetta automatically.
# The F (fix-binary) flag makes the kernel open the interpreter at register
# time and reuse that fd, so the translation works inside container/chroot
# namespaces that can't see /mnt/rosetta — this is what makes amd64 containers
# run transparently. Must happen before containerd starts.
if grep -q 'dew.rosetta=1' /proc/cmdline 2>/dev/null; then
    mkdir -p /mnt/rosetta
    if mount -t virtiofs rosetta /mnt/rosetta 2>/dev/null; then
        chmod 0755 /mnt/rosetta/rosetta 2>/dev/null || true
        modprobe binfmt_misc 2>/dev/null || true
        mountpoint -q /proc/sys/fs/binfmt_misc 2>/dev/null || \
            mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc 2>/dev/null || true
        # printf (not echo) so the literal \xNN escapes reach the kernel's
        # binfmt parser instead of being expanded by the shell.
        if printf '%s' ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/mnt/rosetta/rosetta:OCF' \
            > /proc/sys/fs/binfmt_misc/register 2>/dev/null; then
            echo "rosetta: binfmt_misc registered (amd64 -> Rosetta)"
        else
            echo "rosetta: binfmt register failed (binfmt_misc missing/unmounted, interpreter unreadable, or already registered)"
        fi
    else
        echo "rosetta: virtiofs mount failed"
    fi
fi

# kernel cmdline params
DEW_CPU_QUOTA=""
DEW_MEM_LIMIT=""
DEW_CMD=""
for param in $(cat /proc/cmdline); do
    case "$param" in
        dew.cpu_quota=*) DEW_CPU_QUOTA="${param#dew.cpu_quota=}" ;;
        dew.mem_limit=*) DEW_MEM_LIMIT="${param#dew.mem_limit=}" ;;
        dew.cmd=*)       DEW_CMD="${param#dew.cmd=}" ;;
    esac
done

# cgroup v2
if grep -q cgroup2 /proc/filesystems 2>/dev/null; then
    mountpoint -q /sys/fs/cgroup || mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || true
    mkdir -p /sys/fs/cgroup/dew
    [ -n "$DEW_CPU_QUOTA" ] && echo "$DEW_CPU_QUOTA 100000" > /sys/fs/cgroup/dew/cpu.max 2>/dev/null
    [ -n "$DEW_MEM_LIMIT" ] && echo "$DEW_MEM_LIMIT" > /sys/fs/cgroup/dew/memory.max 2>/dev/null
fi

# unprivileged user with sudo
adduser -D -s /bin/sh dew 2>/dev/null || true
mkdir -p /etc/sudoers.d 2>/dev/null || true
echo "dew ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/dew 2>/dev/null || true
chmod 440 /etc/sudoers.d/dew 2>/dev/null || true

# Container runtime: dew uses crun via the host-pull + overlay path
# (dew-oci-run), launched on demand by `dew up --with` / `dew run --image`.
# There is no in-guest containerd/nerdctl daemon to start at boot. cgroup v2
# (mounted above) and overlay (in KMODS_BASE) are all crun needs.

# dew-agent. Start BEFORE the per-runtime apk install below — the
# released node profile bakes nodejs+npm into the initramfs, so the
# only first-boot install left is build-base + python3 (optional
# native-build deps). cmdUp's `npm install` only needs node+npm and
# can proceed in parallel; making it wait through a 30-60 s apk run
# breaks `dew up` end-to-end (vsock auth races, install reported as
# failed, dev server never reachable).
# VM is the isolation boundary — run agent as root by default.
# DEW_EXEC_USER=dew opts into unprivileged-user exec for untrusted code.
AGENT_ENV=""
if [ -x /usr/local/bin/dew-agent ] && [ -e /dev/vsock ]; then
    env $AGENT_ENV /usr/local/bin/dew-agent >/dev/null 2>&1 &
    echo "dew-agent: vsock ready"
fi

# First-boot runtime install. The released initramfs (Linux CI) bakes
# nodejs + npm into the node profile and python3 + pip into the python
# profile, so the install below is a no-op for users. The Darwin-local
# dev build can't bake (no apk-tools-static for Darwin), so node/python
# get fetched on first boot here.
#
# build-base + python3 (for node-gyp) used to be installed in the
# background here so npm install of a project with native deps would
# eventually succeed. That work moved to cmdUp on the host side
# (internal/detect.NeedsNativeBuildTools): cmdUp now scans the
# project's lockfile and triggers the apk install with a user-visible
# progress message when and only when the project actually needs it.
# Removing the eager background install means pure-JS projects pay
# zero apk cost.
node_first_boot_apk() {
    local NEED=""
    command -v node >/dev/null 2>&1 || NEED="$NEED nodejs npm"
    if [ -n "$NEED" ]; then
        echo "dew: installing$NEED (first boot)..."
        apk update 2>/dev/null || true
        apk upgrade --no-cache musl 2>&1 | tail -1
        apk add --no-cache $NEED 2>&1 | tail -1
        # Same rationale as the stage-1 populate sync: the host can
        # kill the VM at any moment; don't leave the freshly-installed
        # runtime sitting in the guest page cache.
        sync
    fi
}

python_first_boot_apk() {
    local NEED=""
    command -v python3 >/dev/null 2>&1 || NEED="$NEED python3 py3-pip"
    if [ -n "$NEED" ]; then
        echo "dew: installing$NEED (first boot)..."
        apk update 2>/dev/null || true
        apk upgrade --no-cache musl 2>&1 | tail -1
        apk add --no-cache $NEED 2>&1 | tail -1
        sync
    fi
}

# The first-boot runtime install fetches packages over apk, so it needs the
# lease — wait for the backgrounded bring_up_network (a no-op when it already
# ran synchronously under a restricted policy, where NET_PID is empty).
if [ -f /.dew-node-profile ] || [ -f /.dew-python-profile ]; then
    wait_network
fi
[ -f /.dew-node-profile ]   && node_first_boot_apk
[ -f /.dew-python-profile ] && python_first_boot_apk

# startup command (dew.cmd, set by `dew start <cmd>`). A baked boot command may
# expect the network, so wait for the backgrounded lease first.
if [ -n "$DEW_CMD" ]; then
    wait_network
    DECODED=$(echo "$DEW_CMD" | base64 -d 2>/dev/null)
    [ -n "$DECODED" ] && sh -c "$DECODED" &
fi

# Reap the lease child if no network-dependent step waited on it, so it
# doesn't linger as a zombie under PID 1 (a no-op once NET_PID is cleared).
wait_network

echo ""
echo "  dew vm ready"
echo ""

exec /bin/sh 2>/dev/null
INIT2_EOF
chmod 755 "${WORK_DIR}/init-stage2"

# --- Step 7: Pack ---
echo "--- Step 7: Pack initramfs ---"
(cd "$WORK_DIR" && find . | cpio -o -H newc 2>/dev/null | gzip -9 > "$OUT_INITRD")
echo "Profile:   $PROFILE"
echo "Initramfs: $(ls -lh "$OUT_INITRD" | awk '{print $5}') $OUT_INITRD"
echo "Kernel:    $(ls -lh "$OUT_KERNEL" | awk '{print $5}')"

# Symlink default profile
ln -sf "$(basename "$OUT_INITRD")" "${SCRIPT_DIR}/initramfs.cpio.gz"

# Cleanup
rm -rf "$WORK_DIR" "$APK_EXTRACT"

echo ""
echo "=== Done ==="
