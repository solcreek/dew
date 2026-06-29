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
#       [--suite bookworm] [--mirror https://deb.debian.org/debian] [--debug]
#
# --arch         : the debootstrap arch — arm64 or amd64. The --out name is
#                  free-form; "aarch64" here just mirrors dew's vmlinuz-<arch>
#                  asset naming (uname -m), it is NOT an --arch value.
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
MIRROR="https://deb.debian.org/debian"
DEBUG=0

# systemd-as-PID1 essentials + the bits dew/systemd-analyze need.
INCLUDE="systemd,systemd-sysv,udev,dbus,libpam-systemd,iproute2,iputils-ping"

# A value-taking flag needs a following token; without the guard, `set -u` turns
# a trailing `--arch` into an "unbound variable" abort (and `shift 2` would also
# run past the end) instead of this clear message.
need_val() { [ "$2" -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }; }
while [ $# -gt 0 ]; do
    case "$1" in
        --arch)         need_val "$1" "$#"; ARCH="$2"; shift 2 ;;
        --modules-from) need_val "$1" "$#"; MODULES_FROM="$2"; shift 2 ;;
        --agent)        need_val "$1" "$#"; AGENT="$2"; shift 2 ;;
        --out)          need_val "$1" "$#"; OUT="$2"; shift 2 ;;
        --suite)        need_val "$1" "$#"; SUITE="$2"; shift 2 ;;
        --mirror)       need_val "$1" "$#"; MIRROR="$2"; shift 2 ;;
        --debug)        DEBUG=1; shift ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

# Validate the arch early so a typo fails here, not deep inside debootstrap.
case "$ARCH" in
    arm64|amd64) ;;
    "") echo "--arch required (arm64|amd64)" >&2; exit 2 ;;
    *) echo "--arch must be arm64 or amd64 (got: $ARCH)" >&2; exit 2 ;;
esac
[ -n "$MODULES_FROM" ] && [ -f "$MODULES_FROM" ] || { echo "--modules-from <dew-initramfs.cpio.gz> required" >&2; exit 2; }
[ -n "$AGENT" ] && [ -f "$AGENT" ] || { echo "--agent <dew-agent binary> required" >&2; exit 2; }
[ -n "$OUT" ] || { echo "--out required" >&2; exit 2; }
[ "$(id -u)" = "0" ] || { echo "must run as root (debootstrap + device nodes + file ownership)" >&2; exit 2; }
for t in debootstrap cpio gzip depmod; do
    command -v "$t" >/dev/null 2>&1 || { echo "missing tool: $t (apt-get install debootstrap cpio gzip kmod)" >&2; exit 2; }
done
# Require the Debian archive keyring and pass it explicitly. On an Ubuntu build
# host (dew's colima VM) debootstrap defaults to Ubuntu's keyring and would
# otherwise fetch the Debian suite without verifying its signature.
KEYRING="/usr/share/keyrings/debian-archive-keyring.gpg"
[ -f "$KEYRING" ] || { echo "missing Debian archive keyring: $KEYRING (apt-get install debian-archive-keyring)" >&2; exit 2; }

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
debootstrap --arch="$ARCH" --variant=minbase --keyring="$KEYRING" --include="$INCLUDE" \
    "$SUITE" "$ROOT" "$MIRROR"

echo "=== inject dew kernel modules from $MODULES_FROM ==="
mkdir -p "$MODS"
# Split the gzip|cpio pipeline so a decompression failure aborts under set -e
# (#!/bin/sh is dash on the Debian build host — no pipefail, so a masked gzip
# failure would otherwise leave $MODS silently incomplete). --no-absolute-
# filenames keeps a malformed archive from writing outside $MODS.
gzip -dc "$MODULES_FROM" > "$WORK/modules.cpio"
# Preflight: --no-absolute-filenames blocks absolute paths but not `..`
# components, so a crafted archive could still escape $MODS when extracted as
# root. Reject any path-traversal entry before extracting.
if cpio -t < "$WORK/modules.cpio" 2>/dev/null | grep -qE '(^|/)\.\.(/|$)'; then
    echo "refusing to extract $MODULES_FROM: archive contains '..' path components" >&2
    exit 1
fi
( cd "$MODS" && cpio -idm --no-absolute-filenames < "$WORK/modules.cpio" 2>/dev/null )
# Guard the dir before ls so a modules-less archive fails with this message
# instead of an opaque `ls` error under set -e.
[ -d "$MODS/lib/modules" ] || { echo "no /lib/modules in $MODULES_FROM" >&2; exit 1; }
# Require exactly one /lib/modules/<kver>: more than one means the source
# initramfs carries multiple kernels, and silently picking the first (head -1)
# could depmod/inject a set that mismatches the vmlinuz this profile boots with.
KCOUNT="$(ls "$MODS/lib/modules" | wc -l)"
[ "$KCOUNT" -eq 1 ] || { echo "expected exactly one /lib/modules/<kver> in $MODULES_FROM, found $KCOUNT: $(ls "$MODS/lib/modules" | tr '\n' ' ')" >&2; exit 1; }
KVER="$(ls "$MODS/lib/modules")"
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
# vsock is the core AF_VSOCK module the transport needs; virtio_rng feeds the
# guest entropy pool (without it a heavier systemd userland starves /dev/random
# and getrandom() callers block). These match dew's standard early-boot set.
cat > "$ROOT/etc/modules-load.d/dew.conf" <<EOF
vsock
vmw_vsock_virtio_transport
virtio_rng
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
# Split the find|cpio|gzip pipeline so a find/cpio failure can't be masked by
# gzip exiting 0 (dash has no pipefail), which would write a silently broken
# initramfs. Stage the file list and the cpio archive so set -e checks each step.
( cd "$ROOT" && find . > "$WORK/rootfs.files" )
( cd "$ROOT" && cpio -o -H newc 2>/dev/null < "$WORK/rootfs.files" ) > "$WORK/rootfs.cpio"
gzip -1 -c "$WORK/rootfs.cpio" > "$OUT"
echo "Profile:   systemd ($SUITE, $ARCH)"
echo "Kernel:    $KVER"
echo "Rootfs:    $(du -sh "$ROOT" | cut -f1) uncompressed"
echo "Output:    $OUT ($(du -h "$OUT" | cut -f1))"
