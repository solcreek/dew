#!/bin/bash
set -euo pipefail

# Build a custom initramfs for Dew VMs.
# Downloads Alpine virt kernel + minirootfs, adds vsock modules + dew-agent.
#
# Output:
#   initramfs/vmlinuz          — Alpine virt kernel
#   initramfs/initramfs.cpio.gz — custom initramfs with vsock + dew-agent

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
WORK_DIR="${SCRIPT_DIR}/work"
OUT_INITRD="${SCRIPT_DIR}/initramfs.cpio.gz"
OUT_KERNEL="${SCRIPT_DIR}/vmlinuz"

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

echo "=== Building Dew initramfs ==="
echo "Arch: ${ALPINE_ARCH}"
echo ""

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

# --- Step 1: Download Alpine minirootfs ---
echo "--- Step 1: Alpine minirootfs ---"
TARBALL="${SCRIPT_DIR}/cache/alpine-minirootfs-${ALPINE_MINOR}-${ALPINE_ARCH}.tar.gz"
mkdir -p "${SCRIPT_DIR}/cache"
if [ ! -f "$TARBALL" ]; then
    curl -fSL -o "$TARBALL" "$ROOTFS_URL"
else
    echo "Using cached tarball"
fi
tar xzf "$TARBALL" -C "$WORK_DIR"
echo "Rootfs: $(du -sh "$WORK_DIR" | awk '{print $1}')"

# --- Step 2: Download kernel + vsock modules ---
echo "--- Step 2: Alpine virt kernel + vsock modules ---"
KERNEL_APK="${SCRIPT_DIR}/cache/linux-virt.apk"
if [ ! -f "$KERNEL_APK" ]; then
    curl -fSL -o "$KERNEL_APK" "$KERNEL_APK_URL"
else
    echo "Using cached kernel APK"
fi
APK_EXTRACT="${SCRIPT_DIR}/cache/apk-extract"
rm -rf "$APK_EXTRACT"
mkdir -p "$APK_EXTRACT"
tar xzf "$KERNEL_APK" -C "$APK_EXTRACT" 2>/dev/null

# Copy kernel
cp "$APK_EXTRACT/boot/vmlinuz-virt" "$OUT_KERNEL"
echo "Kernel: $(ls -lh "$OUT_KERNEL" | awk '{print $5}')"

# Copy vsock + virtio modules into initramfs
MODDIR="${WORK_DIR}/lib/modules/${KERNEL_VER}"
mkdir -p "$MODDIR/kernel/net/vmw_vsock"
cp "$APK_EXTRACT/lib/modules/${KERNEL_VER}/kernel/net/vmw_vsock/vsock.ko.gz" \
   "$APK_EXTRACT/lib/modules/${KERNEL_VER}/kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz" \
   "$APK_EXTRACT/lib/modules/${KERNEL_VER}/kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz" \
   "$MODDIR/kernel/net/vmw_vsock/"

# Copy virtio_net for networking
mkdir -p "$MODDIR/kernel/drivers/net"
cp "$APK_EXTRACT/lib/modules/${KERNEL_VER}/kernel/drivers/net/virtio_net.ko.gz" \
   "$MODDIR/kernel/drivers/net/" 2>/dev/null || true

# Copy modules.dep and related files
for f in modules.dep modules.dep.bin modules.alias modules.alias.bin modules.symbols modules.symbols.bin modules.builtin modules.order; do
    cp "$APK_EXTRACT/lib/modules/${KERNEL_VER}/$f" "$MODDIR/$f" 2>/dev/null || true
done

echo "Modules: $(du -sh "$MODDIR" | awk '{print $1}')"

# --- Step 3: Build dew-agent ---
echo "--- Step 3: Build dew-agent ---"
mkdir -p "${WORK_DIR}/usr/local/bin"
GOOS=linux GOARCH="$GO_ARCH" CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o "${WORK_DIR}/usr/local/bin/dew-agent" \
    "${REPO_DIR}/cmd/dew-agent/"
echo "Agent: $(ls -lh "${WORK_DIR}/usr/local/bin/dew-agent" | awk '{print $5}')"

# --- Step 4: Custom init ---
echo "--- Step 4: Custom init ---"
cat > "${WORK_DIR}/init" << 'INIT_EOF'
#!/bin/sh
# Dew VM init

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

# load vsock modules
modprobe vsock 2>/dev/null
modprobe vmw_vsock_virtio_transport 2>/dev/null

# network (if eth0 exists = NAT mode)
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 up
    udhcpc -i eth0 -s /usr/share/udhcpc/default.script -q -n 2>/dev/null || true
fi

# virtiofs mounts (kernel cmdline: dew.share=tag:/mountpoint)
for share in $(cat /proc/cmdline | tr ' ' '\n' | grep '^dew.share='); do
    tag="${share#dew.share=}"
    tag_name="${tag%%:*}"
    mount_point="${tag#*:}"
    if [ -n "$tag_name" ] && [ -n "$mount_point" ]; then
        mkdir -p "$mount_point"
        mount -t virtiofs "$tag_name" "$mount_point" 2>/dev/null || true
    fi
done

# extract auth token from kernel cmdline
DEW_TOKEN=""
for param in $(cat /proc/cmdline); do
    case "$param" in
        dew.token=*) DEW_TOKEN="${param#dew.token=}" ;;
    esac
done
export DEW_TOKEN

# dew-agent (vsock exec channel)
if [ -x /usr/local/bin/dew-agent ] && [ -e /dev/vsock ]; then
    /usr/local/bin/dew-agent >/dev/null 2>&1 &
    echo "dew-agent: vsock ready"
fi

echo ""
echo "  dew vm ready"
echo ""

exec /bin/sh 2>/dev/null
INIT_EOF
chmod 755 "${WORK_DIR}/init"

# --- Step 5: Pack initramfs ---
echo "--- Step 5: Pack initramfs ---"
(cd "$WORK_DIR" && find . | cpio -o -H newc 2>/dev/null | gzip -9 > "$OUT_INITRD")
echo "Initramfs: $(ls -lh "$OUT_INITRD" | awk '{print $5}')"
echo "Kernel:    $(ls -lh "$OUT_KERNEL" | awk '{print $5}')"

# Cleanup
rm -rf "$WORK_DIR" "$APK_EXTRACT"

echo ""
echo "=== Done ==="
echo "Usage: dew start --kernel ${OUT_KERNEL} --initrd ${OUT_INITRD}"
