#!/bin/bash
set -uo pipefail

# Benchmark: Lima vs Dew (Alpine) vs Dew (Turbo)
# Measures: boot time, exec latency, total time, binary/image sizes

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEW="$SCRIPT_DIR/dew"
INITRD="$SCRIPT_DIR/initramfs/initramfs.cpio.gz"
KERNEL_ALPINE="$SCRIPT_DIR/initramfs/vmlinuz"
KERNEL_TURBO="$SCRIPT_DIR/kernel/vmlinuz-turbo"
RUNS=3

echo "=== Lima vs Dew Benchmark ==="
echo "Date: $(date -u)"
echo "Runs: $RUNS per target"
echo ""

# --- Sizes ---
echo "--- Binary / Image Sizes ---"
echo "dew binary:       $(ls -lh "$DEW" | awk '{print $5}')"
echo "dew-agent:        $(ls -lh "$SCRIPT_DIR/dew-agent-linux-amd64" 2>/dev/null | awk '{print $5}' || echo 'N/A')"
echo "initramfs:        $(ls -lh "$INITRD" | awk '{print $5}')"
echo "Alpine kernel:    $(ls -lh "$KERNEL_ALPINE" | awk '{print $5}')"
[ -f "$KERNEL_TURBO" ] && echo "Turbo kernel:     $(ls -lh "$KERNEL_TURBO" | awk '{print $5}')"
echo "Lima binary:      $(ls -lhL "$(which limactl)" | awk '{print $5}')"
echo "Lima VM disk:     $(du -sh ~/.lima/creek-sandbox/diffdisk 2>/dev/null | awk '{print $1}' || echo 'N/A')"
echo ""

# --- Lima ---
echo "--- Lima ---"
if limactl list 2>/dev/null | grep -q creek-sandbox; then
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        OUTPUT=$(limactl shell creek-sandbox -- bash -c 'cat /proc/uptime && uname -r' 2>&1 | grep -v mux_client | grep -v ControlSocket)
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        WALL=$((T2 - T1))
        UPTIME=$(echo "$OUTPUT" | head -1 | awk '{print $1}')
        KVER=$(echo "$OUTPUT" | tail -1)
        echo "  Run $i: wall=${WALL}ms  guest_uptime=${UPTIME}s  kernel=$KVER"
    done
    echo "  Note: Lima VM already running (no cold boot measured)"
else
    echo "  Lima VM not running, skipping"
fi
echo ""

# --- Dew Alpine ---
echo "--- Dew (Alpine official kernel) ---"
if [ -f "$DEW" ] && [ -f "$KERNEL_ALPINE" ]; then
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        OUTPUT=$($DEW run --kernel "$KERNEL_ALPINE" --initrd "$INITRD" -- "cat /proc/uptime && uname -r" 2>&1)
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        WALL=$((T2 - T1))
        BOOT=$(echo "$OUTPUT" | grep "VM running" | grep -o '[0-9]*ms' || echo "?")
        UPTIME=$(echo "$OUTPUT" | grep -E '^[0-9]+\.[0-9]' | head -1 | awk '{print $1}')
        KVER=$(echo "$OUTPUT" | grep -E '^[0-9]+\.' -v | grep -v "dew:" | tail -1)
        echo "  Run $i: wall=${WALL}ms  boot=${BOOT}  guest_uptime=${UPTIME}s  kernel=$KVER"
    done
else
    echo "  Not available"
fi
echo ""

# --- Dew Turbo ---
echo "--- Dew (Turbo kernel, vsock built-in) ---"
if [ -f "$DEW" ] && [ -f "$KERNEL_TURBO" ]; then
    # Turbo doesn't need modprobe, build initramfs without modules
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        OUTPUT=$($DEW run --kernel "$KERNEL_TURBO" --initrd "$INITRD" -- "cat /proc/uptime && uname -r" 2>&1)
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        WALL=$((T2 - T1))
        BOOT=$(echo "$OUTPUT" | grep "VM running" | grep -o '[0-9]*ms' || echo "?")
        UPTIME=$(echo "$OUTPUT" | grep -E '^[0-9]+\.[0-9]' | head -1 | awk '{print $1}')
        KVER=$(echo "$OUTPUT" | grep -E '^[0-9]+\.' -v | grep -v "dew:" | tail -1)
        echo "  Run $i: wall=${WALL}ms  boot=${BOOT}  guest_uptime=${UPTIME}s  kernel=$KVER"
    done
else
    echo "  Turbo kernel not built yet, skipping"
fi
echo ""

echo "=== Done ==="
