#!/bin/bash
set -euo pipefail

# Smoke test: validate dew deploy --image with real Docker Hub apps.
# Usage:
#   ./smoke.sh              # run all tier 1 tests
#   ./smoke.sh excalidraw   # run single test

PASS=0
FAIL=0
SKIP=0

test_image() {
  local name="$1"
  local image="$2"
  local port="$3"
  local health_path="$4"
  local env_args="${5:-}"

  printf "  %-20s " "$name"

  # Pull
  if ! docker pull "$image" > /dev/null 2>&1; then
    echo "SKIP (pull failed)"
    SKIP=$((SKIP + 1))
    return
  fi

  # Stop previous
  docker rm -f "dew-test-$name" > /dev/null 2>&1 || true

  # Run
  local host_port=$((port + 10000))
  local run_cmd="docker run -d --name dew-test-$name -p $host_port:$port"
  if [ -n "$env_args" ]; then
    run_cmd="$run_cmd $env_args"
  fi
  run_cmd="$run_cmd $image"

  if ! eval "$run_cmd" > /dev/null 2>&1; then
    echo "FAIL (start)"
    FAIL=$((FAIL + 1))
    return
  fi

  # Wait for health
  local ok=false
  for i in $(seq 1 20); do
    sleep 2
    code=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:$host_port$health_path" 2>/dev/null || echo "000")
    if [ "$code" -ge 200 ] && [ "$code" -lt 400 ]; then
      ok=true
      break
    fi
  done

  # Cleanup
  docker rm -f "dew-test-$name" > /dev/null 2>&1 || true

  if $ok; then
    echo "PASS (${i}s)"
    PASS=$((PASS + 1))
  else
    echo "FAIL (health check, last HTTP $code)"
    FAIL=$((FAIL + 1))
  fi
}

test_build() {
  local name="$1"
  local dir="$2"
  local expected_type="$3"

  printf "  %-20s " "$name (build)"

  if [ ! -d "$dir" ]; then
    echo "SKIP (dir not found)"
    SKIP=$((SKIP + 1))
    return
  fi

  local dew_bin="${DEW_BIN:-./dew}"
  local out="/tmp/dew-smoke-$name.tar.gz"
  rm -f "$out"

  if ! "$dew_bin" build "$dir" -o "$out" > /dev/null 2>&1; then
    echo "FAIL (build)"
    FAIL=$((FAIL + 1))
    return
  fi

  if [ ! -f "$out" ]; then
    echo "FAIL (no tarball)"
    FAIL=$((FAIL + 1))
    return
  fi

  # Check manifest type
  local app_type
  app_type=$(python3 -c "
import tarfile, json
with tarfile.open('$out') as t:
    f = t.extractfile('manifest.json')
    print(json.load(f).get('type', 'unknown'))
" 2>/dev/null || echo "error")

  rm -f "$out"

  if [ "$app_type" = "$expected_type" ]; then
    echo "PASS (type=$app_type)"
    PASS=$((PASS + 1))
  else
    echo "FAIL (type=$app_type, expected $expected_type)"
    FAIL=$((FAIL + 1))
  fi
}

echo ""
echo "💧 Dew App Smoke Tests"
echo ""

TARGET="${1:-all}"

if [ "$TARGET" = "all" ] || [ "$TARGET" = "excalidraw" ]; then
  echo "── Docker image apps ──"
  test_image "excalidraw" "excalidraw/excalidraw" "80" "/"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "uptime-kuma" ]; then
  test_image "uptime-kuma" "louislam/uptime-kuma:2" "3001" "/"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "vaultwarden" ]; then
  test_image "vaultwarden" "vaultwarden/server" "80" "/alive"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "gitea" ]; then
  test_image "gitea" "gitea/gitea" "3000" "/" "-e GITEA__security__INSTALL_LOCK=true"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "ghost" ]; then
  test_image "ghost" "ghost:5-alpine" "2368" "/ghost/api/admin/site/" "-e NODE_ENV=development -e database__client=sqlite3"
fi

if [ "$TARGET" = "all" ] || [ "$TARGET" = "build" ]; then
  echo ""
  echo "── dew build ──"
  test_build "demo-crud" "/Users/linyiru/Projects/creek/demo-vite-sqlite" "server"
fi

echo ""
echo "── Results ──"
echo "  PASS: $PASS  FAIL: $FAIL  SKIP: $SKIP"
echo ""

[ "$FAIL" -eq 0 ] || exit 1
