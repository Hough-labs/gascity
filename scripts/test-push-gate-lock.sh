#!/usr/bin/env bash
#
# test-push-gate-lock.sh — unit tests for the pre-push gate's cross-invocation
# concurrency bound (ga-owh20p) plus static assertions that it's wired into
# scripts/test-local-parallel correctly.
#
# The slot mechanics live in scripts/push-gate-lock-lib.sh and are exercised
# directly here — no real city, no real test suite run. Cross-process denial
# is tested deterministically with flock(1) probes, so there is no reliance
# on timing EXCEPT the one case that inherently requires wall-clock: the
# wait-then-timeout path, which uses second-scale overrides
# (PUSH_GATE_MAX_WAIT_SECONDS / PUSH_GATE_POLL_SECONDS) to stay fast.
#
# Coverage: acquire/hold/deny/release, the bounded wait and its return code,
# FD inheritance (a detached descendant must not pin a slot), dead-holder
# diagnostics, the missing-flock degrade path, malformed tunables, the
# GC_PUSH_GATE_NO_CAP escape hatch, both city-root resolution modes, the
# slots-dir fallback, the non-blocking acquire path, scripts/gate-slot-run's
# busy/propagate/bypass contract, `make test`'s cmd/gc shard loop taking one
# slot for the whole loop, and static assertions that
# scripts/test-local-parallel and the Makefile gate targets wire all of it
# up — including closing the gate FD before the fan-out.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/push-gate-lock-lib.sh"
LOCAL_PARALLEL="$TEST_DIR/test-local-parallel"
GATE_SLOT_RUN="$TEST_DIR/gate-slot-run"
MAKEFILE="$(cd "$TEST_DIR/.." && pwd)/Makefile"

# shellcheck source=./push-gate-lock-lib.sh disable=SC1091
. "$LIB"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}
assert_true()  { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected true"; fi; }
assert_false() { if "${@:2}"; then record_fail "$1" "expected false"; else record_pass "$1"; fi; }
assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "missing '$needle' in: $haystack"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SLOTS="$WORK/gate-slots"

# ---------------- acquire / hold (two slots) ----------------
export PUSH_GATE_MAX_CONCURRENT=2
FD0=""
if push_gate_acquire_slot "$SLOTS" FD0 "holder-A"; then
    record_pass "acquire.first_succeeds"
else
    record_fail "acquire.first_succeeds" "expected return 0"
fi
assert_true "acquire.fd_var_set" test -n "$FD0"

FD1=""
if push_gate_acquire_slot "$SLOTS" FD1 "holder-B"; then
    record_pass "acquire.second_succeeds_distinct_slot"
else
    record_fail "acquire.second_succeeds_distinct_slot" "expected return 0 (max=2)"
fi
assert_true "acquire.second_fd_differs" test "$FD0" != "$FD1"

HOLDERS="$(push_gate_describe_slots "$SLOTS" 2)"
assert_contains "describe.has_pid_a"   "$HOLDERS" "$$"
assert_contains "describe.has_label_a" "$HOLDERS" "holder-A"
assert_contains "describe.has_label_b" "$HOLDERS" "holder-B"

# ---------------- denial while both slots held (cross-process, deterministic) ----------------
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot0" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot0"
fi
if flock -n "$SLOTS/slot-1.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot1" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot1"
fi
CHILD_HELD="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-C; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "deny.lib_from_child_zero_wait" "$CHILD_HELD" "DENIED"

# ---------------- all slots busy: waits, prints diagnostics, then times out with rc 1 ----------------
# The child exits with push_gate_acquire_slot's own status so CHILD_RC asserts
# the LIBRARY's return code, not the child shell's last echo.
START="$(date +%s)"
CHILD_OUT="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=2 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-D; rc=$?; if [[ "$rc" -eq 0 ]]; then echo ACQUIRED; else echo "DENIED rc=$rc"; fi; exit "$rc"' 2>&1)"
CHILD_RC=$?
ELAPSED=$(( $(date +%s) - START ))
assert_contains "wait.busy_message_immediate" "$CHILD_OUT" "busy"
assert_contains "wait.reports_holder_a"        "$CHILD_OUT" "holder-A"
assert_contains "wait.timeout_message"         "$CHILD_OUT" "timed out"
assert_contains "wait.eventually_denied"       "$CHILD_OUT" "DENIED"
assert_true     "wait.elapsed_at_least_bound"  test "$ELAPSED" -ge 2
assert_eq       "wait.library_returns_1_on_timeout" "$CHILD_RC" "1"

# ---------------- release -> reacquire ----------------
push_gate_release_slot "$FD0"
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_pass "release.flock_probe_succeeds"
else
    record_fail "release.flock_probe_succeeds" "could not acquire slot-0 after release"
fi
CHILD_FREE="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-E; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "release.lib_from_child" "$CHILD_FREE" "ACQUIRED"
push_gate_release_slot "$FD1"

# ---------------- FD inheritance: a live detached descendant must not pin a slot ----------------
# scripts/test-local-parallel closes the gate FD before the fan-out so jobs
# never inherit it (asserted statically below). This asserts the library half
# of that contract: releasing unlocks the open-file-description itself, so
# even a descendant that did inherit a copy — a daemonized tmux server, a
# dolt sql-server, an escaped `gc` — stops holding the slot the moment the
# gate releases it.
INHERIT_SLOTS="$WORK/inherit-slots"
FD_INH=""
if push_gate_acquire_slot "$INHERIT_SLOTS" FD_INH "holder-inherit"; then
    if command -v setsid >/dev/null 2>&1; then
        setsid bash -c 'sleep 3' &
    else
        bash -c 'sleep 3' &
    fi
    INHERIT_CHILD=$!
    push_gate_release_slot "$FD_INH"
    if flock -n "$INHERIT_SLOTS/slot-0.lock" -c 'exit 0'; then
        record_pass "inherit.detached_descendant_does_not_pin_slot"
    else
        record_fail "inherit.detached_descendant_does_not_pin_slot" \
            "slot still held after release while a detached descendant is alive"
    fi
    kill "$INHERIT_CHILD" 2>/dev/null || true
    wait "$INHERIT_CHILD" 2>/dev/null || true
else
    record_fail "inherit.detached_descendant_does_not_pin_slot" "could not acquire a slot to set up the case"
fi

# ---------------- describe: a held slot whose recorded PID is gone is flagged ----------------
# Hold a slot for real, then overwrite its holder line with a PID that cannot
# exist — the exact shape a leaked descendant leaves behind: lock genuinely
# held, recorded holder long gone.
DEAD_SLOTS="$WORK/dead-slots"
FD_DEAD=""
if push_gate_acquire_slot "$DEAD_SLOTS" FD_DEAD "holder-dead"; then
    printf '%s %s %s %s\n' "999999999" "1970-01-01T00:00:00Z" "holder-dead" "somehost" >"$DEAD_SLOTS/slot-0.lock"
    DEAD_OUT="$(push_gate_describe_slots "$DEAD_SLOTS" 1)"
    assert_contains "describe.flags_dead_holder" "$DEAD_OUT" "holder pid dead"
    push_gate_release_slot "$FD_DEAD"
else
    record_fail "describe.flags_dead_holder" "could not acquire a slot to set up the case"
fi

# ---------------- missing flock(1): degrade best-effort, never block ----------------
# PATH is stripped of every directory so `command -v flock` fails; the library
# must warn and return 0 with an empty FD rather than burn the wait bound.
mkdir -p "$WORK/empty-bin"
# shellcheck disable=SC2016  # $? and $z are the child shell's, evaluated there
NOFLOCK_OUT="$(LIB="$LIB" DIR="$WORK/noflock-slots" PATH="$WORK/empty-bin" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    "$BASH" -c '. "$LIB"; z=preset; push_gate_acquire_slot "$DIR" z holder-F; echo "rc=$? fd=[$z]"' 2>&1)"
assert_contains "no_flock.warns_and_names_flock" "$NOFLOCK_OUT" "flock(1) not found"
assert_contains "no_flock.returns_zero_empty_fd" "$NOFLOCK_OUT" "rc=0 fd=[]"

# ---------------- mkdir failure: degrade best-effort, never misreport as timeout ----------------
# The original bug (ga-5enlx8): a linked worktree's .git is a FILE, so the
# slots-dir fallback resolved under it and mkdir -p could never succeed. The
# old code mapped that mkdir failure to the same `return 1` as a real
# wait-bound timeout, so operators chased fleet contention that did not
# exist. A blocked FILE (not a permission bit, so this holds even as root)
# stands in for that unwritable-parent case.
BLOCKED_PARENT="$WORK/blocked-parent"
: >"$BLOCKED_PARENT"
MKDIRFAIL_OUT="$(LIB="$LIB" DIR="$BLOCKED_PARENT/gate-slots" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=5 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; z=preset; push_gate_acquire_slot "$DIR" z holder-G; echo "rc=$? fd=[$z]"' 2>&1)"
assert_contains "mkdir_fail.warns_cannot_create_slot_dir" "$MKDIRFAIL_OUT" "cannot create slot dir"
assert_contains "mkdir_fail.returns_zero_empty_fd"        "$MKDIRFAIL_OUT" "rc=0 fd=[]"
# The misreporting was the actual harm, so assert the absence of the timeout
# message directly rather than relying on rc=0 to imply it.
case "$MKDIRFAIL_OUT" in
    *"timed out"*)
        record_fail "mkdir_fail.never_reports_timeout" "found 'timed out' in output: $MKDIRFAIL_OUT" ;;
    *)
        record_pass "mkdir_fail.never_reports_timeout" ;;
esac

# ---------------- malformed tunables fall back to their documented defaults ----------------
# Each bad value must be rejected by name and replaced, never fed to
# arithmetic (`-1`, `abc`) or turned into a busy loop / zero-slot sweep (`0`).
for bad_case in "empty:" "zero:0" "negative:-1" "nonnumeric:abc"; do
    bad_name="${bad_case%%:*}"
    bad_val="${bad_case#*:}"
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-max-$bad_name" \
        PUSH_GATE_MAX_CONCURRENT="$bad_val" PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
        bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.max_concurrent_${bad_name}_still_acquires" "$TUNE_OUT" "rc=0"
    # An empty value is already covered by the `${VAR:-default}` expansion, so
    # only the values that actually reach validation emit the warning.
    if [[ -n "$bad_val" ]]; then
        assert_contains "tunables.max_concurrent_${bad_name}_warns_by_name" "$TUNE_OUT" "PUSH_GATE_MAX_CONCURRENT"
    fi
done
for bad_tunable in PUSH_GATE_MAX_WAIT_SECONDS PUSH_GATE_POLL_SECONDS; do
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-$bad_tunable" BAD="$bad_tunable" \
        bash -c 'export "$BAD=abc"; . "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.${bad_tunable}_warns_by_name" "$TUNE_OUT" "$bad_tunable"
    assert_contains "tunables.${bad_tunable}_still_acquires" "$TUNE_OUT" "rc=0"
done

# ---------------- escape hatch: GC_PUSH_GATE_NO_CAP bypasses the cap entirely ----------------
NOCAP_OUT="$(LIB="$LIB" DIR="$SLOTS" GC_PUSH_GATE_NO_CAP=1 PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "no_cap.bypasses_full_slots" "$NOCAP_OUT" "ACQUIRED"

# ---------------- city-root resolution: env short-circuit ----------------
CITY_ENV="$WORK/city-env"
mkdir -p "$CITY_ENV"
: >"$CITY_ENV/city.toml"
RESOLVED_ENV="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_city_root)"
assert_eq "city_root.env_short_circuit" "$RESOLVED_ENV" "$CITY_ENV"

# An env var pointing at a directory with neither city.toml nor .gc/ must NOT
# be trusted verbatim — it should fall through to walk-up instead of
# returning garbage. Bounded entirely inside $WORK (HOME=$WORK as the
# ceiling) so this can't accidentally pick up a real city.toml somewhere in
# the ambient filesystem's actual ancestry.
BOGUS="$WORK/not-a-city"
mkdir -p "$BOGUS/nested"
if RESOLVED_BOGUS="$(cd "$BOGUS/nested" && GC_CITY_PATH="$BOGUS" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_city_root)"; then
    assert_true "city_root.rejects_unvalidated_env" test "$RESOLVED_BOGUS" != "$BOGUS"
else
    record_pass "city_root.rejects_unvalidated_env"
fi

# ---------------- city-root resolution: walk-up discovery ----------------
CITY_WALK="$WORK/city-walk"
mkdir -p "$CITY_WALK/rigs/proj/sub"
: >"$CITY_WALK/city.toml"
RESOLVED_WALK="$(cd "$CITY_WALK/rigs/proj/sub" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_city_root)"
assert_eq "city_root.walk_up_finds_ancestor" "$RESOLVED_WALK" "$CITY_WALK"

# ---------------- slots dir: derives from city root, falls back to repo-relative ----------------
SLOTS_FROM_CITY="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_slots_dir)"
assert_eq "slots_dir.under_city_root" "$SLOTS_FROM_CITY" "$CITY_ENV/.gc/gate-slots"

NOCITY="$WORK/no-city-repo"
mkdir -p "$NOCITY"
(cd "$NOCITY" && git init -q .) 2>/dev/null || true
SLOTS_FALLBACK="$(cd "$NOCITY" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
assert_eq "slots_dir.falls_back_to_repo_relative" "$SLOTS_FALLBACK" "$NOCITY/.git/gate-slots"

LINKED_REPO="$WORK/linked-repo"
LINKED_A="$WORK/linked-a"
LINKED_B="$WORK/linked-b"
mkdir -p "$LINKED_REPO"
git -C "$LINKED_REPO" init -q
git -C "$LINKED_REPO" config user.name "Push Gate Test"
git -C "$LINKED_REPO" config user.email "push-gate-test@example.invalid"
: >"$LINKED_REPO/tracked"
git -C "$LINKED_REPO" add tracked
git -C "$LINKED_REPO" commit -qm "seed linked worktree fixture"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_A"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_B"

SLOTS_LINKED_A="$(cd "$LINKED_A" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
SLOTS_LINKED_B="$(cd "$LINKED_B" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
LINKED_COMMON="$(cd "$LINKED_REPO/.git" && pwd -P)"
SLOTS_LINKED_A_N="$(cd "$(dirname "$SLOTS_LINKED_A")" && pwd -P)/gate-slots"
assert_eq "slots_dir.linked_worktree_uses_common_git_dir" "$SLOTS_LINKED_A_N" "$LINKED_COMMON/gate-slots"
assert_eq "slots_dir.linked_worktrees_share_slots" "$SLOTS_LINKED_B" "$SLOTS_LINKED_A"

FD_LINKED=""
if push_gate_acquire_slot "$SLOTS_LINKED_A" FD_LINKED "linked-holder"; then
    record_pass "slots_dir.linked_worktree_can_acquire"
    push_gate_release_slot "$FD_LINKED"
else
    record_fail "slots_dir.linked_worktree_can_acquire" "resolved slot directory is not usable: $SLOTS_LINKED_A"
fi

# ---------------- non-blocking acquire: says so, and never claims a timeout ----------------
# The heavy Makefile gates (make test / make test-mac) acquire non-blocking
# (PUSH_GATE_MAX_WAIT_SECONDS=0), because an agent that blocks on the lock
# under a harness timeout gets SIGTERM'd having run zero tests and relaunches
# — the retry loop this cap exists to end, now burning the wait instead of the
# work. That path reported itself as "waiting up to 0s" and then "timed out
# after 0s", describing a wait that expired when it had never waited at all;
# an operator reading that reasonably concludes the host is saturated for
# ten minutes rather than that one slot was busy for one instant.
PUSH_GATE_MAX_CONCURRENT=1
NB_SLOTS="$WORK/nonblocking-slots"
FD_NB=""
if push_gate_acquire_slot "$NB_SLOTS" FD_NB "holder-NB"; then
    NB_OUT="$(LIB="$LIB" DIR="$NB_SLOTS" PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
        bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-NB2; echo "rc=$?"' 2>&1)"
    assert_contains "nonblocking.announces_not_waiting" "$NB_OUT" "not waiting"
    assert_contains "nonblocking.names_current_holder"  "$NB_OUT" "holder-NB"
    assert_contains "nonblocking.returns_1"             "$NB_OUT" "rc=1"
    case "$NB_OUT" in
        *"timed out"*)
            record_fail "nonblocking.never_reports_timeout" "found 'timed out' in output: $NB_OUT" ;;
        *)
            record_pass "nonblocking.never_reports_timeout" ;;
    esac
    push_gate_release_slot "$FD_NB"
else
    record_fail "nonblocking.announces_not_waiting" "could not acquire a slot to set up the case"
fi

# ---------------- scripts/gate-slot-run: the Makefile gate wrapper ----------------
# gascity-6tr: `make test` and `make test-mac` reach go-test-observable
# directly, so the Darwin lane every agent actually runs bypassed the cap that
# only test-local-parallel was wired to. gate-slot-run is the wrapper that
# closes that gap at the Makefile entrypoint layer.
GSR_CITY="$WORK/gsr-city"
mkdir -p "$GSR_CITY"
: >"$GSR_CITY/city.toml"
GSR_SLOTS="$GSR_CITY/.gc/gate-slots"
# One-slot lane, deterministic env: the ambient GC_*/CI vars must not decide
# whether these assertions exercise the capped path or a bypass.
GSR_ENV=(
    GC_CITY_PATH="$GSR_CITY" GC_CITY="" GC_CITY_ROOT=""
    GC_PUSH_GATE_NO_CAP="" CI="" GITHUB_ACTIONS=""
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_POLL_SECONDS=1
)

assert_true "gate_slot_run.is_executable" test -x "$GATE_SLOT_RUN"

GSR_OUT="$(env "${GSR_ENV[@]}" bash "$GATE_SLOT_RUN" selftest-lane sh -c 'echo RAN' 2>&1)"
GSR_RC=$?
assert_contains "gate_slot_run.runs_command"        "$GSR_OUT" "RAN"
assert_eq       "gate_slot_run.propagates_success"  "$GSR_RC"  "0"

env "${GSR_ENV[@]}" bash "$GATE_SLOT_RUN" selftest-lane sh -c 'exit 3' >/dev/null 2>&1
assert_eq "gate_slot_run.propagates_command_status" "$?" "3"

env "${GSR_ENV[@]}" bash "$GATE_SLOT_RUN" only-a-lane >/dev/null 2>&1
assert_eq "gate_slot_run.usage_error_exits_2" "$?" "2"

# The gate command must not inherit the slot descriptor: `go test` spawns test
# binaries that leak daemons (a tmux server, a dolt sql-server, an escaped
# `gc`), and any of them holding a copy pins the slot past this invocation.
# Probing PUSH_GATE_FD_BASE directly asserts the sever itself — releasing the
# lock on exit would mask a missing sever, since flock -u frees the shared
# open-file-description for inheritors too.
FD_PROBE="$(env "${GSR_ENV[@]}" bash "$GATE_SLOT_RUN" selftest-lane \
    bash -c 'if ( true <&'"$PUSH_GATE_FD_BASE"' ) 2>/dev/null; then echo FD_INHERITED; else echo FD_CLOSED; fi' 2>&1)"
assert_contains "gate_slot_run.severs_slot_fd_inheritance" "$FD_PROBE" "FD_CLOSED"

# Lane fully occupied: BUSY is neither a pass nor a test failure, and the gate
# command must not have run at all.
FD_GSR=""
if push_gate_acquire_slot "$GSR_SLOTS" FD_GSR "holder-occupies-lane"; then
    BUSY_OUT="$(env "${GSR_ENV[@]}" bash "$GATE_SLOT_RUN" selftest-lane sh -c 'echo SHOULD_NOT_RUN' 2>&1)"
    BUSY_RC=$?
    assert_eq       "gate_slot_run.busy_exits_75"           "$BUSY_RC"  "75"
    assert_contains "gate_slot_run.busy_says_gate_busy"     "$BUSY_OUT" "GATE BUSY"
    assert_contains "gate_slot_run.busy_says_indeterminate" "$BUSY_OUT" "INDETERMINATE"
    case "$BUSY_OUT" in
        *SHOULD_NOT_RUN*)
            record_fail "gate_slot_run.busy_does_not_run_command" "the gate command ran on a busy lane" ;;
        *)
            record_pass "gate_slot_run.busy_does_not_run_command" ;;
    esac

    NOCAP_OUT_RUN="$(env "${GSR_ENV[@]}" GC_PUSH_GATE_NO_CAP=1 bash "$GATE_SLOT_RUN" selftest-lane sh -c 'echo RAN_UNCAPPED' 2>&1)"
    assert_contains "gate_slot_run.no_cap_bypasses_busy_lane" "$NOCAP_OUT_RUN" "RAN_UNCAPPED"

    # CI runs one job per box; queueing behind itself would turn a green build
    # into an exit-75 red for a contention condition that cannot occur there.
    CI_OUT_RUN="$(env "${GSR_ENV[@]}" GITHUB_ACTIONS=true bash "$GATE_SLOT_RUN" selftest-lane sh -c 'echo RAN_IN_CI' 2>&1)"
    assert_contains "gate_slot_run.ci_bypasses_busy_lane" "$CI_OUT_RUN" "RAN_IN_CI"

    push_gate_release_slot "$FD_GSR"
else
    record_fail "gate_slot_run.busy_exits_75" "could not occupy the lane to set up the case"
fi
PUSH_GATE_MAX_CONCURRENT=2

# ---------------- static wiring assertions against the Makefile gate targets ----------------
# The uncapped lane was the defect: the cap existed and worked, but `make test`
# and `make test-mac` never called it. Assert the wiring, not just the library.
assert_true "wiring.gate_slot_run_sources_lib"  grep -q 'push-gate-lock-lib.sh' "$GATE_SLOT_RUN"
assert_true "wiring.gate_slot_run_acquires"     grep -q 'push_gate_acquire_slot' "$GATE_SLOT_RUN"
assert_true "wiring.gate_slot_run_exits_75"     grep -q 'exit 75'               "$GATE_SLOT_RUN"
assert_true "wiring.gate_slot_run_has_override" grep -q 'GC_PUSH_GATE_NO_CAP'   "$GATE_SLOT_RUN"
assert_true "wiring.gate_slot_run_releases_slot_on_exit" \
    grep -qE 'trap .*push_gate_release_slot.*EXIT' "$GATE_SLOT_RUN"

# Print a Makefile target's recipe lines (the tab-indented block after it).
makefile_recipe() {
    awk -v target="$1" '
        index($0, target ":") == 1 { found = 1; next }
        found && /^\t/ { print; next }
        found { exit }
    ' "$MAKEFILE"
}

for gate_target in test test-mac; do
    RECIPE="$(makefile_recipe "$gate_target")"
    assert_contains "wiring.make_${gate_target}_uses_gate_slot_run" "$RECIPE" "scripts/gate-slot-run"
    # The wrapper must sit OUTSIDE the env -i TEST_ENV allowlist, or it cannot
    # see GC_PUSH_GATE_NO_CAP, the CI markers, or GC_SESSION_NAME for the
    # holder label — the cap would then be unbypassable and unattributable.
    assert_true "wiring.make_${gate_target}_gates_before_test_env" \
        test "${RECIPE%%scripts/gate-slot-run*}" "!=" "$RECIPE"
    if [[ "${RECIPE%%scripts/gate-slot-run*}" == *'$(TEST_ENV)'* ]]; then
        record_fail "wiring.make_${gate_target}_gate_outside_env_i" "gate-slot-run runs inside TEST_ENV's env -i: $RECIPE"
    else
        record_pass "wiring.make_${gate_target}_gate_outside_env_i"
    fi
    # Capping must not have replaced the observable runner it wraps.
    assert_contains "wiring.make_${gate_target}_keeps_observable_runner" "$RECIPE" "scripts/go-test-observable"
done

# ---------------- the `test` target's cmd/gc shard loop ----------------
# `make test` grew a SECOND command when cmd/gc left the package sweep
# (gascity-cgh): a sequential shard loop. Make gives every recipe line its own
# shell, so the sweep's slot cannot span the loop and the loop has to take one
# of its own. Pin both that it is capped at all, and that it takes ONE slot for
# the whole loop rather than one per shard — the acquire is non-blocking, so an
# acquire inside the loop body would make every shard another chance to abort a
# gate that is already running, and would drop the lane between shards.
TEST_RECIPE="$(makefile_recipe test)"
assert_eq "wiring.make_test_caps_both_commands" \
    "$(grep -c 'scripts/gate-slot-run' <<<"$TEST_RECIPE")" "2"
assert_contains "wiring.make_test_shard_loop_covers_cmd_gc" \
    "$TEST_RECIPE" "./scripts/test-go-test-shard ./cmd/gc"
assert_contains "wiring.make_test_shard_loop_is_capped" \
    "$(grep 'for s in' <<<"$TEST_RECIPE")" "scripts/gate-slot-run"
case "${TEST_RECIPE#*for s in}" in
    *scripts/gate-slot-run*)
        record_fail "wiring.make_test_shard_loop_takes_one_slot" \
            "gate-slot-run appears inside the shard loop, so the loop acquires a slot per shard" ;;
    *)
        record_pass "wiring.make_test_shard_loop_takes_one_slot" ;;
esac

# Re-applying this wrapper over a STALE recipe body is how this change was
# rejected once already — the wrapper landed on the pre-cgh `./...` sweep. Pin
# the post-cgh body so a future rebase cannot quietly undo gascity-cgh here.
TEST_SWEEP_LINE="$(grep 'scripts/go-test-observable test --' <<<"$TEST_RECIPE")"
assert_contains "wiring.make_test_sweep_excludes_sharded_pkgs" "$TEST_SWEEP_LINE" '$(UNIT_PKGS_SWEEP)'
case "$TEST_SWEEP_LINE" in
    *'./...'*)
        record_fail "wiring.make_test_sweep_drops_dot_dot_dot" \
            "sweep still passes ./..., pulling cmd/gc back under one deadline: $TEST_SWEEP_LINE" ;;
    *)
        record_pass "wiring.make_test_sweep_drops_dot_dot_dot" ;;
esac
assert_contains "wiring.make_test_mac_shares_the_package_list" \
    "$(makefile_recipe test-mac)" '$(UNIT_PKGS_SWEEP)'

# Darwin runs no separate examples job, so test-mac is the only thing that runs
# $(SHARDED_EXAMPLE_PKGS) at all (gascity-vdhw). Dropping the loop from this
# target would not fail anything — the packages would just silently stop being
# tested on the lane agents actually use.
assert_contains "wiring.make_test_mac_runs_the_example_shards" \
    "$(makefile_recipe test-mac)" 'for p in $(SHARDED_EXAMPLE_PKGS)'
assert_eq "wiring.make_test_mac_takes_exactly_one_slot" \
    "$(grep -c 'scripts/gate-slot-run' <<<"$(makefile_recipe test-mac)")" "1"

# ---------------- behavioural: the wrapped shard loop actually runs ----------------
# Taking one slot for the whole loop costs a `$(SHELL) -c '...'`, because
# gate-slot-run execs a command and a `for` loop is not one. That puts
# TEST_ENV's env -i allowlist, $$s and the backslash continuations inside
# single quotes, where a quoting slip changes what runs without changing what
# the recipe looks like. So run the Makefile's own shard-loop recipe lines,
# lifted verbatim rather than restated, against stubs.
SHARD_PROBE="$WORK/shard-loop-probe"
mkdir -p "$SHARD_PROBE/scripts"
cat >"$SHARD_PROBE/scripts/gate-slot-run" <<'PROBE_WRAPPER'
#!/bin/sh
# Stands in for the real wrapper (whose own contract is asserted above): logs
# one line per acquisition so the loop's slot granularity is observable, then
# runs the gate command verbatim and propagates its status.
echo "ACQUIRE $1" >>"$PROBE_LOG"
probe_lane="$1"
shift
"$@"
probe_rc=$?
echo "RELEASE $probe_lane $probe_rc" >>"$PROBE_LOG"
exit $probe_rc
PROBE_WRAPPER
cat >"$SHARD_PROBE/scripts/test-go-test-shard" <<'PROBE_SHARD'
#!/bin/sh
echo "SHARD $2/$3 $1 fast=$GC_FAST_UNIT count=$GO_TEST_COUNT timeout=$GO_TEST_TIMEOUT" >>"$PROBE_LOG"
[ "$2" = "${PROBE_FAIL_SHARD:-}" ] && exit 7
exit 0
PROBE_SHARD
chmod +x "$SHARD_PROBE/scripts/gate-slot-run" "$SHARD_PROBE/scripts/test-go-test-shard"
{
    printf 'TEST_ENV = env -i PATH="$$PATH" PROBE_LOG="$$PROBE_LOG" PROBE_FAIL_SHARD="$${PROBE_FAIL_SHARD-}"\n'
    printf 'CMD_GC_UNIT_TOTAL ?= 3\n'
    printf 'SHARDED_EXAMPLE_PKGS = ./examples/one ./examples/two\n'
    printf 'EXAMPLES_UNIT_TOTAL ?= 2\n'
    printf 'probe:\n'
    awk '
        /^test:/ { in_target = 1; next }
        in_target && /^\t/ {
            if (index($0, "for s in") > 0) { capture = 1 }
            if (capture) { print }
            next
        }
        in_target { exit }
    ' "$MAKEFILE"
} >"$SHARD_PROBE/Makefile"

PROBE_LOG="$SHARD_PROBE/log"
: >"$PROBE_LOG"
if ( cd "$SHARD_PROBE" && PROBE_LOG="$PROBE_LOG" make probe ) >/dev/null 2>&1; then
    record_pass "shard_loop.clean_run_succeeds"
else
    record_fail "shard_loop.clean_run_succeeds" "make probe failed; log: $(cat "$PROBE_LOG")"
fi
assert_eq "shard_loop.takes_one_slot_for_the_whole_loop" "$(grep -c '^ACQUIRE ' "$PROBE_LOG")" "1"
# 3 cmd/gc shards + EXAMPLES_UNIT_TOTAL shards for each of the two example
# packages. One slot still covers all of them.
assert_eq "shard_loop.runs_every_shard"                  "$(grep -c '^SHARD '   "$PROBE_LOG")" "7"
assert_contains "shard_loop.passes_shard_index_and_total" "$(cat "$PROBE_LOG")" "SHARD 2/3 ./cmd/gc"
assert_contains "shard_loop.shards_the_first_example_pkg" "$(cat "$PROBE_LOG")" "SHARD 2/2 ./examples/one"
assert_contains "shard_loop.shards_the_second_example_pkg" "$(cat "$PROBE_LOG")" "SHARD 2/2 ./examples/two"
assert_contains "shard_loop.keeps_fast_unit_budget"       "$(cat "$PROBE_LOG")" "fast=1 count=1 timeout=15m"

# `|| exit 1` inside the quoted loop is exactly what a quoting slip drops, and
# dropping it would let a red cmd/gc shard pass the gate.
: >"$PROBE_LOG"
( cd "$SHARD_PROBE" && PROBE_LOG="$PROBE_LOG" PROBE_FAIL_SHARD=2 make probe ) >/dev/null 2>&1
assert_true "shard_loop.failing_shard_fails_the_gate" test "$?" -ne 0
assert_eq "shard_loop.failing_shard_stops_the_loop" "$(grep -c '^SHARD ' "$PROBE_LOG")" "2"

# ---------------- behavioural: test-mac's single-slot sweep + shard loop ----------------
# test-mac is the Darwin lane agents run as their configured test_command, and
# gascity-vdhw put a shard loop in it for $(SHARDED_EXAMPLE_PKGS). Keeping the
# fail-in-under-a-second property meant chaining sweep and shards with `&&`
# inside ONE `$(SHELL) -c` rather than adding a second recipe line, so the
# whole target is now a single quoted string: the same class of quoting slip
# the cmd/gc probe above guards, over a recipe that also has to short-circuit.
MAC_PROBE="$WORK/mac-gate-probe"
mkdir -p "$MAC_PROBE/scripts"
cp "$SHARD_PROBE/scripts/gate-slot-run" "$MAC_PROBE/scripts/gate-slot-run"
cp "$SHARD_PROBE/scripts/test-go-test-shard" "$MAC_PROBE/scripts/test-go-test-shard"
cat >"$MAC_PROBE/scripts/go-test-observable" <<'PROBE_SWEEP'
#!/bin/sh
echo "SWEEP $*" >>"$PROBE_LOG"
[ -n "${PROBE_FAIL_SWEEP:-}" ] && exit 9
exit 0
PROBE_SWEEP
# The real gate-green-run, not a stub: it is the outermost word of the recipe
# (gascity-nuw), so the probe only reproduces the lane's real composition if
# the wrapper it runs through is the real one. GATE_GREEN_NO_CACHE below keeps
# it deterministic — the probe dir is a throwaway outside any repo and so is
# uncacheable anyway, but a TMPDIR that happened to sit inside one must not let
# a marker recorded by the first `make probe` skip the second's sweep.
cp "$TEST_DIR/gate-green-run" "$MAC_PROBE/scripts/gate-green-run"
chmod +x "$MAC_PROBE/scripts/gate-slot-run" "$MAC_PROBE/scripts/test-go-test-shard" \
    "$MAC_PROBE/scripts/go-test-observable" "$MAC_PROBE/scripts/gate-green-run"
{
    printf 'TEST_ENV = env -i PATH="$$PATH" PROBE_LOG="$$PROBE_LOG" PROBE_FAIL_SHARD="$${PROBE_FAIL_SHARD-}" PROBE_FAIL_SWEEP="$${PROBE_FAIL_SWEEP-}"\n'
    printf 'UNIT_PKGS_SWEEP = ./pkg/a ./pkg/b\n'
    printf 'SHARDED_EXAMPLE_PKGS = ./examples/one ./examples/two\n'
    printf 'EXAMPLES_UNIT_TOTAL ?= 2\n'
    # gascity-ngab moved the real sweep's two concurrency bounds into
    # $(GATE_TEST_P)/$(GATE_TEST_PARALLEL). This probe copies that recipe line
    # verbatim, so a variable the throwaway Makefile does not define expands to
    # EMPTY: the probe then exercises `-p= -parallel=`, a shape production never
    # runs, while every assertion below still passes. Pinned literals rather
    # than $(shell ./scripts/test-gate-parallelism ...) because the probe wants
    # one fixed host-independent shape; mac_gate.no_flag_expands_empty is what
    # fails if a later variable joins the recipe and not this list.
    printf 'GATE_TEST_P ?= 4\n'
    printf 'GATE_TEST_PARALLEL ?= 4\n'
    printf 'probe:\n'
    awk '
        /^test-mac:/ { in_target = 1; next }
        in_target && /^\t/ { print; next }
        in_target { exit }
    ' "$MAKEFILE"
} >"$MAC_PROBE/Makefile"

PROBE_LOG="$MAC_PROBE/log"
: >"$PROBE_LOG"
if ( cd "$MAC_PROBE" && PROBE_LOG="$PROBE_LOG" GATE_GREEN_NO_CACHE=1 make probe ) >/dev/null 2>&1; then
    record_pass "mac_gate.clean_run_succeeds"
else
    record_fail "mac_gate.clean_run_succeeds" "make probe failed; log: $(cat "$PROBE_LOG")"
fi
assert_eq "mac_gate.takes_one_slot_for_sweep_and_shards" "$(grep -c '^ACQUIRE ' "$PROBE_LOG")" "1"
assert_eq "mac_gate.runs_the_sweep_once"                 "$(grep -c '^SWEEP '   "$PROBE_LOG")" "1"
assert_eq "mac_gate.runs_every_example_shard"            "$(grep -c '^SHARD '   "$PROBE_LOG")" "4"
assert_contains "mac_gate.sweep_keeps_the_package_list"  "$(cat "$PROBE_LOG")" "SWEEP test-mac -- "
assert_contains "mac_gate.sweep_carries_the_gate_bounds" "$(cat "$PROBE_LOG")" "-p=4 -parallel=4"
# The general form of the assertion above. An undefined Makefile variable
# expands to empty and leaves a bare `-flag=` in the argv, so match any such
# flag rather than only today's two: the next variable added to the recipe then
# fails here instead of silently thinning what this probe covers.
assert_false "mac_gate.no_flag_expands_empty" \
    grep -qE '(^|[[:space:]])-[A-Za-z-]+=([[:space:]]|$)' "$PROBE_LOG"
assert_contains "mac_gate.shards_the_first_example_pkg"  "$(cat "$PROBE_LOG")" "SHARD 2/2 ./examples/one"
assert_contains "mac_gate.shards_the_second_example_pkg" "$(cat "$PROBE_LOG")" "SHARD 2/2 ./examples/two"
assert_contains "mac_gate.shards_keep_fast_unit_budget"  "$(cat "$PROBE_LOG")" "fast=1 count=1 timeout=15m"

# A red sweep must fail the gate before any shard runs — that is what the `&&`
# buys, and dropping it would report a green gate off the shards alone.
: >"$PROBE_LOG"
( cd "$MAC_PROBE" && PROBE_LOG="$PROBE_LOG" GATE_GREEN_NO_CACHE=1 PROBE_FAIL_SWEEP=1 make probe ) >/dev/null 2>&1
assert_true "mac_gate.failing_sweep_fails_the_gate" test "$?" -ne 0
assert_eq "mac_gate.failing_sweep_skips_the_shards" "$(grep -c '^SHARD ' "$PROBE_LOG")" "0"

# `|| exit 1` inside the quoted loop is what a quoting slip drops; without it a
# red example shard would pass the Darwin gate.
: >"$PROBE_LOG"
( cd "$MAC_PROBE" && PROBE_LOG="$PROBE_LOG" GATE_GREEN_NO_CACHE=1 PROBE_FAIL_SHARD=2 make probe ) >/dev/null 2>&1
assert_true "mac_gate.failing_shard_fails_the_gate" test "$?" -ne 0
assert_eq "mac_gate.failing_shard_stops_the_loop" "$(grep -c '^SHARD ' "$PROBE_LOG")" "2"

# ---------------- static wiring assertions against test-local-parallel ----------------
assert_true "wiring.sources_lib"   grep -q 'push-gate-lock-lib.sh' "$LOCAL_PARALLEL"
assert_true "wiring.calls_acquire" grep -q 'push_gate_acquire_slot' "$LOCAL_PARALLEL"
assert_true "wiring.has_override"  grep -q 'GC_PUSH_GATE_NO_CAP'   "$LOCAL_PARALLEL"

acq_line="$(grep -n 'push_gate_acquire_slot ' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$acq_line" ]]; then
    LOSER_BLOCK="$(sed -n "${acq_line},$((acq_line + 10))p" "$LOCAL_PARALLEL")"
    assert_contains "wiring.timeout_exits_75" "$LOSER_BLOCK" "exit 75"
else
    record_fail "wiring.timeout_exits_75" "no push_gate_acquire_slot call found in $LOCAL_PARALLEL"
fi
# Must be the release call wired to an EXIT trap specifically — not just any
# trap and any release call existing independently somewhere in the file
# (e.g. an unrelated per-job cleanup trap for a temp dir).
assert_true "wiring.releases_slot_on_exit" grep -qE 'trap .*push_gate_release_slot.*EXIT' "$LOCAL_PARALLEL"

# The gate FD must be closed BEFORE the job fan-out, or every job — and every
# daemon a job leaks — inherits a copy and can pin the slot past this
# invocation's death. Line ordering is the assertion: a close that lands after
# the fan-out is worthless.
close_line="$(grep -nE 'exec \$\{gate_fd\}>&-' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
fanout_line="$(grep -n 'xargs -0' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$close_line" && -n "$fanout_line" && "$close_line" -lt "$fanout_line" ]]; then
    record_pass "wiring.closes_gate_fd_before_fanout"
else
    record_fail "wiring.closes_gate_fd_before_fanout" \
        "close at line '${close_line:-none}', fan-out at line '${fanout_line:-none}'"
fi

echo
echo "push-gate-lock tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
