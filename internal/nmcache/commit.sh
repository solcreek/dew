#!/bin/sh
# commit.sh — atomically commit a successful install to the cache.
#
# Called by the host AFTER the install command exited 0. Renaming
# .inprogress → .json is atomic on ext4, so a crash between the two
# steps still leaves the cache in a recoverable state:
#   - crash before rename: .inprogress present, .json absent
#       → next boot wipes and re-installs (safe)
#   - crash after rename: .json present, .inprogress absent
#       → next boot considers cache valid (correct: install succeeded)

set -eu

: "${DEW_NM_KEY:?DEW_NM_KEY must be set}"
: "${DEW_NM_CACHE_ROOT:=/var/cache/dew/nm}"

CACHE_DIR="$DEW_NM_CACHE_ROOT/$DEW_NM_KEY"
STAMP="$CACHE_DIR/.dew-stamp.json"
INPROG="$CACHE_DIR/.dew-stamp.inprogress.json"

if [ ! -f "$INPROG" ]; then
    echo "commit.sh: nothing to commit (no .inprogress)" >&2
    exit 1
fi

mv "$INPROG" "$STAMP"
