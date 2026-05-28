#!/bin/bash
set -uo pipefail

# Smoke test for Dew — validates core functionality before release.
# Requires: built dew binary (make sign), initramfs profiles built.
#
# Usage: ./smoke-test.sh [kernel] [initramfs-dir]

DEW="$(cd "$(dirname "$0")" && pwd)/dew"
KERNEL="${1:-$(dirname "$0")/initramfs/vmlinuz}"
INITRD_DIR="${2:-$(dirname "$0")/initramfs}"
PASS=0
FAIL=0
SKIP=0

cleanup() {
    kill $(jobs -p) 2>/dev/null
    rm -f ~/.local/state/dew/default.sock
    rm -f /tmp/dew-smoke-*.img
}
trap cleanup EXIT

test_result() {
    local name="$1" result="$2"
    if [ "$result" = "pass" ]; then
        echo "  ✓ $name"
        PASS=$((PASS+1))
    elif [ "$result" = "skip" ]; then
        echo "  - $name (skipped)"
        SKIP=$((SKIP+1))
    else
        echo "  ✗ $name"
        FAIL=$((FAIL+1))
    fi
}

echo "=== Dew Smoke Test ==="
echo ""

# --- Test 1: Binary runs ---
VER=$($DEW version 2>&1)
if echo "$VER" | grep -q "dew"; then
    test_result "binary runs ($VER)" "pass"
else
    test_result "binary runs" "fail"
fi

# --- Test 2: Minimal profile boot + exec ---
INITRD_MIN="$INITRD_DIR/initramfs-minimal.cpio.gz"
if [ -f "$INITRD_MIN" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock
    $DEW start --profile minimal --kernel "$KERNEL" --initrd "$INITRD_MIN" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$($DEW exec "echo smoke-ok" 2>&1)
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
    if [ "$RESULT" = "smoke-ok" ]; then
        test_result "minimal: boot + exec" "pass"
    else
        test_result "minimal: boot + exec" "fail"
    fi
else
    test_result "minimal: boot + exec" "skip"
fi

# --- Test 3: Port forwarding ---
INITRD_MIN="$INITRD_DIR/initramfs-minimal.cpio.gz"
if [ -f "$INITRD_MIN" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock
    $DEW start --profile minimal --kernel "$KERNEL" --initrd "$INITRD_MIN" \
        --network --forward 19876:9876 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    # Start a simple listener inside VM
    $DEW exec "echo 'HTTP/1.0 200 OK\r\n\r\nsmoke-port' | nc -l -p 9876 &" 2>/dev/null
    sleep 1
    RESULT=$(curl -s --max-time 3 http://localhost:19876/ 2>/dev/null)
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
    if echo "$RESULT" | grep -q "smoke-port"; then
        test_result "port forwarding (vsock proxy)" "pass"
    else
        test_result "port forwarding (vsock proxy)" "fail"
    fi
else
    test_result "port forwarding" "skip"
fi

# --- Test 4: Network isolation (no NIC) ---
INITRD_MIN="$INITRD_DIR/initramfs-minimal.cpio.gz"
if [ -f "$INITRD_MIN" ] && [ -f "$KERNEL" ]; then
    RESULT=$($DEW run --kernel "$KERNEL" --initrd "$INITRD_MIN" \
        -- "ip link show eth0 2>&1" 2>/dev/null)
    if echo "$RESULT" | grep -q "does not exist\|not found\|No such"; then
        test_result "network isolation (no NIC)" "pass"
    elif [ -z "$RESULT" ]; then
        test_result "network isolation (no NIC)" "pass"
    else
        test_result "network isolation (no NIC)" "fail"
    fi
else
    test_result "network isolation" "skip"
fi

# --- Test 5: Node profile (second boot = no segfault) ---
INITRD_NODE="$INITRD_DIR/initramfs-node.cpio.gz"
if [ -f "$INITRD_NODE" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock ~/.local/share/dew/node.img
    # First boot
    $DEW start --profile node --kernel "$KERNEL" --initrd "$INITRD_NODE" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 120); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    $DEW exec "node --version" 2>/dev/null
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock; sleep 1
    # Second boot
    $DEW start --profile node --kernel "$KERNEL" --initrd "$INITRD_NODE" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$($DEW exec "node -e 'console.log(\"node-ok\")'" 2>&1)
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
    if [ "$RESULT" = "node-ok" ]; then
        test_result "node: second boot (no segfault)" "pass"
    else
        test_result "node: second boot (no segfault)" "fail"
    fi
else
    test_result "node: second boot" "skip"
fi

# --- Test 6: Standard profile + containerd ---
INITRD_STD="$INITRD_DIR/initramfs-standard.cpio.gz"
if [ -f "$INITRD_STD" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock /tmp/dew-smoke-std.img
    $DEW start --kernel "$KERNEL" --initrd "$INITRD_STD" \
        --network --memory 2048 --disk /tmp/dew-smoke-std.img 2>/dev/null &
    PID=$!
    for i in $(seq 1 120); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$($DEW exec "containerd --version 2>&1 | head -1" 2>&1)
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
    if echo "$RESULT" | grep -q "containerd"; then
        test_result "standard: containerd running" "pass"
    else
        test_result "standard: containerd running" "fail"
    fi
else
    test_result "standard: containerd" "skip"
fi

# --- Test 7: Detect (unit-level) ---
GO_TEST=$(cd "$(dirname "$0")" && go test ./internal/detect/ -count=1 2>&1 | tail -1)
if echo "$GO_TEST" | grep -q "ok"; then
    test_result "detect: unit tests" "pass"
else
    test_result "detect: unit tests" "fail"
fi

# --- Test 8: All unit tests ---
GO_TEST_ALL=$(cd "$(dirname "$0")" && go test ./... -count=1 2>&1)
PASS_COUNT=$(echo "$GO_TEST_ALL" | grep -c "ok ")
TOTAL_PKG=8
if [ "$PASS_COUNT" -ge "$TOTAL_PKG" ]; then
    test_result "all unit tests ($PASS_COUNT/$TOTAL_PKG packages)" "pass"
else
    test_result "all unit tests ($PASS_COUNT/$TOTAL_PKG packages)" "fail"
fi

echo ""
echo "=== Results ==="
echo "  Pass: $PASS"
echo "  Fail: $FAIL"
echo "  Skip: $SKIP"
echo ""

if [ $FAIL -gt 0 ]; then
    exit 1
fi
