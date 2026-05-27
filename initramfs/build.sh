#!/bin/bash
set -euo pipefail

# Build Dew initramfs.
#
# Usage:
#   ./build.sh              # standard profile (default)
#   ./build.sh minimal      # exec-only, no container runtime
#   ./build.sh standard     # containerd + nerdctl + runc + CNI

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

# Copy ALL kernel modules (depmod handles dependencies automatically)
mkdir -p "$WORK_DIR/lib/modules"
cp -a "$APK_EXTRACT/lib/modules/${KERNEL_VER}" "$WORK_DIR/lib/modules/"
find "$WORK_DIR/lib/modules/${KERNEL_VER}" -name "*.ko.gz" -exec gunzip {} \;
echo "Modules: $(du -sh "$MODDIR" | awk '{print $1}')"

# --- Step 3: Build dew-agent ---
echo "--- Step 3: Build dew-agent ---"
mkdir -p "${WORK_DIR}/usr/local/bin"
GOOS=linux GOARCH="$GO_ARCH" CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o "${WORK_DIR}/usr/local/bin/dew-agent" \
    "${REPO_DIR}/cmd/dew-agent/"
echo "Agent: $(ls -lh "${WORK_DIR}/usr/local/bin/dew-agent" | awk '{print $5}')"

# --- Step 4: Container runtime (standard profile only) ---
if [ "$PROFILE" = "standard" ]; then
    echo "--- Step 4: Container runtime ---"

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

    # Install e2fsprogs + dependencies (extract APKs directly)
    echo "Installing e2fsprogs..."
    E2FS_PKGS="e2fsprogs e2fsprogs-libs libcom_err libuuid libblkid libeconf"
    for pkg in $E2FS_PKGS; do
        APK_FILE="${CACHE}/${pkg}.apk"
        if [ ! -f "$APK_FILE" ]; then
            URL=$(curl -sL "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/" \
                  | grep -o "${pkg}-[0-9][^\"]*\.apk" | head -1)
            [ -n "$URL" ] && curl -fSL -o "$APK_FILE" \
                "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/main/${ALPINE_ARCH}/${URL}" || true
        fi
        [ -f "$APK_FILE" ] && tar xzf "$APK_FILE" -C "$WORK_DIR" 2>/dev/null || true
    done
    echo "e2fsprogs: $(ls "$WORK_DIR/sbin/mkfs.ext4" 2>/dev/null && echo 'OK' || echo 'MISSING')"

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

# ── standard profile: switch_root to ext4 disk ──
# containerd needs overlayfs on a real filesystem (not tmpfs).
# If /dev/vda exists and containerd is present, switch_root to disk.
HAS_CONTAINERD=false
[ -x /usr/local/bin/containerd ] && HAS_CONTAINERD=true

if [ "$HAS_CONTAINERD" = true ]; then
    # Wait for virtio_blk to create /dev/vda
    for i in 1 2 3 4 5 6 7 8 9 10; do
        [ -b /dev/vda ] && break
        sleep 0.1
    done
fi

if [ "$HAS_CONTAINERD" = true ] && [ -b /dev/vda ]; then
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
        cp -a /bin /etc /lib /opt /sbin /usr /var /init-stage2 /mnt/root/ 2>/dev/null || true
        mkdir -p /mnt/root/dev /mnt/root/proc /mnt/root/sys \
                 /mnt/root/run /mnt/root/tmp /mnt/root/data \
                 /mnt/root/dev/pts /mnt/root/dev/shm
        touch /mnt/root/.dew-initialized
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

# network
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 up 2>/dev/null || true
    for i in 1 2 3 4 5; do
        [ "$(cat /sys/class/net/eth0/carrier 2>/dev/null)" = "1" ] && break
        sleep 0.1
    done
    udhcpc -i eth0 -s /usr/share/udhcpc/default.script -q -t 3 || true
    echo "nameserver 1.1.1.1" > /etc/resolv.conf
fi

# virtiofs mounts
for share in $(cat /proc/cmdline | tr ' ' '\n' | grep '^dew.share='); do
    tag="${share#dew.share=}"
    tag_name="${tag%%:*}"
    mount_point="${tag#*:}"
    if [ -n "$tag_name" ] && [ -n "$mount_point" ]; then
        mkdir -p "$mount_point"
        mount -t virtiofs "$tag_name" "$mount_point" 2>/dev/null || true
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

# unprivileged user
adduser -D -s /bin/sh dew 2>/dev/null || true

# containerd (standard profile, now on ext4 rootfs)
if [ -x /usr/local/bin/containerd ]; then
    mkdir -p /run/containerd /var/lib/containerd /var/lib/nerdctl
    containerd >/var/log/containerd.log 2>&1 &
    sleep 0.5
    echo "containerd: started"
fi

# dew-agent
AGENT_ENV=""
[ ! -x /usr/local/bin/containerd ] && AGENT_ENV="DEW_EXEC_USER=dew"
if [ -x /usr/local/bin/dew-agent ] && [ -e /dev/vsock ]; then
    env $AGENT_ENV /usr/local/bin/dew-agent >/dev/null 2>&1 &
    echo "dew-agent: vsock ready"
fi

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
