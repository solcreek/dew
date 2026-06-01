#!/bin/bash
set -euo pipefail

# Build Dew initramfs.
#
# Usage:
#   ./build.sh              # standard profile (default)
#   ./build.sh minimal      # exec-only, no container runtime
#   ./build.sh node          # Node.js + npm + build-base (React/Vite ready)
#   ./build.sh standard     # node + containerd + nerdctl + runc + CNI

PROFILE="${1:-standard}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="${SCRIPT_DIR}/work"
OUT_INITRD="${SCRIPT_DIR}/initramfs-${PROFILE}.cpio.gz"
OUT_KERNEL="${SCRIPT_DIR}/vmlinuz"
CACHE="${SCRIPT_DIR}/cache"

ALPINE_VERSION="3.21"
ALPINE_MINOR="3.21.3"
ARCH="$(uname -m)"

case "$ARCH" in
    x86_64)  ALPINE_ARCH="x86_64"; GO_ARCH="amd64" ;;
    arm64)   ALPINE_ARCH="aarch64"; GO_ARCH="arm64" ;;
    aarch64) ALPINE_ARCH="aarch64"; GO_ARCH="arm64" ;;
    *)       echo "unsupported arch: $ARCH"; exit 1 ;;
esac

ROOTFS_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/${ALPINE_ARCH}/alpine-minirootfs-${ALPINE_MINOR}-${ALPINE_ARCH}.tar.gz"
KERNEL_APK_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/linux-virt-6.12.91-r0.apk"
KERNEL_VER="6.12.91-0-virt"

# Container runtime versions
CONTAINERD_VERSION="2.1.1"
NERDCTL_VERSION="2.1.2"
RUNC_VERSION="1.2.6"
CNI_VERSION="1.7.1"

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
KERNEL_APK="${CACHE}/linux-virt.apk"
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
            crc32c_generic"
# Container networking (CNI bridge + masquerade) — standard profile only.
KMODS_STANDARD="bridge br_netfilter veth iptable_nat nf_nat \
                xt_MASQUERADE xt_addrtype"
KMODS_KEEP="$KMODS_BASE"
if [ "$PROFILE" = "standard" ]; then
    KMODS_KEEP="$KMODS_KEEP $KMODS_STANDARD"
fi

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
    local apk_static_bin="${CACHE}/apk.static"
    if [ -x "$apk_static_bin" ]; then
        echo "$apk_static_bin"
        return 0
    fi
    local listing_url="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/"
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
    APK_FILE="${CACHE}/${pkg}.apk"
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

# --- Step 4b: Container runtime (standard profile only) ---
if [ "$PROFILE" = "standard" ]; then
    echo "--- Step 4b: Container runtime ---"

    # containerd (static build for musl/Alpine)
    CONTAINERD_TAR="${CACHE}/containerd-static-${CONTAINERD_VERSION}-linux-${GO_ARCH}.tar.gz"
    if [ ! -f "$CONTAINERD_TAR" ]; then
        echo "Downloading containerd ${CONTAINERD_VERSION} (static)..."
        curl -fSL -o "$CONTAINERD_TAR" \
            "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-static-${CONTAINERD_VERSION}-linux-${GO_ARCH}.tar.gz"
    fi
    tar xzf "$CONTAINERD_TAR" -C "$WORK_DIR/usr/local/" 2>/dev/null
    echo "containerd: $(ls -lh "$WORK_DIR/usr/local/bin/containerd" | awk '{print $5}')"

    # runc
    RUNC_BIN="${CACHE}/runc-${RUNC_VERSION}-${GO_ARCH}"
    if [ ! -f "$RUNC_BIN" ]; then
        echo "Downloading runc ${RUNC_VERSION}..."
        curl -fSL -o "$RUNC_BIN" \
            "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${GO_ARCH}"
    fi
    cp "$RUNC_BIN" "$WORK_DIR/usr/local/bin/runc"
    chmod 755 "$WORK_DIR/usr/local/bin/runc"
    echo "runc: $(ls -lh "$WORK_DIR/usr/local/bin/runc" | awk '{print $5}')"

    # nerdctl
    NERDCTL_TAR="${CACHE}/nerdctl-${NERDCTL_VERSION}-linux-${GO_ARCH}.tar.gz"
    if [ ! -f "$NERDCTL_TAR" ]; then
        echo "Downloading nerdctl ${NERDCTL_VERSION}..."
        curl -fSL -o "$NERDCTL_TAR" \
            "https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${GO_ARCH}.tar.gz"
    fi
    tar xzf "$NERDCTL_TAR" -C "$WORK_DIR/usr/local/bin/" nerdctl 2>/dev/null
    echo "nerdctl: $(ls -lh "$WORK_DIR/usr/local/bin/nerdctl" | awk '{print $5}')"

    # CNI plugins
    CNI_TAR="${CACHE}/cni-plugins-${CNI_VERSION}-linux-${GO_ARCH}.tgz"
    if [ ! -f "$CNI_TAR" ]; then
        echo "Downloading CNI plugins ${CNI_VERSION}..."
        curl -fSL -o "$CNI_TAR" \
            "https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-${GO_ARCH}-v${CNI_VERSION}.tgz"
    fi
    mkdir -p "$WORK_DIR/opt/cni/bin"
    tar xzf "$CNI_TAR" -C "$WORK_DIR/opt/cni/bin/" 2>/dev/null
    echo "CNI: $(du -sh "$WORK_DIR/opt/cni/bin" | awk '{print $1}')"

    # e2fsprogs already installed by node/standard shared block above

    # containerd config
    mkdir -p "$WORK_DIR/etc/containerd"
    cat > "$WORK_DIR/etc/containerd/config.toml" << 'CTRD_EOF'
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.cri.v1.runtime".cni]
  bin_dir = "/opt/cni/bin"
CTRD_EOF
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
modprobe vsock 2>/dev/null || true
modprobe vmw_vsock_virtio_transport 2>/dev/null || true

# ── switch_root to ext4 disk (node/standard profiles with --disk) ──
# Provides real filesystem for npm cache, node_modules, containerd.
# Wait for virtio_blk to create /dev/vda
for i in 1 2 3 4 5 6 7 8 9 10; do
    [ -b /dev/vda ] && break
    sleep 0.1
done

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
    fi

    # Always refresh init-stage2 and markers from initramfs
    cp /init-stage2 /mnt/root/init-stage2
    chmod 755 /mnt/root/init-stage2
    [ -f /.dew-node-profile ] && cp /.dew-node-profile /mnt/root/.dew-node-profile
    [ -f /.dew-python-profile ] && cp /.dew-python-profile /mnt/root/.dew-python-profile

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

# network. Apple VZ's host-managed NAT is racy at first-boot: usually
# it answers DHCP within ~2 s, occasionally not until ~60-80 s. The
# default busybox udhcpc invocation (`-t 3 -q`) retries the 3-packet
# discovery burst forever (no `-n`), so the visible symptom is the
# user staring at "udhcpc: broadcasting discover" repeating every 30 s
# while init-stage2 blocks. `-n -t 30 -T 3` makes ONE patient
# attempt: 30 discovers spaced 3 s apart (~90 s window), then exit
# pass-or-fail so init-stage2 can proceed either way.
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 up 2>/dev/null || true
    for i in 1 2 3 4 5 6 7 8 9 10; do
        [ "$(cat /sys/class/net/eth0/carrier 2>/dev/null)" = "1" ] && break
        sleep 0.1
    done
    udhcpc -i eth0 -s /usr/share/udhcpc/default.script -q -n -t 30 -T 3 || true
    echo "nameserver 1.1.1.1" > /etc/resolv.conf
fi

# Egress policy. When the host passes dew.netpolicy=restricted on the
# kernel cmdline, set OUTPUT default to DROP and explicitly accept:
#   - loopback
#   - DNS to the configured resolver (so /etc/resolv.conf still works)
#   - the IPv4 addresses listed in dew.allow=ip1,ip2,...
# When dew.netpolicy is unset (the default), egress is open as before
# — this is opt-in for v1; default-deny + hostname allowlist is a
# follow-up that needs a DNS-aware proxy.
NETPOLICY=""
ALLOW_IPS=""
for param in $(cat /proc/cmdline 2>/dev/null); do
    case "$param" in
        dew.netpolicy=*) NETPOLICY="${param#dew.netpolicy=}" ;;
        dew.allow=*)     ALLOW_IPS="${param#dew.allow=}" ;;
    esac
done
if [ "$NETPOLICY" = "restricted" ] && command -v iptables >/dev/null 2>&1; then
    # Load the netfilter modules iptables needs. The nft backend is
    # Alpine's default; ip_tables/iptable_filter cover the legacy
    # backend that older user scripts may use directly.
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
fi

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

# containerd (standard profile, now on ext4 rootfs)
if [ -x /usr/local/bin/containerd ]; then
    # CNI bridge networking modules. Without these the bridge plugin fails
    # with `could not add "nerdctl0": operation not supported` because the
    # bridge driver isn't registered with the kernel.
    modprobe bridge 2>/dev/null || true
    modprobe br_netfilter 2>/dev/null || true
    modprobe veth 2>/dev/null || true
    # Masquerade NAT for outbound traffic from containers
    modprobe iptable_nat 2>/dev/null || true
    modprobe nf_nat 2>/dev/null || true
    modprobe xt_MASQUERADE 2>/dev/null || true
    modprobe xt_addrtype 2>/dev/null || true
    # iptables needed for CNI bridge networking
    command -v iptables >/dev/null 2>&1 || apk add -q --no-cache iptables 2>/dev/null || true
    # TMPDIR for nerdctl/containerd scratch mounts
    export TMPDIR=/tmp/containerd-tmp
    mkdir -p /tmp/containerd-tmp
    chmod 1777 /tmp/containerd-tmp
    mkdir -p /run/containerd /var/lib/containerd /var/lib/nerdctl
    containerd >/var/log/containerd.log 2>&1 &
    sleep 0.5
    echo "containerd: started"
fi

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

# Per-runtime first-boot install. Split into critical (the runtime
# itself, blocking) and optional (build-base + python3, backgrounded).
# The released Linux-CI initramfs bakes node+npm, so the critical
# branch is a no-op and cmdUp's `npm install` proceeds in parallel
# with the slow build-base/python3 fetch. The Darwin-local build path
# can't bake (no apk-tools-static for the host's libc), so node falls
# into the critical branch and still blocks — this is only us during
# development, not what users see from the release.
node_critical_apk() {
    local CRITICAL_APK="" OPTIONAL_APK=""
    command -v node    >/dev/null 2>&1 || CRITICAL_APK="$CRITICAL_APK nodejs npm"
    command -v gcc     >/dev/null 2>&1 || OPTIONAL_APK="$OPTIONAL_APK build-base"
    command -v python3 >/dev/null 2>&1 || OPTIONAL_APK="$OPTIONAL_APK python3"
    if [ -n "$CRITICAL_APK" ]; then
        echo "dew: installing$CRITICAL_APK (first boot)..."
        apk update 2>/dev/null || true
        apk upgrade --no-cache musl 2>&1 | tail -1
        apk add --no-cache $CRITICAL_APK 2>&1 | tail -1
    fi
    if [ -n "$OPTIONAL_APK" ]; then
        echo "dew: installing$OPTIONAL_APK (background)..."
        (
            [ -z "$CRITICAL_APK" ] && apk update >/dev/null 2>&1
            apk add --no-cache $OPTIONAL_APK >/dev/null 2>&1
            echo "dew: background install done:$OPTIONAL_APK"
        ) &
    fi
}

python_critical_apk() {
    local CRITICAL_APK="" OPTIONAL_APK=""
    command -v python3 >/dev/null 2>&1 || CRITICAL_APK="$CRITICAL_APK python3 py3-pip"
    command -v gcc     >/dev/null 2>&1 || OPTIONAL_APK="$OPTIONAL_APK build-base"
    if [ -n "$CRITICAL_APK" ]; then
        echo "dew: installing$CRITICAL_APK (first boot)..."
        apk update 2>/dev/null || true
        apk upgrade --no-cache musl 2>&1 | tail -1
        apk add --no-cache $CRITICAL_APK 2>&1 | tail -1
    fi
    if [ -n "$OPTIONAL_APK" ]; then
        echo "dew: installing$OPTIONAL_APK (background)..."
        (
            [ -z "$CRITICAL_APK" ] && apk update >/dev/null 2>&1
            apk add --no-cache $OPTIONAL_APK >/dev/null 2>&1
            echo "dew: background install done:$OPTIONAL_APK"
        ) &
    fi
}

[ -f /.dew-node-profile ]   && node_critical_apk
[ -f /.dew-python-profile ] && python_critical_apk

# startup command
if [ -n "$DEW_CMD" ]; then
    DECODED=$(echo "$DEW_CMD" | base64 -d 2>/dev/null)
    [ -n "$DECODED" ] && sh -c "$DECODED" &
fi

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
