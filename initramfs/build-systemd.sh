#!/bin/sh
# build-systemd.sh — assemble the dew `systemd` profile rootfs (R1).
#
# Unlike build.sh (Alpine busybox, runnable on the macOS dev host), this profile
# needs a real systemd userland, so it debootstraps Debian and MUST run on a
# Debian/Ubuntu host with root — e.g. the colima/lima Linux VM, or a Debian CI
# container. The kernel stays dew's Alpine `linux-virt` (modules are injected
# from an existing dew initramfs), so host and guest kernel match.
#
# Output is a cpio.gz the kernel unpacks to a tmpfs rootfs and boots with PID 1 =
# systemd (the rootfs has no /etc/initrd-release, so systemd runs as the real
# root, not in initrd mode). dew-agent is supervised by systemd over vsock.
#
# Usage (run as root):
#   build-systemd.sh --arch arm64 \
#       --modules-from <dew-initramfs.cpio.gz> \
#       --agent <dew-agent-linux-arch> \
#       --out initramfs-systemd-aarch64.cpio.gz \
#       [--suite bookworm] [--mirror http://deb.debian.org/debian] [--debug]
#
# --modules-from : an existing dew initramfs (e.g. the standard profile) whose
#                  /lib/modules/<kver> matches the vmlinuz this profile boots with.
# --debug        : empty root password + autologin on the hvc0 serial console
#                  (for `dew run --json` boot debugging). NEVER ship with --debug.
set -eu

ARCH=""
MODULES_FROM=""
AGENT=""
OUT=""
SUITE="bookworm"
MIRROR="http://deb.debian.org/debian"
DEBUG=0

# systemd-as-PID1 essentials + the bits dew/systemd-analyze need.
INCLUDE="systemd,systemd-sysv,udev,dbus,libpam-systemd,iproute2,iputils-ping"

while [ $# -gt 0 ]; do
    case "$1" in
        --arch)         ARCH="$2"; shift 2 ;;
        --modules-from) MODULES_FROM="$2"; shift 2 ;;
        --agent)        AGENT="$2"; shift 2 ;;
        --out)          OUT="$2"; shift 2 ;;
        --suite)        SUITE="$2"; shift 2 ;;
        --mirror)       MIRROR="$2"; shift 2 ;;
        --debug)        DEBUG=1; shift ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

[ -n "$ARCH" ] || { echo "--arch required (arm64|amd64)" >&2; exit 2; }
[ -n "$MODULES_FROM" ] && [ -f "$MODULES_FROM" ] || { echo "--modules-from <dew-initramfs.cpio.gz> required" >&2; exit 2; }
[ -n "$AGENT" ] && [ -f "$AGENT" ] || { echo "--agent <dew-agent binary> required" >&2; exit 2; }
[ -n "$OUT" ] || { echo "--out required" >&2; exit 2; }
[ "$(id -u)" = "0" ] || { echo "must run as root (debootstrap + device nodes + file ownership)" >&2; exit 2; }
for t in debootstrap cpio gzip depmod; do
    command -v "$t" >/dev/null 2>&1 || { echo "missing tool: $t (apt-get install debootstrap cpio kmod)" >&2; exit 2; }
done

WORK="$(mktemp -d)"
ROOT="$WORK/sysroot"
MODS="$WORK/mods"
cleanup() {
    # debootstrap may leave /proc /sys bind-mounted in the rootfs on failure.
    umount -R "$ROOT/proc" 2>/dev/null || true
    umount -R "$ROOT/sys" 2>/dev/null || true
    umount -lR "$ROOT" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "=== debootstrap $SUITE ($ARCH) ==="
debootstrap --arch="$ARCH" --variant=minbase --include="$INCLUDE" \
    "$SUITE" "$ROOT" "$MIRROR"

echo "=== inject dew kernel modules from $MODULES_FROM ==="
mkdir -p "$MODS"
gzip -dc "$MODULES_FROM" | (cd "$MODS" && cpio -idm 2>/dev/null)
KVER="$(ls "$MODS/lib/modules" | head -1)"
[ -n "$KVER" ] || { echo "no /lib/modules in $MODULES_FROM" >&2; exit 1; }
echo "kernel modules: $KVER"
mkdir -p "$ROOT/lib/modules"
cp -a "$MODS/lib/modules/$KVER" "$ROOT/lib/modules/"
depmod -b "$ROOT" "$KVER"

echo "=== boot wiring ==="
# The kernel runs /init from the initramfs; point it at systemd.
ln -sf /sbin/init "$ROOT/init"

# dew-agent, supervised by systemd over vsock.
install -D -m0755 "$AGENT" "$ROOT/usr/local/bin/dew-agent"
cat > "$ROOT/etc/systemd/system/dew-agent.service" <<EOF
[Unit]
Description=dew guest agent
DefaultDependencies=no
After=local-fs.target systemd-modules-load.service
Before=sysinit.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dew-agent
Restart=on-failure

[Install]
WantedBy=sysinit.target
EOF
mkdir -p "$ROOT/etc/systemd/system/sysinit.target.wants"
ln -sf ../dew-agent.service "$ROOT/etc/systemd/system/sysinit.target.wants/dew-agent.service"

# Load vsock (for the agent) + virtio + ext4 early via systemd-modules-load.
cat > "$ROOT/etc/modules-load.d/dew.conf" <<EOF
vmw_vsock_virtio_transport
virtio_blk
virtio_net
ext4
EOF

if [ "$DEBUG" = "1" ]; then
    echo "=== DEBUG: empty root password + hvc0 autologin (do NOT ship) ==="
    sed -i 's|^root:[^:]*:|root::|' "$ROOT/etc/shadow"
    mkdir -p "$ROOT/etc/systemd/system/serial-getty@hvc0.service.d"
    cat > "$ROOT/etc/systemd/system/serial-getty@hvc0.service.d/autologin.conf" <<EOF
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear %I 115200 vt220
EOF
fi

echo "=== pack $OUT ==="
( cd "$ROOT" && find . | cpio -o -H newc 2>/dev/null ) | gzip -1 > "$OUT"
echo "Profile:   systemd ($SUITE, $ARCH)"
echo "Kernel:    $KVER"
echo "Rootfs:    $(du -sh "$ROOT" | cut -f1) uncompressed"
echo "Output:    $OUT ($(du -h "$OUT" | cut -f1))"
