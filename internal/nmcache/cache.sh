#!/bin/sh
# cache.sh — node_modules cache setup, runs inside the dew guest VM.
#
# Inputs (passed by the host via environment):
#   DEW_NM_KEY   12-char project key (sha256[:12] of abs project path)
#   DEW_NM_WANT  serialized stamp the cache should hold for cache hit
#
# Outputs (to stdout, one line):
#   DEW_NM_CACHE=hit   bind-mount done, install can be skipped
#   DEW_NM_CACHE=miss  bind-mount done, .inprogress marker written;
#                      caller must run install and then commit-stamp.sh
#
# Crash recovery contract:
#   If .dew-stamp.inprogress.json is present, the previous install
#   died midway. node_modules is wiped before mount so the next
#   install starts clean. This is the load-bearing invariant of the
#   write-ahead-pointer pattern; see TestScript_CrashRecovery.

set -eu

: "${DEW_NM_KEY:?DEW_NM_KEY must be set}"
: "${DEW_NM_WANT:?DEW_NM_WANT must be set}"
: "${DEW_NM_CACHE_ROOT:=/var/cache/dew/nm}"
: "${DEW_NM_TARGET:=/app/node_modules}"

CACHE_DIR="$DEW_NM_CACHE_ROOT/$DEW_NM_KEY"
STAMP="$CACHE_DIR/.dew-stamp.json"
INPROG="$CACHE_DIR/.dew-stamp.inprogress.json"

mkdir -p "$CACHE_DIR/node_modules"

# Crash recovery: previous install died before commit. Wipe and start
# clean — never trust a half-installed tree.
if [ -e "$INPROG" ]; then
    rm -rf "$CACHE_DIR/node_modules"
    rm -f "$INPROG" "$STAMP"
    mkdir -p "$CACHE_DIR/node_modules"
fi

mkdir -p "$DEW_NM_TARGET"
if ! mountpoint -q "$DEW_NM_TARGET"; then
    mount --bind "$CACHE_DIR/node_modules" "$DEW_NM_TARGET"
fi

GOT=""
if [ -f "$STAMP" ]; then
    GOT=$(cat "$STAMP")
fi

if [ "$GOT" = "$DEW_NM_WANT" ]; then
    echo "DEW_NM_CACHE=hit"
else
    # Mark install in progress — if we crash before commit, the next
    # boot will detect the marker and wipe.
    printf "%s" "$DEW_NM_WANT" > "$INPROG"
    echo "DEW_NM_CACHE=miss"
fi
