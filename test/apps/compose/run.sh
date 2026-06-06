#!/bin/bash
set -uo pipefail

# Compose compatibility PoC for Dew.
#
# Boots a standard-profile VM, shares this fixture into the guest, runs
# `nerdctl compose up`, then checks the four compatibility risk points
# independently so one failure does not mask the others.
#
# Prereqs:
#   - built host binary:   make sign   (produces ./dew)
# The kernel + standard initramfs are auto-resolved by `dew vm start
# --profile standard` (downloaded from GitHub Releases on first use); a local
# `bash initramfs/build.sh standard` is only needed to test initramfs changes.
#
# Usage:
#   bash test/apps/compose/run.sh
#
# Env overrides:
#   DEW_BIN   path to dew binary           (default: repo ./dew)
#   XT_MODS   host dir of decompressed .ko files to insmod (xt_comment fix probe)
#   BK_IMAGE  BuildKit image for the S2 build tier (default: pinned above)
# The kernel + standard initramfs are auto-resolved by `dew vm start`.

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
DEW_BIN="${DEW_BIN:-$ROOT/dew}"
# Kernel + standard initramfs are auto-resolved (and downloaded from GitHub
# Releases on first use) by `dew vm start --profile standard` — no local
# initramfs build or Docker required.

PASS=0; FAIL=0
result() { # name  ok|fail  detail
  if [ "$2" = ok ]; then printf '  \033[32m✓\033[0m %-22s %s\n' "$1" "${3:-}"; PASS=$((PASS+1))
  else printf '  \033[31m✗\033[0m %-22s %s\n' "$1" "${3:-}"; FAIL=$((FAIL+1)); fi
}

de() { "$DEW_BIN" exec "$@"; }                 # exec in the running VM
deq() { "$DEW_BIN" exec "$@" >/dev/null 2>&1; } # quiet variant

cleanup() {
  echo; echo "── teardown ──"
  deq sh -c 'nerdctl compose -f /compose/compose.yaml down -v 2>/dev/null'
  deq sh -c 'nerdctl rm -f dew-buildkitd 2>/dev/null'
  "$DEW_BIN" down >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo; echo "💧 Dew compose PoC"; echo

# --- preflight ---------------------------------------------------------------
[ -x "$DEW_BIN" ] || { echo "FATAL: dew binary not found at $DEW_BIN (run: make sign)"; exit 1; }

# --- boot standard VM with fixture shared + ports forwarded ------------------
echo "── booting standard VM ──"
# shared dirs are read-only by default (the security default), which is all
# the build context needs; no :ro suffix (that suffix is mis-parsed today).
#
# XT_MODS (optional): a host dir of decompressed .ko files to insmod after
# boot, to prove the CNI fix against a RELEASED/older initramfs without
# rebuilding it. Those images' KMODS_STANDARD omitted the netfilter modules
# CNI needs (xt_comment, the nft nat-chain set, and the portmap xt modules),
# so multi-container bridge networking hung. This branch's initramfs already
# bundles them, so XT_MODS is only needed when testing against an unfixed
# image; provide the full module set, not just xt_comment.
XT_SHARE=()
[ -n "${XT_MODS:-}" ] && XT_SHARE=(--share "xtmods:$XT_MODS")
"$DEW_BIN" vm start --profile standard \
  --share "compose:$HERE" \
  ${XT_SHARE[@]+"${XT_SHARE[@]}"} \
  --forward 8080:8080 \
  --forward 8081:8081 >/tmp/dew-compose-poc-vm.log 2>&1 &
# wait until the VM boots, downloads assets on first use, and exec is live.
# First boot pulls a ~122MB initramfs, so allow a generous window.
echo "   (booting; first run downloads assets — may take a few minutes)"
for _ in $(seq 1 240); do deq true && break; sleep 1; done
deq true || { echo "FATAL: VM did not become reachable (see /tmp/dew-compose-poc-vm.log)"; exit 1; }

# sanity: nerdctl + containerd present
deq sh -c 'command -v nerdctl && nerdctl info' \
  || { echo "FATAL: nerdctl/containerd not ready in guest"; exit 1; }

# optional: inject xt_comment (+ friends) to prove the multi-container fix
if [ -n "${XT_MODS:-}" ]; then
  echo "── injecting netfilter modules (XT_MODS) ──"
  de sh -c '
    # Load every .ko provided, in two passes so modules whose deps load later
    # (e.g. nft_chain_nat/nft_nat, the portmap xt_tcpudp/xt_nat set) still come
    # up. Whatever the dir contains is loaded — not just xt_comment.
    for pass in 1 2; do
      for m in /xtmods/*.ko; do
        [ -f "$m" ] || continue
        insmod "$m" 2>/dev/null && echo "    insmod $(basename "$m")"
      done
    done
    # test on filter/INPUT (always exists); nat/POSTROUTING may be absent on a
    # clean ruleset and would give a false negative unrelated to xt_comment.
    if iptables -A INPUT -m comment --comment dewtest -j ACCEPT 2>/dev/null; then
      iptables -D INPUT -m comment --comment dewtest -j ACCEPT 2>/dev/null
      echo "    iptables -m comment: OK (xt_comment loaded)"
    else
      echo "    iptables -m comment: STILL FAILS"
    fi
  ' | sed 's/^/  /'
fi

# --- bring up the image-only services FIRST ---------------------------------
# `nerdctl compose up` builds every build: service before starting ANY
# service and aborts the whole up on a build failure — so a broken build
# would mask the volume / DNS / ports signals. Start the image-only services
# (db, gateway, netcheck) on their own to get clean, independent signal.
# clear any half-created state from a prior interrupted run (persisted on disk)
deq sh -c 'cd /compose && nerdctl compose -f /compose/compose.yaml down -v 2>/dev/null; nerdctl network rm compose_default 2>/dev/null'
echo "── nerdctl compose up (image services: db, gateway, netcheck) ──"
de sh -c 'cd /compose && nerdctl compose -f /compose/compose.yaml up -d db gateway netcheck 2>&1' | sed 's/^/    /'
echo

echo "── risk-point checks (image-tier) ──"

# R4: named volume created and bound to db
if deq sh -c 'nerdctl volume ls | grep -q dbdata'; then
  result "volume (named)" ok "dbdata present"
else
  result "volume (named)" fail "dbdata missing"
fi

# R3: service-to-service DNS + TCP over the compose network
NETCHECK=$(de sh -c "nerdctl ps --format '{{.Names}}' | grep netcheck | head -1" 2>/dev/null | tr -d '\r')
# Retry the probe: postgres runs first-boot initdb for several seconds before
# it listens on 5432, so a single immediate check false-fails on timing, not
# networking. (alpine's busybox nc supports -z.)
dns_ok=
if [ -n "$NETCHECK" ]; then
  for _ in $(seq 1 15); do
    deq sh -c "nerdctl exec $NETCHECK sh -c 'nc -z -w3 db 5432'" && { dns_ok=1; break; }
    sleep 2
  done
fi
if [ -n "$dns_ok" ]; then
  result "service DNS + net" ok "netcheck → db:5432 reachable"
else
  result "service DNS + net" fail "could not reach db:5432 from netcheck"
fi

# R2: published port reachable from the host (CNI portmap + dew --forward)
sleep 2
CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:8080/ 2>/dev/null || echo 000)
if [ "$CODE" -ge 200 ] && [ "$CODE" -lt 500 ]; then
  result "ports (image svc)" ok "host:8080 → HTTP $CODE"
else
  result "ports (image svc)" fail "host:8080 → HTTP $CODE"
fi

# --- S2: ephemeral BuildKit for the build: service --------------------------
# Key nerdctl detail: `nerdctl build` shells out to the `buildctl` CLIENT
# binary locally and talks to a buildkitd over BUILDKIT_HOST. The standard
# profile ships neither. S2 keeps the heavy daemon out of the base by running
# buildkitd as a throwaway container — and gets the small `buildctl` client by
# copying it OUT of that same image into the guest PATH. No extra download, no
# base bloat: the build toolchain is pay-per-use, materialised from one image.
# Pinned for reproducibility; override with BK_IMAGE=... for experimentation.
BK_IMAGE="${BK_IMAGE:-moby/buildkit:v0.28.1}"
BK_SOCK="unix:///run/dew-buildkit/buildkitd.sock"
echo "── S2: materialise BuildKit ($BK_IMAGE) ──"
de sh -c "
  set -e
  mkdir -p /run/dew-buildkit
  nerdctl rm -f dew-buildkitd 2>/dev/null || true
  # copy the buildctl client out of the image into the guest PATH (mount an
  # out-dir so the copied binary lands on the guest, not inside the container)
  nerdctl run --rm -v /run/dew-buildkit:/out --entrypoint sh '$BK_IMAGE' -c 'cp /usr/bin/buildctl /out/buildctl' 2>&1
  install -m755 /run/dew-buildkit/buildctl /usr/local/bin/buildctl
  # start the heavy daemon as a throwaway container
  nerdctl run -d --name dew-buildkitd --privileged --net=host \
    -v /run/dew-buildkit:/run/buildkit \
    '$BK_IMAGE' --addr unix:///run/buildkit/buildkitd.sock 2>&1
" | sed 's/^/    /'
deq sh -c 'for _ in $(seq 1 30); do [ -S /run/dew-buildkit/buildkitd.sock ] && command -v buildctl >/dev/null && exit 0; sleep 1; done; exit 1' \
  && echo "    buildctl + buildkitd ready" \
  || echo "    WARN: buildctl/buildkitd not ready"

echo "── nerdctl compose build builder (BUILDKIT_HOST → ephemeral) ──"
de sh -c "cd /compose && BUILDKIT_HOST=$BK_SOCK nerdctl compose -f /compose/compose.yaml build builder 2>&1" | sed 's/^/    /'
echo "── nerdctl compose up builder ──"
de sh -c "cd /compose && BUILDKIT_HOST=$BK_SOCK nerdctl compose -f /compose/compose.yaml up -d builder 2>&1" | sed 's/^/    /'

echo
echo "── risk-point check (build-tier) ──"
# R1: build: context via S2 ephemeral buildkit. PASS requires BOTH a freshly
# built image AND the built service actually serving — gate on the HTTP probe
# (retried, since it has just started), not merely on an image existing, so a
# stale image from a prior run can't produce a false pass.
sleep 2
CODE2=000
if deq sh -c 'nerdctl images | grep -q builder'; then
  for _ in $(seq 1 10); do
    CODE2=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:8081/ 2>/dev/null || echo 000)
    case "$CODE2" in 2*|3*) break;; esac
    sleep 2
  done
fi
case "$CODE2" in
  2*|3*) result "build: (S2 buildkit)" ok   "image built, host:8081 → HTTP $CODE2";;
  *)     result "build: (S2 buildkit)" fail "image present=$(deq sh -c 'nerdctl images | grep -q builder' && echo yes || echo no), host:8081 → HTTP $CODE2";;
esac

echo; echo "── results ──"; echo "  PASS: $PASS  FAIL: $FAIL"
echo
