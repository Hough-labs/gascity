#!/usr/bin/env bash
#
# test-gate-green-run.sh — unit tests for the per-tree green-marker cache
# that stops a refinery direct merge from running the Darwin test lane twice
# on an unchanged tree (gascity-nuw).
#
# Everything here is hermetic: a throwaway git repo plus a fake `go` and a
# fake lane command on PATH. No real suite runs. The fake lane command
# appends to a counter file, so "did the lane actually run?" is a fact this
# file reads rather than a timing inference, and every case asserts the
# DELTA in that counter so the cases stay independent of each other.
#
# Coverage: the miss/record/hit cycle, every key input that must invalidate a
# marker (lane, argv, tree, toolchain), every state that must disable the
# cache outright (dirty worktree, no git dir, CI), the exit statuses that
# must NOT record a pass (ordinary failure and gate-busy alike), TTL expiry
# and clock steps, the GATE_GREEN_NO_CACHE escape hatch, marker
# auditability, the redaction that keeps assignment values out of the marker,
# what the key does and does not distinguish, the diagnostic a miss prints,
# pruning, and a static assertion that the Makefile's test-mac recipe still
# routes through this wrapper ahead of the slot gate.
#
# Three cases carry the bead's actual acceptance criterion rather than a
# component of it, and all use real git rather than a stand-in: a lane run
# from a real `git push` pre-push hook must see the marker the preceding gate
# run recorded (git hands hooks an environment of its own, and that is what
# the tree SHA and clean-status are read from); the same must hold when the
# wrapped argv carries the ambient environment the real recipe interpolates,
# which is the case that git's exec-path injection breaks and the one the
# fixed-argv case above cannot exhibit; and a marker recorded in one linked
# worktree must be visible from another, which is what makes a fast-forward
# merge in the refinery's worktree free.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE_GREEN_RUN="$TEST_DIR/gate-green-run"
MAKEFILE="$(cd "$TEST_DIR/.." && pwd)/Makefile"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}
assert_true() { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected true"; fi; }
assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "missing '$needle' in: $haystack"; fi
}
assert_not_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" != *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "found '$needle' in: $haystack"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ---------------- fakes on PATH ----------------
BIN="$WORK/bin"
mkdir -p "$BIN"

# `go version` is part of the cache key, so the fake reads it from a file the
# tests rewrite to simulate a toolchain bump.
GO_VERSION_FILE="$WORK/go-version.txt"
echo "go version go1.99.0 gate-green/test" >"$GO_VERSION_FILE"
cat >"$BIN/go" <<EOF
#!/usr/bin/env bash
cat "$GO_VERSION_FILE"
EOF
chmod +x "$BIN/go"

RUNS="$WORK/runs"
: >"$RUNS"
cat >"$BIN/lane-cmd" <<EOF
#!/usr/bin/env bash
echo "run \$*" >>"$RUNS"
exit \${LANE_CMD_EXIT:-0}
EOF
chmod +x "$BIN/lane-cmd"

export PATH="$BIN:$PATH"

# ---------------- throwaway repo ----------------
REPO="$WORK/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q
git -C "$REPO" config user.email gate-green@example.invalid
git -C "$REPO" config user.name "gate green"
echo one >"$REPO/a.txt"
git -C "$REPO" add -A
git -C "$REPO" commit -qm one

MARKER_DIR="$REPO/.git/gate-green"
ERR="$WORK/err"

run_count()     { wc -l <"$RUNS" | tr -d ' '; }
# find(1) rather than a glob: the marker dir may not exist yet, and an
# unmatched glob would be counted as one literal "path" by wc.
marker_count()  { find "$MARKER_DIR" -maxdepth 1 -type f -name '*.marker' 2>/dev/null | wc -l | tr -d ' '; }
reset_markers() { rm -rf "$MARKER_DIR"; }

# run_gate [VAR=VAL ...] <lane> <command> [args...]
#
# Leading VAR=VAL words are handed to env(1) rather than prefixed onto the
# call: in bash a prefix assignment on a FUNCTION call persists in the shell
# afterwards, which would silently leak LANE_CMD_EXIT or CI into every later
# case. Stderr is captured so cases can assert on the diagnostics.
run_gate() {
    local envs=()
    while [[ "$1" == *=* ]]; do envs+=("$1"); shift; done
    ( cd "$REPO" && env ${envs[@]+"${envs[@]}"} "$GATE_GREEN_RUN" "$@" ) >/dev/null 2>"$ERR"
}

# Rewrites the recorded_epoch of the one live marker. A missing marker is a
# test bug, not a silent no-op: without this guard the redirect below resolves
# against an empty path and drops a stray ".rewritten" into the CURRENT
# directory instead of failing the case that expected a marker.
rewrite_marker_epoch() {
    local name="$1" epoch="$2" marker
    marker="$(find "$MARKER_DIR" -maxdepth 1 -type f -name '*.marker' 2>/dev/null | sed -n '1p')"
    if [[ -z "$marker" ]]; then
        record_fail "$name" "expected a recorded marker to rewrite, found none"
        return 1
    fi
    sed "s/^recorded_epoch=.*/recorded_epoch=${epoch}/" "$marker" >"$marker.rewritten" \
        && mv -f "$marker.rewritten" "$marker"
}

# ---------------- usage ----------------
( cd "$REPO" && "$GATE_GREEN_RUN" only-a-lane ) >/dev/null 2>&1
assert_eq "usage.too_few_args_exits_2" "$?" "2"

# ---------------- miss, record, hit ----------------
reset_markers
before="$(run_count)"; run_gate test-mac lane-cmd alpha; status=$?
assert_eq "miss.exit_status"         "$status" "0"
assert_eq "miss.ran_the_lane"        "$(( $(run_count) - before ))" "1"
assert_eq "miss.recorded_one_marker" "$(marker_count)" "1"

before="$(run_count)"; run_gate test-mac lane-cmd alpha; status=$?
assert_eq "hit.exit_status"        "$status" "0"
assert_eq "hit.did_not_rerun_lane" "$(( $(run_count) - before ))" "0"
assert_contains "hit.explains_itself"        "$(cat "$ERR")" "already passed"
assert_contains "hit.names_the_escape_hatch" "$(cat "$ERR")" "GATE_GREEN_NO_CACHE=1"

# ---------------- the marker is auditable, not just a flag ----------------
MARKER_BODY="$(cat "$MARKER_DIR"/*.marker 2>/dev/null)"
assert_contains "marker.records_lane"    "$MARKER_BODY" "lane=test-mac"
assert_contains "marker.records_tree"    "$MARKER_BODY" "tree=$(git -C "$REPO" rev-parse 'HEAD^{tree}')"
assert_contains "marker.records_epoch"   "$MARKER_BODY" "recorded_epoch="
assert_contains "marker.records_command" "$MARKER_BODY" "command=lane-cmd alpha"

# ---------------- every key input invalidates ----------------
before="$(run_count)"; run_gate other-lane lane-cmd alpha
assert_eq "key.lane_change_reruns" "$(( $(run_count) - before ))" "1"

before="$(run_count)"; run_gate test-mac lane-cmd beta
assert_eq "key.argv_change_reruns" "$(( $(run_count) - before ))" "1"

echo "go version go2.0.0 gate-green/test" >"$GO_VERSION_FILE"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "key.toolchain_change_reruns" "$(( $(run_count) - before ))" "1"
echo "go version go1.99.0 gate-green/test" >"$GO_VERSION_FILE"

before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "key.marker_for_old_toolchain_still_hits" "$(( $(run_count) - before ))" "0"

echo two >"$REPO/b.txt"
git -C "$REPO" add -A
git -C "$REPO" commit -qm two
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "key.tree_change_reruns" "$(( $(run_count) - before ))" "1"

# ---------------- states that disable the cache ----------------
reset_markers
echo debris >"$REPO/untracked.txt"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "dirty.runs_the_lane"     "$(( $(run_count) - before ))" "1"
assert_eq "dirty.records_no_marker" "$(marker_count)" "0"
assert_contains "dirty.says_why" "$(cat "$ERR")" "worktree is dirty"
rm -f "$REPO/untracked.txt"

reset_markers
run_gate test-mac lane-cmd alpha
assert_eq "clean_again.records_marker" "$(marker_count)" "1"
echo debris >"$REPO/untracked.txt"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "dirty.ignores_existing_marker" "$(( $(run_count) - before ))" "1"
rm -f "$REPO/untracked.txt"

reset_markers
run_gate test-mac lane-cmd alpha
before="$(run_count)"; run_gate CI=true test-mac lane-cmd alpha
assert_eq "ci.ignores_marker"      "$(( $(run_count) - before ))" "1"
assert_contains "ci.says_why" "$(cat "$ERR")" "CI detected"
assert_eq "ci.records_no_new_marker" "$(marker_count)" "1"

NOGIT="$WORK/nogit"
mkdir -p "$NOGIT"
before="$(run_count)"
( cd "$NOGIT" && env GIT_CEILING_DIRECTORIES="$WORK" "$GATE_GREEN_RUN" test-mac lane-cmd alpha ) >/dev/null 2>"$ERR"
assert_eq "nogit.runs_the_lane" "$(( $(run_count) - before ))" "1"
assert_contains "nogit.says_why" "$(cat "$ERR")" "not cacheable"

# ---------------- statuses that must not record a pass ----------------
reset_markers
run_gate LANE_CMD_EXIT=1 test-mac lane-cmd alpha; status=$?
assert_eq "failure.propagates_status" "$status" "1"
assert_eq "failure.records_no_marker" "$(marker_count)" "0"

run_gate LANE_CMD_EXIT=75 test-mac lane-cmd alpha; status=$?
assert_eq "gate_busy.propagates_75"     "$status" "75"
assert_eq "gate_busy.records_no_marker" "$(marker_count)" "0"

run_gate LANE_CMD_EXIT=3 test-mac lane-cmd alpha; status=$?
assert_eq "failure.propagates_arbitrary_status" "$status" "3"

# ---------------- TTL, clock steps, escape hatch ----------------
reset_markers
run_gate test-mac lane-cmd alpha

before="$(run_count)"; run_gate GATE_GREEN_TTL_SECONDS=0 test-mac lane-cmd alpha
assert_eq "ttl.zero_never_hits" "$(( $(run_count) - before ))" "1"

rewrite_marker_epoch "ttl.expired_marker_reruns" "$(( $(date +%s) - 100000 ))"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "ttl.expired_marker_reruns" "$(( $(run_count) - before ))" "1"

rewrite_marker_epoch "clock.future_marker_reruns" "$(( $(date +%s) + 100000 ))"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "clock.future_marker_reruns" "$(( $(run_count) - before ))" "1"

before="$(run_count)"; run_gate GATE_GREEN_TTL_SECONDS=not-a-number test-mac lane-cmd alpha
assert_eq "ttl.malformed_falls_back_to_default_and_hits" "$(( $(run_count) - before ))" "0"
assert_contains "ttl.malformed_says_so" "$(cat "$ERR")" "malformed GATE_GREEN_TTL_SECONDS"

before="$(run_count)"; run_gate GATE_GREEN_NO_CACHE=1 test-mac lane-cmd alpha
assert_eq "no_cache.forces_a_run" "$(( $(run_count) - before ))" "1"
assert_contains "no_cache.says_why" "$(cat "$ERR")" "GATE_GREEN_NO_CACHE set"
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "no_cache.green_run_still_records" "$(( $(run_count) - before ))" "0"

# ---------------- the shape the bead is actually about ----------------
# A lane run twice on one tree: a gate runs it, then `git push` runs it again
# from the pre-push hook. git hands hooks an environment of its own — GIT_DIR
# above all — and that environment is what gate-green-run reads its tree SHA
# and its clean-status from. Reasoning that the cache survives that is not the
# same as watching it survive, so this case actually pushes.
reset_markers
REMOTE="$WORK/remote.git"
git init -q --bare "$REMOTE"
git -C "$REPO" remote add origin "$REMOTE"
cat >"$REPO/.git/hooks/pre-push" <<EOF
#!/usr/bin/env bash
cat >/dev/null   # git feeds "<local ref> <sha> <remote ref> <sha>" on stdin
exec "$GATE_GREEN_RUN" test-mac lane-cmd alpha
EOF
chmod +x "$REPO/.git/hooks/pre-push"

before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "prepush.gate_run_ran_the_lane" "$(( $(run_count) - before ))" "1"

before="$(run_count)"
( cd "$REPO" && git push -q origin HEAD:refs/heads/pushed ) >/dev/null 2>"$ERR"
push_status=$?
assert_eq "prepush.push_succeeded"          "$push_status" "0"
assert_eq "prepush.hook_did_not_rerun_lane" "$(( $(run_count) - before ))" "0"

# ...and the same push with no marker to lean on must still run the lane, or
# the case above would pass just as well against a hook that never fired.
reset_markers
before="$(run_count)"
( cd "$REPO" && git push -q origin HEAD:refs/heads/pushed-again ) >/dev/null 2>"$ERR"
assert_eq "prepush.hook_runs_lane_without_a_marker" "$(( $(run_count) - before ))" "1"
rm -f "$REPO/.git/hooks/pre-push"

# ---------------- the ambient values the recipe interpolates ----------------
# The case above proves the hook can reach a marker, but its wrapped argv is a
# fixed string. The real recipe's is not: once make expands $(TEST_ENV) the
# wrapped argv is `env -i PATH=... HOME=... GOFLAGS=... ...`, so whatever those
# variables hold at call time becomes part of the command being keyed.
#
# git prepends its own exec-path to PATH for every hook it runs (measured:
# /Applications/Xcode.app/Contents/Developer/usr/libexec/git-core appears in
# the hook's PATH and not the parent shell's). So the gate run and the pre-push
# run of one identical tree ALWAYS disagree about PATH's value — by
# construction, on every machine. A key built from those values can therefore
# never match across the one boundary this cache exists to span, which is the
# whole feature silently doing nothing.
#
# This case reproduces that boundary: same lane, same tree, argv carrying the
# ambient PATH exactly as TEST_ENV does.
reset_markers
cat >"$REPO/.git/hooks/pre-push" <<EOF
#!/usr/bin/env bash
cat >/dev/null   # git feeds "<local ref> <sha> <remote ref> <sha>" on stdin
exec "$GATE_GREEN_RUN" test-mac lane-cmd "PATH=\$PATH" alpha
EOF
chmod +x "$REPO/.git/hooks/pre-push"

before="$(run_count)"
( cd "$REPO" && "$GATE_GREEN_RUN" test-mac lane-cmd "PATH=$PATH" alpha ) >/dev/null 2>"$ERR"
assert_eq "ambient.gate_run_ran_the_lane" "$(( $(run_count) - before ))" "1"

before="$(run_count)"
( cd "$REPO" && git push -q origin HEAD:refs/heads/ambient ) >/dev/null 2>"$ERR"
assert_eq "ambient.push_succeeded"          "$?" "0"
assert_eq "ambient.hook_did_not_rerun_lane" "$(( $(run_count) - before ))" "0"

# ...and with no marker to lean on the same push must still run the lane, so
# the case above cannot pass against a hook that simply never fired.
reset_markers
before="$(run_count)"
( cd "$REPO" && git push -q origin HEAD:refs/heads/ambient-again ) >/dev/null 2>"$ERR"
assert_eq "ambient.hook_runs_lane_without_a_marker" "$(( $(run_count) - before ))" "1"
rm -f "$REPO/.git/hooks/pre-push"

# ---------------- markers are shared across linked worktrees ----------------
# The refinery merges in its own linked worktree; a fast-forward there produces
# the identical tree another worktree already tested. That is only a saving if
# a marker recorded in one linked worktree is visible from the other, so assert
# the sharing instead of trusting the --git-common-dir reasoning behind it.
reset_markers
LINKED="$WORK/linked"
git -C "$REPO" worktree add -q --detach "$LINKED" HEAD
before="$(run_count)"; run_gate test-mac lane-cmd alpha
assert_eq "linked.seed_run_ran_the_lane" "$(( $(run_count) - before ))" "1"
before="$(run_count)"
( cd "$LINKED" && "$GATE_GREEN_RUN" test-mac lane-cmd alpha ) >/dev/null 2>"$ERR"
assert_eq "linked.other_worktree_hits_the_marker" "$(( $(run_count) - before ))" "0"
assert_eq "linked.recorded_no_second_marker"      "$(marker_count)" "1"

# ---------------- pruning ----------------
reset_markers
run_gate test-mac lane-cmd alpha
touch -t 200001010000 "$MARKER_DIR/stale-lane.deadbeef.marker"
run_gate test-mac lane-cmd alpha
assert_true "prune.drops_expired_markers" test ! -e "$MARKER_DIR/stale-lane.deadbeef.marker"
assert_eq   "prune.keeps_the_live_marker" "$(marker_count)" "1"

# ---------------- assignment values never reach the marker ----------------
# Once make expands $(TEST_ENV) the wrapped argv is a long `env -i VAR=value`
# list — PATH, HOME, ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, OLLAMA_API_KEY.
# A marker recording that verbatim would persist whatever the caller had
# exported into a file this script re-prints to stderr on EVERY later cache
# hit, so a value must never be written. The names must survive, because they
# are what makes a marker auditable, and the key must still tell two different
# values apart — redaction is a display rule, not a weakening of the cache.
reset_markers
SECRET="s3cr3t-not-for-disk"
run_gate test-mac lane-cmd "SECRET_TOKEN=$SECRET" -p=4 --
REDACTED_MARKER="$(cat "$MARKER_DIR"/*.marker 2>/dev/null)"
assert_not_contains "redaction.marker_omits_the_value"     "$REDACTED_MARKER" "$SECRET"
assert_contains     "redaction.marker_keeps_the_name"      "$REDACTED_MARKER" "SECRET_TOKEN=<redacted>"
assert_contains     "redaction.keeps_non_assignment_args"  "$REDACTED_MARKER" "-p=4 --"

before="$(run_count)"; run_gate test-mac lane-cmd "SECRET_TOKEN=$SECRET" -p=4 --
assert_eq           "redaction.still_hits_the_marker" "$(( $(run_count) - before ))" "0"
assert_not_contains "redaction.hit_output_omits_the_value" "$(cat "$ERR")" "$SECRET"

# An assignment's value is not key material, and that is deliberate. The
# ambient case above is the reason: PATH's value provably differs between a
# gate run and the pre-push run of the same tree, so a value-sensitive key
# cannot hit across the boundary the cache exists to span. What the key keeps
# is the command's SHAPE — which lane, which runner, which flags, which
# variables are being set — and the tree SHA is what covers content.
before="$(run_count)"; run_gate test-mac lane-cmd "SECRET_TOKEN=other-value" -p=4 --
assert_eq "key.assignment_value_is_not_key_material" "$(( $(run_count) - before ))" "0"

# The NAME still is: setting a different variable is a different command, and
# a marker recorded for one must not be honored for the other.
before="$(run_count)"; run_gate test-mac lane-cmd "OTHER_TOKEN=other-value" -p=4 --
assert_eq "key.assignment_name_change_reruns" "$(( $(run_count) - before ))" "1"

# So are the non-assignment words, which is what keeps two different flag sets
# or two different runners apart.
before="$(run_count)"; run_gate test-mac lane-cmd "SECRET_TOKEN=$SECRET" -p=8 --
assert_eq "key.flag_change_reruns" "$(( $(run_count) - before ))" "1"

# ---------------- a miss explains itself ----------------
# The key is a hash, so a silent miss cannot be diagnosed from the outside:
# the only way to ask "why did these two runs not share a marker?" is to read
# the key each one looked for.
reset_markers
run_gate test-mac lane-cmd alpha
assert_contains "miss.names_the_key"  "$(cat "$ERR")" "no recorded pass for test-mac key="
run_gate test-mac lane-cmd alpha
assert_not_contains "hit.does_not_claim_a_miss" "$(cat "$ERR")" "no recorded pass"

# ---------------- Makefile wiring ----------------
# The recipe must ask "already green?" BEFORE it asks for a slot: a cache hit
# that first occupies a slot has spent the scarce resource this whole gate
# exists to ration. Ordering on the line is the assertion, not mere presence.
TEST_MAC_RECIPE="$(awk '/^test-mac:/{found=1; next} found && /^\t/{print; exit}' "$MAKEFILE")"
assert_contains "wiring.test_mac_uses_gate_green_run"      "$TEST_MAC_RECIPE" "scripts/gate-green-run test-mac"
assert_contains "wiring.test_mac_still_uses_gate_slot_run" "$TEST_MAC_RECIPE" "scripts/gate-slot-run test-mac"
green_prefix="${TEST_MAC_RECIPE%%scripts/gate-green-run*}"
slot_prefix="${TEST_MAC_RECIPE%%scripts/gate-slot-run*}"
if [[ "${#green_prefix}" -lt "${#slot_prefix}" ]]; then
    record_pass "wiring.green_check_precedes_slot_acquire"
else
    record_fail "wiring.green_check_precedes_slot_acquire" "gate-green-run must come first in: $TEST_MAC_RECIPE"
fi

echo
echo "gate-green-run tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
