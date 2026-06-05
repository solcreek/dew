#!/usr/bin/env bash
# release.sh — cut a dew release end-to-end from one CLI command.
#
# Usage: scripts/release.sh <version>
#
#   version: bare semver (e.g. 0.7.32). Leading `v` accepted and stripped.
#
# Steps (all aborted if any precondition fails — no partial state):
#   1. Validate version arg + sanity-check repo state
#       - working tree clean
#       - on branch main
#       - up to date with origin/main
#       - tag v<version> doesn't already exist (local or remote)
#       - CHANGELOG.md has a non-empty [Unreleased] section
#   2. Rename CHANGELOG.md `[Unreleased]` → `[<version>] - <YYYY-MM-DD>`
#       (UTC date; matches the format the existing CHANGELOG uses)
#   3. Commit the CHANGELOG bump as `chore: changelog for v<version>`
#   4. Create tag v<version> (unsigned — see docs/RELEASING.md rationale)
#   5. Push commit + tag to origin
#       (tag push triggers .github/workflows/release.yml)
#   6. Print the release pipeline URL
#
# The goal: zero browser clicks, zero manual file editing at release time.
# CHANGELOG entries are still authored by PR authors during normal dev —
# this script only RELOCATES them from [Unreleased] to the new section.

set -euo pipefail

err()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }
note() { printf '\033[36m→\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }

# ── 1. Validate args + repo state ──────────────────────────────────

[ $# -eq 1 ] || err "usage: $0 <version>   e.g. $0 0.7.32"

VERSION="${1#v}"  # strip leading v if present
echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
    || err "version '$VERSION' is not bare semver (e.g. 0.7.32)"

TAG="v$VERSION"
TODAY=$(date -u +%Y-%m-%d)
REPO_ROOT=$(git rev-parse --show-toplevel)
CHANGELOG="$REPO_ROOT/CHANGELOG.md"

cd "$REPO_ROOT"

note "preflight checks"

# clean tree
if ! git diff --quiet || ! git diff --cached --quiet; then
    git status --short
    err "working tree not clean — commit or stash first"
fi

# on main
BRANCH=$(git symbolic-ref --short HEAD)
[ "$BRANCH" = "main" ] || err "not on main (currently on '$BRANCH')"

# up to date with origin
git fetch --quiet origin main
LOCAL=$(git rev-parse main)
REMOTE=$(git rev-parse origin/main)
[ "$LOCAL" = "$REMOTE" ] || err "local main is not in sync with origin/main — pull/push first"

# tag doesn't already exist
if git rev-parse "$TAG" >/dev/null 2>&1; then
    err "tag $TAG already exists locally"
fi
if git ls-remote --exit-code --tags origin "$TAG" >/dev/null 2>&1; then
    err "tag $TAG already exists on origin"
fi

# CHANGELOG has a non-empty [Unreleased] section
[ -f "$CHANGELOG" ] || err "CHANGELOG.md not found at $CHANGELOG"

# Extract everything between '## [Unreleased]' and the next '## [' header.
# If the result is whitespace-only, the section is empty and there's
# nothing to release.
UNRELEASED_BODY=$(awk '
    /^## \[Unreleased\]/ { in_section=1; next }
    /^## \[/ && in_section { exit }
    in_section { print }
' "$CHANGELOG")

if [ -z "$(echo "$UNRELEASED_BODY" | tr -d '[:space:]')" ]; then
    err "CHANGELOG.md [Unreleased] section is empty — nothing to release"
fi

ok "preflight passed"

# ── 2. Rewrite CHANGELOG ──────────────────────────────────────────

note "renaming [Unreleased] → [$VERSION] - $TODAY"

# Insert a fresh empty [Unreleased] above the new versioned section so
# the next PR has a place to add entries. The empty section is the
# accepted convention — same shape Keep-a-Changelog recommends.
#
# sed -i works differently on BSD (mac) vs GNU; -i '' is portable on
# mac, but safer is to write to a temp file and atomic-rename.
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
awk -v version="$VERSION" -v today="$TODAY" '
    /^## \[Unreleased\]/ {
        print "## [Unreleased]"
        print ""
        print "## [" version "] - " today
        next
    }
    { print }
' "$CHANGELOG" > "$TMP"
mv "$TMP" "$CHANGELOG"

ok "CHANGELOG updated"

# ── 3-5. Commit + tag + push ──────────────────────────────────────

note "committing CHANGELOG bump"
git add CHANGELOG.md
git commit -m "chore: changelog for $TAG" --quiet
ok "committed $(git rev-parse --short HEAD)"

note "tagging $TAG"
git tag "$TAG" -m "$TAG"
ok "tagged"

note "pushing main + $TAG"
git push --quiet origin main
git push --quiet origin "$TAG"
ok "pushed"

# ── 6. Show pipeline URL ──────────────────────────────────────────

REMOTE_URL=$(git config --get remote.origin.url)
# Convert git@github.com:owner/repo.git → owner/repo
SLUG=$(echo "$REMOTE_URL" | sed -E 's,^git@github\.com:,,;s,^https://github\.com/,,;s,\.git$,,')
echo
ok "release $TAG triggered"
echo "   pipeline: https://github.com/$SLUG/actions/workflows/release.yml"
echo "   release : https://github.com/$SLUG/releases/tag/$TAG  (appears once pipeline completes)"
