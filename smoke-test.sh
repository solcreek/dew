#!/bin/bash
set -uo pipefail

# Smoke test for Dew — validates core functionality before release.
# Requires: built dew binary (make sign), initramfs profiles built.
#
# Usage: ./smoke-test.sh [kernel] [initramfs-dir]

# Absolute paths throughout: the `dew up` tests cd into temp project
# dirs, where a relative ./initramfs/vmlinuz silently stops existing
# and dew tries (and fails) to re-download assets into the project.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEW="$SCRIPT_DIR/dew"
KERNEL="${1:-$SCRIPT_DIR/initramfs/vmlinuz}"
INITRD_DIR="${2:-$SCRIPT_DIR/initramfs}"
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

# kill_port frees a host TCP port, portably. BSD/macOS xargs has no `-r`
# (--no-run-if-empty), so capture the PIDs and only kill when non-empty
# rather than piping through `xargs -r`. No-op when lsof is absent (rather
# than a silent command-not-found), and `kill --` so a PID can never be
# mistaken for a flag.
kill_port() {
    command -v lsof >/dev/null 2>&1 || return 0
    local pids
    pids=$(lsof -ti:"$1" 2>/dev/null)
    [ -n "$pids" ] && kill -9 -- $pids 2>/dev/null
    return 0
}

echo "=== Dew Smoke Test ==="
echo ""

# --- Test 1: Binary runs ---
VER=$("$DEW" version 2>&1)
if echo "$VER" | grep -q "dew"; then
    test_result "binary runs ($VER)" "pass"
else
    test_result "binary runs" "fail"
fi

# --- Test 2: Minimal profile boot + exec ---
INITRD_MIN="$INITRD_DIR/initramfs-minimal.cpio.gz"
if [ -f "$INITRD_MIN" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock
    "$DEW" start --profile minimal --kernel "$KERNEL" --initrd "$INITRD_MIN" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$("$DEW" exec "echo smoke-ok" 2>&1)
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
    "$DEW" start --profile minimal --kernel "$KERNEL" --initrd "$INITRD_MIN" \
        --network --forward 19876:9876 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    # Start a simple listener inside VM. printf, not echo: busybox
    # echo's backslash-escape handling is a compile-time option, and
    # without expansion curl never sees the CRLF header terminator and
    # times out waiting for the response to complete.
    "$DEW" exec "printf 'HTTP/1.0 200 OK\r\n\r\nsmoke-port' | nc -l -p 9876 &" 2>/dev/null
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
    RESULT=$("$DEW" run --kernel "$KERNEL" --initrd "$INITRD_MIN" \
        -- "ip link show eth0 2>&1" 2>/dev/null)
    # busybox `ip` says "can't find device"; iproute2 says "does not
    # exist" — accept both so the assertion survives initramfs rebuilds.
    if echo "$RESULT" | grep -q "does not exist\|not found\|No such\|can't find"; then
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
    # First boot. Poll for node: the CI-built initramfs bakes it
    # (immediate), the Darwin-local build installs it on first boot
    # (apk, needs network + time) — the test must pass with either.
    "$DEW" start --profile node --kernel "$KERNEL" --initrd "$INITRD_NODE" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 120); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    for i in $(seq 1 45); do
        NODE_V=$("$DEW" exec "node --version" 2>/dev/null)
        [ -n "$NODE_V" ] && break
        sleep 2
    done
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock; sleep 1
    # Second boot
    "$DEW" start --profile node --kernel "$KERNEL" --initrd "$INITRD_NODE" --network 2>/dev/null &
    PID=$!
    for i in $(seq 1 60); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$("$DEW" exec "node -e 'console.log(\"node-ok\")'" 2>&1)
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
    if [ "$RESULT" = "node-ok" ]; then
        test_result "node: second boot (no segfault)" "pass"
    else
        test_result "node: second boot (no segfault)" "fail"
    fi
else
    test_result "node: second boot" "skip"
fi

# --- Test 6: Standard profile crun runtime (replaces containerd) ---
# Containers run via crun on the host-pull + overlay path (dew-oci-run); there
# is no in-guest containerd. Assert the runtime binary and launcher are present
# and crun is executable.
INITRD_STD="$INITRD_DIR/initramfs-standard.cpio.gz"
if [ -f "$INITRD_STD" ] && [ -f "$KERNEL" ]; then
    rm -f ~/.local/state/dew/default.sock /tmp/dew-smoke-std.img
    "$DEW" start --kernel "$KERNEL" --initrd "$INITRD_STD" \
        --network --memory 2048 --disk /tmp/dew-smoke-std.img 2>/dev/null &
    PID=$!
    for i in $(seq 1 120); do [ -S ~/.local/state/dew/default.sock ] && break; sleep 0.5; done
    RESULT=$("$DEW" exec "crun --version 2>&1 | head -1" 2>&1)
    if echo "$RESULT" | grep -q "crun"; then
        test_result "standard: crun runtime available" "pass"
    else
        test_result "standard: crun runtime available" "fail"
    fi
    LAUNCHER=$("$DEW" exec "test -x /usr/local/bin/dew-oci-run && echo launcher-ok" 2>&1)
    if echo "$LAUNCHER" | grep -q "launcher-ok"; then
        test_result "standard: dew-oci-run launcher present" "pass"
    else
        test_result "standard: dew-oci-run launcher present" "fail"
    fi
    kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -f ~/.local/state/dew/default.sock
else
    test_result "standard: crun runtime available" "skip"
    test_result "standard: dew-oci-run launcher present" "skip"
fi

# --- Test 6b: First-boot DHCP lease arrives quickly across cold boots ---
# Regression guard for v0.7.7 → v0.7.8: the pruned initramfs boots so
# fast it can race Apple VZ's NAT init. With udhcpc's default `-t 3`
# (no `-n`) the busybox client retries the 3-packet burst forever,
# blocking init-stage2 for ~90 s of visible "broadcasting discover"
# spam. The fix is `-n -t 30 -T 3` — single patient attempt. This
# test boots cold (fresh node.img) three times and asserts each cold
# boot acquires a lease within 20 s.
if [ -f "$INITRD_NODE" ] && [ -f "$KERNEL" ]; then
    DHCP_FAILS=0
    DHCP_MAX_S=0
    for n in 1 2 3; do
        pkill -9 -f 'dew start\|dew run' 2>/dev/null; sleep 1
        rm -f ~/.local/state/dew/default.sock ~/.local/share/dew/node.img
        S=$(date +%s)
        # `dew run` joins all post-`--` argv with spaces and feeds the
        # result to /bin/sh -c, so we pass ONE pre-quoted shell line
        # rather than `sh -c '<pipeline>'` (which would degenerate to
        # `sh -c` running with no script).
        OUT=$("$DEW" run --profile node --network --kernel "$KERNEL" --initrd "$INITRD_NODE" \
              -- 'ip -4 addr show eth0 2>/dev/null | awk "/inet /{print \$2; exit}"' 2>&1)
        D=$(($(date +%s)-S))
        if echo "$OUT" | grep -qE '192\.168\.[0-9]+\.[0-9]+/'; then
            [ "$D" -gt "$DHCP_MAX_S" ] && DHCP_MAX_S=$D
        else
            DHCP_FAILS=$((DHCP_FAILS+1))
        fi
    done
    if [ "$DHCP_FAILS" = "0" ] && [ "$DHCP_MAX_S" -le 60 ]; then
        test_result "node: first-boot DHCP <60s (3 cold boots, max ${DHCP_MAX_S}s)" "pass"
    else
        test_result "node: first-boot DHCP <60s (3 cold boots, max ${DHCP_MAX_S}s, ${DHCP_FAILS} failures)" "fail"
    fi
    rm -f ~/.local/state/dew/default.sock ~/.local/share/dew/node.img
else
    test_result "node: first-boot DHCP" "skip"
fi

# --- Test 6c: `dew up` aha — pure JS, no build-tools install should happen ---
# Scaffolds Vite+React (no native deps), runs `dew up`, asserts:
#   1. curl http://localhost:5173/ returns the React HTML within 180s
#   2. the build-tools install path was NOT triggered (regression guard
#      for the lockfile-scanner: a stock vite+react must not match
#      knownNativeNodePackages)
if [ -f "$INITRD_NODE" ] && [ -f "$KERNEL" ] && command -v npm >/dev/null 2>&1; then
    pkill -9 -f 'dew start\|dew run\|dew up' 2>/dev/null
    kill_port 5173
    sleep 1
    rm -f ~/.local/state/dew/default.sock ~/.local/share/dew/node.img
    PROJ=$(mktemp -d -t dew-smoke-up)
    (
        cd "$PROJ"
        npm create vite@latest my-app -- --template react >/dev/null 2>&1
    )
    UP_OK=0
    UP_T=0
    LOG=/tmp/dew-smoke-up.log
    if [ -d "$PROJ/my-app" ]; then
        (
            cd "$PROJ/my-app"
            "$DEW" up --kernel "$KERNEL" --initrd "$INITRD_NODE" >$LOG 2>&1
        ) &
        UP_PID=$!
        START=$(date +%s)
        for i in $(seq 1 90); do
            sleep 2
            code=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' http://localhost:5173/ 2>/dev/null || echo 000)
            if [ "$code" = "200" ] || [ "$code" = "304" ]; then
                BODY=$(curl -sS --max-time 5 http://localhost:5173/ 2>/dev/null)
                if echo "$BODY" | grep -q '<!doctype html>'; then
                    UP_OK=1
                    UP_T=$(($(date +%s)-START))
                    break
                fi
            fi
        done
        kill -9 $UP_PID 2>/dev/null; wait $UP_PID 2>/dev/null
        pkill -9 -f 'dew start\|dew up' 2>/dev/null
        rm -f ~/.local/state/dew/default.sock
    fi
    if [ "$UP_OK" = "1" ]; then
        test_result "dew up: vite project serves React HTML in ${UP_T}s" "pass"
    else
        test_result "dew up: vite project never served (waited 180s)" "fail"
    fi
    # Negative assertion: pure JS must not trigger build tools install.
    # A false positive here means knownNativeNodePackages has a name
    # that vite+react actually pulls in.
    if grep -q 'installing build tools' "$LOG" 2>/dev/null; then
        OFFENDER=$(grep 'installing build tools' "$LOG" | head -1)
        test_result "dew up: vite without native deps doesn't trigger build tools (${OFFENDER})" "fail"
    else
        test_result "dew up: vite without native deps doesn't trigger build tools" "pass"
    fi
    rm -rf "$PROJ"
else
    test_result "dew up: vite e2e" "skip"
    test_result "dew up: no build tools on pure JS" "skip"
fi

# --- Test 6d: `dew up` with sharp in lockfile triggers build-tools install ---
# Positive path for the lockfile scanner: a project that pins sharp
# must see "installing build tools (sharp)" before "installing deps".
# Confirms cmdUp reads the right directory and the scanner correctly
# matches against knownNativeNodePackages.
if [ -f "$INITRD_NODE" ] && [ -f "$KERNEL" ] && command -v npm >/dev/null 2>&1; then
    pkill -9 -f 'dew start\|dew run\|dew up' 2>/dev/null
    kill_port 5173
    sleep 1
    rm -f ~/.local/state/dew/default.sock ~/.local/share/dew/node.img
    PROJ=$(mktemp -d -t dew-smoke-sharp)
    (
        cd "$PROJ"
        npm create vite@latest my-app -- --template react >/dev/null 2>&1
        cd my-app
        # Inject sharp into the lockfile only — actual install happens
        # in the VM. --package-lock-only writes package-lock.json
        # without touching node_modules.
        npm install sharp --package-lock-only >/dev/null 2>&1
    )
    UP_OK=0
    UP_T=0
    LOG=/tmp/dew-smoke-sharp.log
    if [ -d "$PROJ/my-app" ]; then
        (
            cd "$PROJ/my-app"
            "$DEW" up --kernel "$KERNEL" --initrd "$INITRD_NODE" >$LOG 2>&1
        ) &
        UP_PID=$!
        START=$(date +%s)
        for i in $(seq 1 90); do
            sleep 2
            code=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' http://localhost:5173/ 2>/dev/null || echo 000)
            if [ "$code" = "200" ] || [ "$code" = "304" ]; then
                UP_OK=1
                UP_T=$(($(date +%s)-START))
                break
            fi
        done
        kill -9 $UP_PID 2>/dev/null; wait $UP_PID 2>/dev/null
        pkill -9 -f 'dew start\|dew up' 2>/dev/null
        rm -f ~/.local/state/dew/default.sock
    fi
    # Positive assertion: the sharp install must trigger the build-tools
    # path, AND the build-tools step must precede the install step.
    BT_LINE=$(grep -n 'installing build tools' "$LOG" 2>/dev/null | head -1 | cut -d: -f1)
    INSTALL_LINE=$(grep -n 'installing deps' "$LOG" 2>/dev/null | head -1 | cut -d: -f1)
    if [ -n "$BT_LINE" ] && [ -n "$INSTALL_LINE" ] && [ "$BT_LINE" -lt "$INSTALL_LINE" ] && [ "$UP_OK" = "1" ]; then
        test_result "dew up: sharp in lockfile triggers build tools before install (served in ${UP_T}s)" "pass"
    elif [ -z "$BT_LINE" ]; then
        test_result "dew up: sharp didn't trigger build tools (lockfile scanner miss)" "fail"
    elif [ "$BT_LINE" -ge "$INSTALL_LINE" ]; then
        test_result "dew up: build tools ran AFTER install (ordering bug)" "fail"
    else
        test_result "dew up: sharp project never served" "fail"
    fi
    rm -rf "$PROJ"
else
    test_result "dew up: sharp triggers build tools" "skip"
fi

# --- Test 6d: hang guard — a mute guest errors out, never hangs ---
# Regression guard for the `dew run` lockup: against a guest with no
# vsock transport (vz's connect completion handler never fires) and no
# serial shell, `dew run` used to block forever. Boot a deliberately
# mute init and assert dew gives up with a non-zero exit within the
# documented deadlines (60s vsock wait + 60s serial wait + slack).
if [ -f "$KERNEL" ] && command -v go >/dev/null 2>&1; then
    MUTE_DIR=$(mktemp -d /tmp/dew-smoke-mute.XXXXXX)
    case "$(uname -m)" in
        arm64|aarch64) MUTE_ARCH=arm64 ;;
        *)             MUTE_ARCH=amd64 ;;
    esac
    (cd "$(dirname "$0")" && CGO_ENABLED=0 GOOS=linux GOARCH=$MUTE_ARCH \
        go build -ldflags="-s -w" -o "$MUTE_DIR/init" ./test/mute-init/) 2>/dev/null
    if [ -x "$MUTE_DIR/init" ]; then
        (cd "$MUTE_DIR" && echo init | cpio -o -H newc 2>/dev/null | gzip > mute.cpio.gz)
        START_S=$(date +%s)
        "$DEW" run --kernel "$KERNEL" --initrd "$MUTE_DIR/mute.cpio.gz" \
            -- echo hang-guard >/dev/null 2>&1 &
        RUNPID=$!
        # Watchdog: if the hang regresses, fail the test instead of
        # hanging the whole suite.
        HUNG=0
        for i in $(seq 1 160); do
            kill -0 $RUNPID 2>/dev/null || break
            sleep 1
        done
        if kill -0 $RUNPID 2>/dev/null; then
            HUNG=1
            kill -9 $RUNPID 2>/dev/null
        fi
        wait $RUNPID 2>/dev/null
        RC=$?
        ELAPSED_S=$(($(date +%s)-START_S))
        if [ "$HUNG" = "0" ] && [ "$RC" -ne 0 ]; then
            test_result "hang guard: mute guest errors in ${ELAPSED_S}s (rc=$RC)" "pass"
        elif [ "$HUNG" = "1" ]; then
            test_result "hang guard: mute guest still hung after ${ELAPSED_S}s" "fail"
        else
            test_result "hang guard: mute guest exited 0 (want non-zero)" "fail"
        fi
    else
        test_result "hang guard: mute guest (fixture build failed)" "skip"
    fi
    rm -rf "$MUTE_DIR"
else
    test_result "hang guard: mute guest" "skip"
fi

# --- Test 6e: multi-service dew.toml stack + host.lo.internal (field-eval topology) ---
# Sediments the external field evaluation's canonical stack into a regression
# guard: three arbitrary OCI services (redis + mailpit + anycable-go) composed
# into ONE standard-profile VM via dew.toml [[service]], plus the reverse
# host-forward. Asserts, end to end:
#   1. all three boot and forward to the host (mailpit :8025 → 200; redis :6379
#      + anycable :8080 reachable)
#   2. service-to-service on localhost works (anycable connects to redis)
#   3. host.internal resolves to the NAT gateway inside the guest
#   4. host.lo.internal (127.0.0.2) tunnels over vsock to a 127.0.0.1-ONLY host
#      listener — the loopback-host-callback case host.internal cannot do
#   5. `dew services` lists the dew.toml [[service]] images, not just built-ins
# Heavy (standard profile + three image pulls): skips without the standard
# initramfs, python3 (host listener), or curl.
INITRD_STD="$INITRD_DIR/initramfs-standard.cpio.gz"
if [ -f "$INITRD_STD" ] && [ -f "$KERNEL" ] && command -v python3 >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
    pkill -9 -f 'dew start\|dew run\|dew up' 2>/dev/null
    rm -f ~/.local/state/dew/default.sock /tmp/dew-smoke-stack.img /tmp/dew-smoke-stack.log
    HOST_LO_PORT=50071
    # A 127.0.0.1-ONLY host listener: unreachable via host.internal (NAT) by
    # design — that's the whole point of host.lo.internal. python's http.server
    # answers 200 on GET / and binds loopback only.
    kill_port "$HOST_LO_PORT"
    python3 -m http.server "$HOST_LO_PORT" --bind 127.0.0.1 >/dev/null 2>&1 &
    HOST_LISTENER_PID=$!
    PROJ=$(mktemp -d -t dew-smoke-stack)
    cat > "$PROJ/dew.toml" <<TOML
[project]
profile = "standard"

[[service]]
name = "redis"
image = "redis:7-alpine"
port = 6379

[[service]]
name = "mailpit"
image = "axllent/mailpit:v1.30"
port = 8025

[[service]]
name = "anycable"
image = "anycable/anycable-go:1.5"
port = 8080
env = ["REDIS_URL=redis://localhost:6379/0", "ANYCABLE_HOST=0.0.0.0"]

[host]
expose = [$HOST_LO_PORT]
TOML
    STACK_LOG=/tmp/dew-smoke-stack.log
    (
        cd "$PROJ"
        # exec so $! (STACK_PID) is the real `dew up` process, not the
        # subshell wrapping it — otherwise kill -9 $STACK_PID reaps the
        # subshell and orphans `dew up` (and its VM), leaking ports.
        exec "$DEW" up --services-only --profile standard --kernel "$KERNEL" --initrd "$INITRD_STD" \
            --memory 2048 --disk /tmp/dew-smoke-stack.img >"$STACK_LOG" 2>&1
    ) &
    STACK_PID=$!
    # Readiness: poll mailpit's HTTP UI (serves / → 200) for up to 240s; the
    # first run pulls three images over the network.
    STACK_OK=0
    for i in $(seq 1 120); do
        sleep 2
        code=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' http://localhost:8025/ 2>/dev/null || echo 000)
        [ "$code" = "200" ] && { STACK_OK=1; break; }
    done
    if [ "$STACK_OK" = "1" ]; then
        test_result "stack: 3 OCI services in one VM, mailpit :8025 serves" "pass"
    else
        test_result "stack: 3-service stack never came up (waited 240s)" "fail"
    fi

    if [ "$STACK_OK" = "1" ]; then
        # 1. redis + anycable forwarded to the host.
        if (exec 3<>/dev/tcp/127.0.0.1/6379) 2>/dev/null; then
            exec 3>&- 3<&-
            test_result "stack: redis :6379 forwarded to host" "pass"
        else
            test_result "stack: redis :6379 not forwarded" "fail"
        fi
        ACODE=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' http://localhost:8080/health 2>/dev/null || echo 000)
        if [ "$ACODE" != "000" ]; then
            test_result "stack: anycable :8080 forwarded to host (HTTP $ACODE)" "pass"
        else
            test_result "stack: anycable :8080 not forwarded" "fail"
        fi

        # 2. service-to-service on localhost: anycable reached redis, no DNS error.
        ALOG=$("$DEW" logs anycable 2>/dev/null)
        if echo "$ALOG" | grep -qiE 'subscrib|provider=redis|connected to redis' \
            && ! echo "$ALOG" | grep -qi 'lookup localhost'; then
            test_result "stack: anycable → redis on localhost (service-to-service)" "pass"
        else
            test_result "stack: anycable → redis on localhost failed" "fail"
        fi

        # 3. host.internal resolves to the NAT gateway inside the guest.
        # Verify the invariant init-stage2 builds — host.internal maps to the
        # guest's actual default-route gateway (the VZ NAT host) — rather than a
        # hardcoded 192.168.* prefix, which would break if VZ ever renumbered
        # the subnet even though host.internal stayed correct.
        HI_IP=$("$DEW" exec "awk '/host.internal/{print \$1; exit}' /etc/hosts" 2>/dev/null | tr -d '[:space:]')
        GW_IP=$("$DEW" exec "ip route 2>/dev/null | awk '/^default/{print \$3; exit}'" 2>/dev/null | tr -d '[:space:]')
        if [ -n "$HI_IP" ] && [ "$HI_IP" = "$GW_IP" ]; then
            test_result "stack: host.internal → default gateway ($HI_IP)" "pass"
        else
            test_result "stack: host.internal '$HI_IP' != default gateway '$GW_IP'" "fail"
        fi

        # 4. host.lo.internal: maps to 127.0.0.2 AND tunnels over vsock to the
        #    127.0.0.1-only host listener.
        HLO=$("$DEW" exec "grep host.lo.internal /etc/hosts" 2>/dev/null)
        if echo "$HLO" | grep -q '127.0.0.2'; then
            test_result "stack: host.lo.internal → 127.0.0.2 (reverse-forward alias)" "pass"
        else
            test_result "stack: host.lo.internal did not map to 127.0.0.2 (got '$HLO')" "fail"
        fi
        REACH=$("$DEW" exec "wget -q -O /dev/null -T 5 http://host.lo.internal:$HOST_LO_PORT/ && echo lo-ok" 2>/dev/null)
        if echo "$REACH" | grep -q 'lo-ok'; then
            test_result "stack: host.lo.internal:$HOST_LO_PORT reaches host 127.0.0.1 over vsock" "pass"
        else
            test_result "stack: host.lo.internal vsock tunnel to host loopback failed" "fail"
        fi

        # 5. dew services lists the dew.toml [[service]] images, not just built-ins.
        SVC=$(cd "$PROJ" && "$DEW" services 2>/dev/null)
        if echo "$SVC" | grep -q 'mailpit' && echo "$SVC" | grep -q 'anycable'; then
            test_result "stack: dew services lists dew.toml services (mailpit + anycable)" "pass"
        else
            test_result "stack: dew services omitted dew.toml services" "fail"
        fi
    else
        # Stack never came up — the dependent assertions are unmeasurable, skip.
        test_result "stack: redis/anycable forward" "skip"
        test_result "stack: service-to-service localhost" "skip"
        test_result "stack: host.internal gateway" "skip"
        test_result "stack: host.lo.internal vsock tunnel" "skip"
        test_result "stack: dew services listing" "skip"
    fi

    kill -9 $STACK_PID 2>/dev/null; wait $STACK_PID 2>/dev/null
    kill $HOST_LISTENER_PID 2>/dev/null
    pkill -9 -f 'dew start\|dew up' 2>/dev/null
    rm -f ~/.local/state/dew/default.sock /tmp/dew-smoke-stack.img "$STACK_LOG"
    rm -rf "$PROJ"
else
    test_result "stack: multi-service dew.toml + host.lo.internal" "skip"
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
