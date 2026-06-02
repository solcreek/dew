#!/bin/bash
set -uo pipefail

# Benchmark: Lima (cold/warm) vs Dew (Alpine/Turbo/Session)

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
echo "--- Sizes ---"
echo "dew binary:       $(ls -lh "$DEW" | awk '{print $5}')"
echo "initramfs:        $(ls -lh "$INITRD" | awk '{print $5}')"
echo "Alpine kernel:    $(ls -lh "$KERNEL_ALPINE" | awk '{print $5}')"
[ -f "$KERNEL_TURBO" ] && echo "Turbo kernel:     $(ls -lh "$KERNEL_TURBO" | awk '{print $5}')"
echo "Lima binary:      $(ls -lhL "$(which limactl)" 2>/dev/null | awk '{print $5}' || echo 'N/A')"
echo ""

# --- Lima warm ---
echo "--- Lima (warm, SSH on running VM) ---"
if limactl list 2>/dev/null | grep -q "Running"; then
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        limactl shell creek-sandbox -- cat /proc/uptime 2>&1 | grep -v mux_client | grep -v ControlSocket >/dev/null
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        echo "  Run $i: wall=$((T2-T1))ms"
    done
else
    echo "  VM not running, skipping"
fi
echo ""

# --- Lima cold ---
echo "--- Lima (cold boot → exec → stop) ---"
if which limactl >/dev/null 2>&1 && limactl list 2>/dev/null | grep -q creek-sandbox; then
    # Stop if running
    limactl stop creek-sandbox 2>/dev/null || true
    sleep 1
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        limactl start creek-sandbox 2>&1 >/dev/null
        limactl shell creek-sandbox -- cat /proc/uptime 2>&1 | grep -v mux_client | grep -v ControlSocket >/dev/null
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        echo "  Run $i: wall=$((T2-T1))ms"
        if [ $i -lt $RUNS ]; then
            limactl stop creek-sandbox 2>/dev/null || true
            sleep 1
        fi
    done
else
    echo "  Lima not available, skipping"
fi
echo ""

# --- Dew Alpine cold ---
echo "--- Dew Alpine (cold boot → exec → shutdown) ---"
if [ -f "$DEW" ] && [ -f "$KERNEL_ALPINE" ]; then
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        OUTPUT=$($DEW run --kernel "$KERNEL_ALPINE" --initrd "$INITRD" -- "cat /proc/uptime" 2>&1)
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        BOOT=$(echo "$OUTPUT" | grep "VM running" | grep -o '[0-9]*ms' || echo "?")
        echo "  Run $i: wall=$((T2-T1))ms  boot=$BOOT"
    done
fi
echo ""

# --- Dew Turbo cold ---
echo "--- Dew Turbo (cold boot → exec → shutdown) ---"
if [ -f "$DEW" ] && [ -f "$KERNEL_TURBO" ]; then
    for i in $(seq 1 $RUNS); do
        T1=$(python3 -c "import time; print(int(time.time()*1000))")
        OUTPUT=$($DEW run --kernel "$KERNEL_TURBO" --initrd "$INITRD" -- "cat /proc/uptime" 2>&1)
        T2=$(python3 -c "import time; print(int(time.time()*1000))")
        BOOT=$(echo "$OUTPUT" | grep "VM running" | grep -o '[0-9]*ms' || echo "?")
        echo "  Run $i: wall=$((T2-T1))ms  boot=$BOOT"
    done
else
    echo "  Turbo kernel not built, skipping"
fi
echo ""

echo "=== Done ==="
