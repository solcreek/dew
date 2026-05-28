#!/bin/bash
set -euo pipefail

# Build a monolithic x86_64 kernel for Dew VM.
# Modeled on Apple's containerization kernel config:
# - All containerd features built-in (overlay, bridge, veth, iptables, cgroups)
# - All virtio drivers built-in (blk, net, console, fs, pci, vsock)
# - CONFIG_MODULES off (monolithic, no module loading)
# - Minimal drivers (no bare-metal, no cloud-provider-specific)
#
# Usage:
#   ./build-dew-kernel.sh           # build locally (requires Linux or Lima)
#   # Or use kernel/Dockerfile for CI

KERNEL_VERSION="${KERNEL_VERSION:-6.12.91}"
KERNEL_SERIES="${KERNEL_VERSION%.*}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORK="${SCRIPT_DIR}/build"
OUT="${SCRIPT_DIR}/vmlinuz-dew"

echo "=== Building Dew kernel ${KERNEL_VERSION} (x86_64, monolithic) ==="

mkdir -p "$WORK"
cd "$WORK"

if [ ! -d "linux-${KERNEL_VERSION}" ]; then
    echo "Downloading kernel source..."
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_SERIES%%.*}.x/linux-${KERNEL_VERSION}.tar.xz" \
        | tar xJ
fi

cd "linux-${KERNEL_VERSION}"

echo "Generating config..."
# Start from x86_64 defconfig
make x86_64_defconfig > /dev/null 2>&1

# Apply Dew overrides
cat >> .config << 'DEW_CONFIG'

# === Dew VM kernel config overrides ===
# Monolithic: no modules
# CONFIG_MODULES is not set
# CONFIG_MODULE_SIG is not set
# CONFIG_MODVERSIONS is not set

# === Virtio (Apple VZ device model) ===
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_PCI_LEGACY=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_VIRTIO_FS=y
CONFIG_VIRTIO_MMIO=y
CONFIG_VIRTIO_BALLOON=y
CONFIG_VIRTIO_INPUT=y

# === vsock ===
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS_COMMON=y
CONFIG_VSOCKETS_LOOPBACK=y

# === Filesystem ===
CONFIG_EXT4_FS=y
CONFIG_EXT4_FS_POSIX_ACL=y
CONFIG_EXT4_FS_SECURITY=y
CONFIG_OVERLAY_FS=y
CONFIG_FUSE_FS=y

# === Networking (containerd CNI) ===
CONFIG_BRIDGE=y
CONFIG_BRIDGE_NETFILTER=y
CONFIG_BRIDGE_NF_EBTABLES=y
CONFIG_VETH=y
CONFIG_PACKET=y
CONFIG_NET_NS=y

# === Netfilter / iptables (CNI bridge plugin) ===
CONFIG_NETFILTER=y
CONFIG_NETFILTER_ADVANCED=y
CONFIG_NETFILTER_XTABLES=y
CONFIG_NETFILTER_XT_MATCH_ADDRTYPE=y
CONFIG_NETFILTER_XT_MATCH_COMMENT=y
CONFIG_NETFILTER_XT_MATCH_CONNTRACK=y
CONFIG_NETFILTER_XT_MATCH_MULTIPORT=y
CONFIG_NETFILTER_XT_MATCH_STATE=y
CONFIG_NETFILTER_XT_NAT=y
CONFIG_NETFILTER_XT_TARGET_MASQUERADE=y
CONFIG_NETFILTER_XT_TARGET_REDIRECT=y
CONFIG_NF_CONNTRACK=y
CONFIG_NF_NAT=y
CONFIG_NF_NAT_MASQUERADE=y
CONFIG_NF_NAT_REDIRECT=y
CONFIG_NF_DEFRAG_IPV4=y
CONFIG_NF_TABLES=y
CONFIG_IP_NF_IPTABLES=y
CONFIG_IP_NF_FILTER=y
CONFIG_IP_NF_NAT=y
CONFIG_IP_NF_TARGET_MASQUERADE=y
CONFIG_IP_NF_TARGET_REDIRECT=y
CONFIG_IP_NF_TARGET_REJECT=y

# === Cgroups (containerd resource limits) ===
CONFIG_CGROUPS=y
CONFIG_CGROUP_SCHED=y
CONFIG_CGROUP_PIDS=y
CONFIG_CGROUP_FREEZER=y
CONFIG_CGROUP_DEVICE=y
CONFIG_CGROUP_CPUACCT=y
CONFIG_CGROUP_PERF=y
CONFIG_CGROUP_BPF=y
CONFIG_BLK_CGROUP=y
CONFIG_MEMCG=y

# === Namespaces (container isolation) ===
CONFIG_NAMESPACES=y
CONFIG_PID_NS=y
CONFIG_USER_NS=y
CONFIG_UTS_NS=y
CONFIG_IPC_NS=y
CONFIG_NET_NS=y
CONFIG_TIME_NS=y

# === Misc required ===
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_TMPFS=y
CONFIG_PROC_FS=y
CONFIG_SYSFS=y
CONFIG_UNIX=y
CONFIG_INET=y
CONFIG_IPV6=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y
CONFIG_PRINTK=y
CONFIG_BUG=y
CONFIG_SHMEM=y
CONFIG_AIO=y
CONFIG_EPOLL=y
CONFIG_SIGNALFD=y
CONFIG_TIMERFD=y
CONFIG_EVENTFD=y
CONFIG_INOTIFY_USER=y
DEW_CONFIG

# Resolve config with defaults for new symbols
make olddefconfig > /dev/null 2>&1

echo "Building..."
make -j"$(nproc)" bzImage 2>&1 | tail -3

cp arch/x86/boot/bzImage "$OUT"

echo ""
echo "=== Done ==="
echo "Kernel: $(ls -lh "$OUT" | awk '{print $5}') $OUT"
echo "Modules: none (monolithic)"
