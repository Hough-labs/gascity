#!/usr/bin/env bash
# scripts/upgrade-integration.sh — reset the `integration` branch to the pinned
# upstream BASELINE and replay the fork's patches/ on top.
#
# Usage: make upgrade   (the Makefile passes BASELINE in via the environment)
#
# What it does:
#   1. Guards: on the integration branch, clean tree, patches/ non-empty
#   2. Fetches upstream so BASELINE is reachable
#   3. Copies patches/ to a tmpdir FIRST — `git reset` wipes it, because the
#      upstream baseline does not track patches/
#   4. Shows what the baseline brings in beyond the current tree
#   5. Resets integration to BASELINE
#   6. Replays the patches via `git am --3way`
#   7. Regenerates patches/ from the freshly replayed commits (it is excluded
#      from the export, so `git am` does not restore it) and commits if changed
#   8. On conflict: prints clear resume/abort instructions and exits 1
#   9. On success: offers to build and install

set -euo pipefail

# BASELINE is normally injected by `make upgrade`; keep a default so the script
# is runnable standalone. Must match the Makefile's BASELINE.
BASELINE="${BASELINE:-a7297c511d637a3609947386f3389d76ddb2f23b}"

RED='\033[0;31m'
GRN='\033[0;32m'
YLW='\033[0;33m'
BLU='\033[0;34m'
DIM='\033[2m'
RST='\033[0m'

info()  { echo -e "${BLU}▸${RST} $*"; }
ok()    { echo -e "${GRN}✓${RST} $*"; }
warn()  { echo -e "${YLW}⚠${RST} $*"; }
die()   { echo -e "${RED}✗${RST} $*" >&2; exit 1; }
dim()   { echo -e "${DIM}$*${RST}"; }

# ── Guards ───────────────────────────────────────────────────────────────────

BRANCH=$(git symbolic-ref --short HEAD 2>/dev/null) || die "Not on a git branch"
[ "$BRANCH" = "integration" ] || die "Must be on the integration branch (currently on: $BRANCH)"

if ! git diff --quiet || ! git diff --cached --quiet; then
    die "Working tree is dirty. Commit or stash changes before upgrading."
fi

PATCH_COUNT=$(find patches/ -maxdepth 1 -name '*.patch' 2>/dev/null | wc -l | tr -d ' ')
[ "$PATCH_COUNT" -gt 0 ] || die "No patches found in patches/ — run 'make patches' first"

# ── Fetch and locate baseline ────────────────────────────────────────────────

info "Fetching upstream..."
git fetch upstream --quiet --tags || warn "fetch failed (offline?); using local objects"

git rev-parse --quiet --verify "$BASELINE^{commit}" >/dev/null 2>&1 \
    || die "BASELINE $BASELINE not found locally — fetch it before upgrading."

TARGET_SHORT=$(git rev-parse --short "$BASELINE")
TARGET_SUBJ=$(git log -1 --format='%s' "$BASELINE")

# ── Copy patches out before the reset wipes them ─────────────────────────────

PATCH_TMPDIR=$(mktemp -d)
trap 'rm -rf "$PATCH_TMPDIR"' EXIT
cp -f patches/*.patch "$PATCH_TMPDIR/"

INCOMING=$(git log --oneline "HEAD..$BASELINE" 2>/dev/null || true)
if [ -n "$INCOMING" ]; then
    INCOMING_COUNT=$(printf '%s\n' "$INCOMING" | grep -c . || true)
    echo ""
    echo -e "${BLU}Baseline $TARGET_SHORT brings in $INCOMING_COUNT commit(s) not in the current tree:${RST}"
    printf '%s\n' "$INCOMING" | while read -r line; do dim "  $line"; done
    echo ""
else
    dim "Baseline unchanged vs the current tree — re-materializing patches."
fi

# ── Reset and replay ─────────────────────────────────────────────────────────

info "Resetting integration to $TARGET_SHORT ($TARGET_SUBJ)..."
git reset --hard "$BASELINE" --quiet
ok "Reset to $TARGET_SHORT"

echo ""
info "Replaying $PATCH_COUNT fork patches..."
PATCH_NUM=0
for patch in "$PATCH_TMPDIR"/*.patch; do
    PATCH_NUM=$((PATCH_NUM + 1))
    SUBJECT=$(grep -m1 '^Subject:' "$patch" | sed 's/Subject: \[PATCH[^]]*\] //')
    printf "  ${DIM}%d/%d %s${RST}\n" "$PATCH_NUM" "$PATCH_COUNT" "$SUBJECT"
done
echo ""

if ! git am --3way "$PATCH_TMPDIR"/*.patch; then
    echo ""
    die "$(cat <<EOF
Patch conflict — resolve then continue:

  1. Edit the conflicting file(s)
  2. git add <file>
  3. git am --continue

To bail out entirely:
  git am --abort
  git reset --hard $BASELINE
EOF
)"
fi

ok "All $PATCH_COUNT patches applied — integration at $(git rev-parse --short HEAD)"

# ── Restore patches/ ─────────────────────────────────────────────────────────
# The reset wiped patches/ and `git am` did not restore it (patches/ is excluded
# from the export). Regenerate it from the replayed commits so the tree once
# again satisfies the invariant `make check-patches` enforces.
echo ""
info "Regenerating patches/ from the replayed commits..."
make -s patches >/dev/null
# The reset wiped patches/, so after replay it is untracked — stage first, then
# check the index (a plain `git diff` would not see untracked files).
git add patches/
if ! git diff --cached --quiet -- patches/; then
    git commit --quiet -m "build(patches): refresh export after upgrade to $TARGET_SHORT"
    ok "patches/ refreshed and committed"
else
    ok "patches/ unchanged"
fi
echo ""

# ── Optional build ───────────────────────────────────────────────────────────

if [ -t 0 ]; then
    read -rp "$(echo -e "${BLU}▸${RST} Build and install now? [Y/n] ")" REPLY
    REPLY=${REPLY:-Y}
    if [[ "$REPLY" =~ ^[Yy]$ ]]; then
        echo ""
        make install
    fi
else
    dim "  (non-interactive — run 'make install' when ready)"
fi

echo ""
ok "Upgrade complete."
