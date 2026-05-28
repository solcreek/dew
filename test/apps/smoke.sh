#!/bin/bash
set -euo pipefail

# Smoke test: validate dew build + dew deploy + dew serve with real apps.
# No Docker required. Uses dew serve's built-in process/static server.
#
# Usage:
#   ./smoke.sh              # run all tests
#   ./smoke.sh static       # run static site tests only
#   ./smoke.sh server       # run server app tests only

DEW_BIN="${DEW_BIN:-./dew}"
SERVE_DIR="/tmp/dew-smoke-$$"
SERVE_PORT=9082
TOKEN="smoke-test-token"
PASS=0
FAIL=0

cleanup() {
  kill $(lsof -ti:$SERVE_PORT) 2>/dev/null || true
  # Kill any app processes on 10000+
  for p in $(seq 10000 10010); do
    kill $(lsof -ti:$p) 2>/dev/null || true
  done
  rm -rf "$SERVE_DIR"
}
trap cleanup EXIT

setup_serve() {
  mkdir -p "$SERVE_DIR"
  echo "$TOKEN" > "$SERVE_DIR/token"
  "$DEW_BIN" serve --port $SERVE_PORT --data-dir "$SERVE_DIR" > /dev/null 2>&1 &
  sleep 2
  if ! curl -s -o /dev/null http://localhost:$SERVE_PORT/v1/system/health; then
    echo "FATAL: dew serve failed to start"
    exit 1
  fi
}

test_build_deploy() {
  local name="$1"
  local dir="$2"
  local expected_type="$3"
  local health_path="${4:-/}"

  printf "  %-25s " "$name"

  if [ ! -d "$dir" ]; then
    echo "SKIP (dir not found)"
    return
  fi

  local tarball="/tmp/dew-smoke-$name.tar.gz"
  rm -f "$tarball"

  # Build
  if ! "$DEW_BIN" build "$dir" -o "$tarball" > /dev/null 2>&1; then
    echo "FAIL (build)"
    FAIL=$((FAIL + 1))
    return
  fi

  # Deploy
  if ! DEW_TOKEN=$TOKEN "$DEW_BIN" deploy "http://localhost:$SERVE_PORT" --tarball "$tarball" --app "$name" > /dev/null 2>&1; then
    echo "FAIL (deploy)"
    FAIL=$((FAIL + 1))
    rm -f "$tarball"
    return
  fi
  rm -f "$tarball"

  # Get port
  local app_port
  app_port=$(curl -s -H "Authorization: Bearer $TOKEN" "http://localhost:$SERVE_PORT/v1/apps" | \
    python3 -c "import json,sys; apps=json.load(sys.stdin); print([a['port'] for a in apps if a['name']=='$name'][0])" 2>/dev/null)

  if [ -z "$app_port" ] || [ "$app_port" = "0" ]; then
    echo "FAIL (no port)"
    FAIL=$((FAIL + 1))
    return
  fi

  # Health check
  sleep 1
  local code
  code=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:$app_port$health_path" 2>/dev/null || echo "000")
  if [ "$code" -ge 200 ] && [ "$code" -lt 400 ]; then
    echo "PASS (:$app_port, HTTP $code)"
    PASS=$((PASS + 1))
  else
    echo "FAIL (HTTP $code on :$app_port)"
    FAIL=$((FAIL + 1))
  fi
}

echo ""
echo "💧 Dew App Smoke Tests"
echo ""

setup_serve

TARGET="${1:-all}"

if [ "$TARGET" = "all" ] || [ "$TARGET" = "static" ]; then
  echo "── Static sites ──"
  test_build_deploy "open-slide" "/tmp/my-slide" "static" "/"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "server" ]; then
  echo ""
  echo "── Server apps ──"
  test_build_deploy "demo-crud" "/Users/linyiru/Projects/creek/demo-vite-sqlite" "server" "/api/health"
fi

echo ""
echo "── Results ──"
echo "  PASS: $PASS  FAIL: $FAIL"
echo ""

[ "$FAIL" -eq 0 ] || exit 1
