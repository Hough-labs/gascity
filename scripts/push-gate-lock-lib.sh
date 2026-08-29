#!/usr/bin/env bash
#
# push-gate-lock-lib.sh — cross-invocation concurrency bound for the heavy
# local test suites (ga-owh20p).
#
# WHY THIS EXISTS
#   Two measured incidents (2026-07-14 load 88.07 with 5 concurrent
#   test-fast-parallel runs + 2 gates + 1 `make test`; 2026-07-2x load
#   53.6-82.1 with ~20 concurrent gate processes) show the pre-push gate has
#   zero cross-invocation concurrency control: any number of pushes, direct
#   `make` runs, and CI jobs can pile onto the same host at once, producing
#   false-red failures (timeouts, OOM-adjacent slowdowns) indistinguishable
#   from real regressions. This is the same recurring flake cluster
#   documented repeatedly since 2026-07-14 (see bd ga-owh20p notes).
#
#   This is one of three orthogonal axes: (1) within-run job sizing
#   (scripts/test-local-job-count, existing), (2) per-invocation resource
#   isolation (systemd-run --slice, existing), (3) cross-invocation
#   concurrency bound — this file. See TESTING.md for all three.
#
# MECHANISM
#   Adapted from packs/maintainer-pr-review/scripts/run-lock-lib.sh's
#   mpr_acquire_global_slot in the gc-management meta-repo (numbered
#   flock(1) slot files under a slot directory; the kernel releases the
#   lock when the last descriptor on the open-file-description is closed or
#   unlocked, so a normal exit, a test failure, and a crash all free the
#   slot alike — no PID-file liveness probing needed).
#
#   FD inheritance is deliberately severed at the fan-out boundary in
#   scripts/test-local-parallel: the slot FD is closed inside the subshell
#   that spawns the job fan-out, so no test job — and no daemon a test job
#   leaks (a tmux server, a dolt sql-server, an escaped `gc`) — ever holds a
#   copy of it. The consequence for operators is worth stating plainly: a
#   slot that is still locked when its gate process is gone is NOT a stale
#   file the kernel forgot to clean up. It means some descendant inherited
#   the descriptor anyway and outlived the gate. push_gate_describe_slots
#   flags exactly that case; the fix is to find and kill the leaked
#   descendant (`lsof <slot-file>`), never to delete the slot file.
#
#   Slots bound HOW MANY gates run at once; they say nothing about WHO goes
#   next. Until gascity-3ndv the wait was a bare poll-and-race — every waiter
#   re-attempted a non-blocking flock every POLL_SECONDS — so at release time
#   whichever waiter's poll happened to land first won, independent of how
#   long anyone had waited. That is LIFO-ish under load and unbounded for any
#   individual waiter: measured, a waiter queued with a 2700s budget was
#   passed over for its entire budget by at least four later arrivals, and a
#   refinery waiter queued 9 minutes earlier than another lost both slots that
#   freed within 2 seconds of each other.
#
#   Acquire is therefore ordered by a FIFO ticket queue under
#   <slot_dir>/queue. A waiter publishes a ticket whose name carries a
#   monotonic sequence number and holds an flock on it for as long as it
#   waits, so ticket liveness works exactly like slot liveness — the kernel
#   drops the lock when the waiter dies, and an unlocked ticket is by
#   definition abandoned and reaped on sight. No PID probing, no heartbeat, no
#   cleanup daemon. A waiter may take a slot only when no live ticket sits
#   ahead of its own, and it drops its ticket the moment it holds a slot: the
#   ticket orders the WAIT, and holding it through the run would block every
#   follower for the length of the gate.
#
#   Non-blocking callers (PUSH_GATE_MAX_WAIT_SECONDS=0 — which is what the
#   Makefile gate targets use) take no ticket, because they hold no position.
#   They instead YIELD: a free slot that a waiter is already in line for is
#   not theirs to take, and they report the same EX_TEMPFAIL they already
#   report for a busy lane. This half is load-bearing rather than a nicety —
#   nearly all real gate traffic arrives non-blocking, so without it the
#   queue would order only the waiters and every fresh arrival would keep
#   overtaking them, which is the starvation itself.
#
#   Both directions are bounded on purpose. A ticket records its waiter's own
#   deadline and stops counting once past it, so a waiter that is alive but
#   wedged cannot close the lane indefinitely — otherwise ordering would only
#   trade one unbounded starvation for another.
#
#   One deliberate deviation from mpr: mpr's caller is an automatic
#   cooldown-retry dispatcher, so it fails fast (immediate EX_TEMPFAIL) when
#   all slots are busy. This gate's caller is a synchronous, human/agent-
#   facing command (a push, or a direct `make` invocation), so instead of
#   failing instantly it polls with a bounded wait, printing an immediate
#   diagnostic the moment it starts waiting (FR5) and naming current
#   holders. Only after the wait bound elapses does it report failure — the
#   caller is expected to map that to `exit 75` (EX_TEMPFAIL), never a bare
#   `exit 1`, so this is never confused with a real test failure or with
#   scripts/push-ownership-guard.sh's unrelated exit-1 contract.
#
# CONTRACT
#   - Slot content is diagnostic only: "<pid> <iso8601-utc> <label>
#     <hostname>" (mpr's own global slot only stamps pid+timestamp; this
#     adds label+hostname per FR8, since callers here span an entire city
#     of heterogeneous rigs/roles rather than one single-purpose script).
#     Slot files are never sourced or eval'd — display only.
#   - This library is entirely bd/beads/claim-lease-agnostic. It is a
#     generic, reusable mutex-semaphore primitive with no awareness of bead
#     claims, mail, or any other Gas City concept. Do not add any
#     bd-specific behavior here — layer that (if ever needed) in the
#     caller, not this library.
#   - Acquire is FIFO among waiters: a caller that began waiting later can
#     never take a slot ahead of one that began waiting earlier. Ordering is
#     layered OVER the slots, never in place of them — if the queue cannot be
#     set up (an unwritable queue dir, a busy queue FD) the acquire degrades
#     to the old unordered poll with a diagnostic rather than failing, exactly
#     as a missing flock(1) does.
#   - A non-blocking acquire yields to any live queued waiter and returns 1
#     even when a slot is free. Callers already map that 1 to exit 75
#     (EX_TEMPFAIL, "gate busy, nothing ran, INDETERMINATE"), which is equally
#     true of a yield, so the caller-visible contract is unchanged — only who
#     gets the slot is.
#   - GC_PUSH_GATE_NO_CAP=1 disables the cap entirely for one invocation
#     (escape hatch, FR9): acquire always succeeds immediately, nothing to
#     release. A missing flock(1) degrades the same way — a warning plus an
#     uncapped run — rather than blocking the caller for the full wait
#     bound, matching how nice/ionice and the systemd slice are treated as
#     optional in scripts/test-local-parallel.
#   - Malformed tunables fall back to their documented defaults with a
#     diagnostic naming the offending variable; they never reach arithmetic
#     or `sleep` unvalidated.
#   - A timed-out acquire returns 1 (shell-false), and ONLY a timed-out
#     acquire returns 1 — every degrade case (missing flock(1), a slot dir
#     that cannot be created) prints its own diagnostic and returns 0 with
#     an empty fd instead, so callers can trust that a 1 always means a
#     real wait-bound expiry, never an environment defect misreported as
#     fleet contention. This library never calls `exit` itself — mapping a
#     timeout to process exit code 75 is the caller's job
#     (scripts/test-local-parallel), keeping this file a pure, testable
#     function library.
#
# FUNCTIONS
#   push_gate_city_root
#       Print the resolved city root. Tries GC_CITY_PATH, GC_CITY,
#       GC_CITY_ROOT in turn — each is validated (must contain city.toml or
#       a legacy .gc/ runtime root) before being trusted, so a stray or
#       malicious env var can't redirect the lock directory anywhere
#       arbitrary (mirrors cmd/gc/main.go's validateCityPath intent). Falls
#       back to a directory walk-up from $PWD looking for city.toml (or,
#       failing that, the first ancestor with a legacy .gc/ root),
#       ceiling-bounded at $HOME. Best-effort bash port of
#       cmd/gc/city_discovery.go's findCityWithOptions — not a byte-for-
#       byte parity guarantee (this is NFR4's "degrade outside a city"
#       fallback path, not the primary resolution mechanism real `gc`
#       sessions rely on via env vars). Returns 1 if nothing is found.
#   push_gate_slots_dir
#       Print the slot directory to use: <city_root>/.gc/gate-slots when
#       push_gate_city_root resolves, else <git_common_dir>/gate-slots —
#       still <repo_root>/.git/gate-slots in a normal clone, but sibling
#       linked worktrees resolve to the one shared common dir instead of a
#       per-worktree `.git` file that cannot hold a directory
#       (NFR4 fallback, never /tmp — see AGENTS.md Build Cache Conventions).
#       Does not create the directory (push_gate_acquire_slot does).
#   push_gate_acquire_slot <slot_dir> <fd_out_var> [holder_label]
#       Reads tunables from env: PUSH_GATE_MAX_CONCURRENT (default 2),
#       PUSH_GATE_MAX_WAIT_SECONDS (default 600), PUSH_GATE_POLL_SECONDS
#       (default 15); each is validated and falls back to its default on a
#       malformed value. holder_label defaults to
#       ${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}. If the slot
#       dir cannot be created (e.g. an unwritable parent), degrades the same
#       way as a missing flock(1): diagnostic to stderr, empty fd, return 0
#       — never conflated with a timeout. Otherwise sweeps slots 0..N-1
#       non-blocking; acquires the first free one immediately (fd assigned
#       to the caller's <fd_out_var>, return 0). If all slots are busy:
#       prints an immediate unbuffered diagnostic naming current holders
#       (FR5), then re-sweeps every POLL_SECONDS until a slot frees or
#       MAX_WAIT_SECONDS elapses. Returns 0 (acquired) or 1 (timed out —
#       caller should `exit 75`). PUSH_GATE_MAX_WAIT_SECONDS=0 selects a
#       non-blocking acquire — one sweep, then return 1 without sleeping —
#       which is what scripts/gate-slot-run uses for the Makefile gate
#       targets, where a caller under a harness timeout must learn the lane
#       is occupied rather than spend its whole budget discovering it. That
#       path reports "not waiting", never a timeout, because it never waited.
#   push_gate_describe_slots <slot_dir> <max_concurrent>
#       Print one "slot-<i>: <holder line>" line per currently-occupied
#       slot, for the FR5 wait message and FR8 operator diagnostics. A slot
#       whose recorded PID no longer exists is flagged as a leaked
#       descendant (see MECHANISM), since that is the one case where the
#       holder line alone points at the wrong process.
#   push_gate_queue_dir <slot_dir>
#       Print the FIFO wait-queue directory for a slot directory
#       (<slot_dir>/queue). Does not create it.
#   push_gate_queue_join <queue_dir> <fd_out_var> <ticket_out_var>
#                        <max_wait_secs> [label]
#       Take a queue position: allocate a monotonic ticket number under an
#       flock'd counter, create and lock the ticket file under a dot-name,
#       then rename it into place — atomically, carrying the lock — so a
#       ticket is never visible to a scanner in the unlocked state that would
#       mark it abandoned. The recorded deadline is now + max_wait_secs.
#       Returns 0 with fd and ticket path assigned, or 1 if the queue could
#       not be joined; a 1 is not fatal, the caller acquires unordered.
#   push_gate_queue_leave <fd> [ticket_path]
#       Give up a queue position: unlink the ticket first so no scanner can
#       still count it, then unlock and close. Safe with empty arguments,
#       which is what an unqueued caller passes.
#   push_gate_queue_ahead_count <queue_dir> [my_ticket_path]
#       Print how many live waiters are queued ahead of my_ticket_path, or
#       ahead of everyone when it is omitted (what an unqueued caller asks).
#       Reaps abandoned tickets (unlocked, so their holder is gone) and skips
#       expired ones (past their own recorded deadline).
#   push_gate_release_slot <fd>
#       Explicit release + close. Normally unnecessary (process exit
#       releases the flock) — provided for tests and tight loops, mirroring
#       mpr_release_run_lock.
#
# PORTABILITY
#   This file deliberately stays bash 3.2-compatible (macOS's stock
#   /bin/bash): no `local -n` namerefs (4.3) and no `exec {var}<>` dynamic
#   FD allocation (4.1). Sibling scripts under the same entrypoint hold the
#   same floor on purpose — see scripts/go-test-observable and
#   scripts/test-integration-shard.
#
# Sourced by scripts/test-local-parallel and scripts/gate-slot-run, and
# directly by scripts/test-push-gate-lock.sh.

# Base file-descriptor number for slot <i>; slot <i> always maps to
# PUSH_GATE_FD_BASE + i. Fixed numbers rather than bash 4.1's `exec {var}<>`
# keep the 3.2 floor above.
PUSH_GATE_FD_BASE=200

# Resolve the city root, validating any env var before trusting it so a
# stray GC_CITY_PATH can't redirect the lock directory arbitrarily.
push_gate_city_root() {
    local _pgc_var _pgc_candidate _pgc_abs
    for _pgc_var in GC_CITY_PATH GC_CITY GC_CITY_ROOT; do
        _pgc_candidate="${!_pgc_var:-}"
        [[ -n "$_pgc_candidate" ]] || continue
        _pgc_abs="$(cd "$_pgc_candidate" 2>/dev/null && pwd)" || continue
        if [[ -f "$_pgc_abs/city.toml" || -d "$_pgc_abs/.gc" ]]; then
            printf '%s\n' "$_pgc_abs"
            return 0
        fi
    done

    # Walk-up discovery: bash port of cmd/gc/city_discovery.go's
    # findCityWithOptions. city.toml wins outright; a legacy .gc/-only
    # ancestor is remembered as a fallback but only used if no city.toml is
    # ever found before the ceiling.
    local _pgc_dir="$PWD" _pgc_home="${HOME:-}" _pgc_legacy="" _pgc_parent
    while :; do
        if [[ -f "$_pgc_dir/city.toml" ]]; then
            printf '%s\n' "$_pgc_dir"
            return 0
        fi
        if [[ -z "$_pgc_legacy" && -d "$_pgc_dir/.gc" ]]; then
            _pgc_legacy="$_pgc_dir"
        fi
        if [[ -n "$_pgc_home" && "$_pgc_dir" == "$_pgc_home" ]]; then
            break
        fi
        _pgc_parent="$(dirname "$_pgc_dir")"
        [[ "$_pgc_parent" == "$_pgc_dir" ]] && break
        _pgc_dir="$_pgc_parent"
    done

    if [[ -n "$_pgc_legacy" ]]; then
        printf '%s\n' "$_pgc_legacy"
        return 0
    fi
    return 1
}

# Print the slot directory to use (city-rooted, or common-Git-dir fallback).
# Does not create it.
push_gate_slots_dir() {
    local _pgs_city_root
    if _pgs_city_root="$(push_gate_city_root)"; then
        printf '%s/.gc/gate-slots\n' "$_pgs_city_root"
        return 0
    fi
    # --git-common-dir (Git 2.5+) may print a path relative to $PWD, so
    # absolutize it here rather than with --path-format=absolute (Git 2.31+):
    # git rev-parse echoes an unrecognized option and still exits 0, which
    # would smuggle garbage past this `if` on older git.
    local _pgs_git_common
    if _pgs_git_common="$(git rev-parse --git-common-dir 2>/dev/null)" && [[ -n "$_pgs_git_common" ]]; then
        _pgs_git_common="$(cd "$_pgs_git_common" 2>/dev/null && pwd)" || return 1
        printf '%s/gate-slots\n' "$_pgs_git_common"
        return 0
    fi
    return 1
}

# Print one diagnostic line per currently-occupied slot.
push_gate_describe_slots() {
    local _pgd_dir="$1" _pgd_max="$2"
    local _pgd_i _pgd_slot _pgd_line _pgd_pid
    for (( _pgd_i = 0; _pgd_i < _pgd_max; _pgd_i++ )); do
        _pgd_slot="$_pgd_dir/slot-${_pgd_i}.lock"
        [[ -f "$_pgd_slot" ]] || continue
        # Still held? A successful non-blocking probe means it's free —
        # nothing to report (its file may hold a stale line from the last
        # holder, which would be misleading to print as a current holder).
        if flock -n "$_pgd_slot" -c 'exit 0' 2>/dev/null; then
            continue
        fi
        _pgd_line=""
        IFS= read -r _pgd_line <"$_pgd_slot" 2>/dev/null || true
        # The slot is held but the process that stamped it is gone, so the
        # holder line names the wrong process. The lock is being kept alive
        # by a descendant that inherited the descriptor — the one case
        # `lsof` on the slot file answers and the holder line does not.
        _pgd_pid="${_pgd_line%% *}"
        if [[ "$_pgd_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$_pgd_pid" 2>/dev/null; then
            _pgd_line="$_pgd_line (holder pid dead — likely a leaked descendant)"
        fi
        printf '  slot-%s: %s\n' "$_pgd_i" "$_pgd_line"
    done
}

# True when file descriptor $1 is already open in this shell. Slot FDs are
# fixed numbers, so a second acquire in the same process must not re-open a
# number it already holds: `exec N<>file` on a live N closes the old
# descriptor and silently drops that slot's lock.
_push_gate_fd_in_use() {
    ( true <&"$1" ) 2>/dev/null
}

# Print a validated numeric tunable. $1 = env var name, $2 = documented
# default, $3 = minimum allowed value. A malformed value is reported by name
# and replaced by the default, so it never reaches arithmetic or `sleep`.
_push_gate_tunable() {
    local _pgt_name="$1" _pgt_default="$2" _pgt_min="$3"
    local _pgt_value="${!_pgt_name:-$_pgt_default}"
    if ! [[ "$_pgt_value" =~ ^[0-9]+$ ]] || [[ "$_pgt_value" -lt "$_pgt_min" ]]; then
        echo "push-gate: ignoring malformed ${_pgt_name}='${_pgt_value}' — using default ${_pgt_default}" >&2
        _pgt_value="$_pgt_default"
    fi
    printf '%s\n' "$_pgt_value"
}

# ---------------------------------------------------------------------------
# FIFO wait queue (gascity-3ndv)
#
# The slots below are the mutual exclusion; this queue is only the ORDER in
# which waiters get at them. A waiter publishes a ticket file whose name
# carries a monotonic sequence number and holds an flock on it for as long as
# it is waiting, so liveness works exactly like the slots do: the kernel drops
# the lock when the waiter dies, and an unlocked ticket is by definition
# abandoned. No PID probing, no heartbeat, no cleanup daemon.
#
# Queue FDs sit BELOW PUSH_GATE_FD_BASE so they cannot collide with slot
# <i> -> BASE+i for any PUSH_GATE_MAX_CONCURRENT the caller sets.
PUSH_GATE_QUEUE_FD=199
PUSH_GATE_QUEUE_SEQ_FD=198

# Print the wait-queue directory for a slot directory. Does not create it.
push_gate_queue_dir() {
    printf '%s/queue\n' "$1"
}

# Print the next ticket number, allocated under an flock on the counter file so
# two joiners can never mint the same one. The lock is held for two reads and a
# write, so contention is resolved by a bounded spin rather than a sleep loop;
# a joiner that cannot get it degrades to an unordered acquire rather than
# blocking. Returns 1 if no number could be allocated.
_push_gate_queue_next_seq() {
    local _pgq_dir="$1"
    local _pgq_seq_file="$_pgq_dir/.seq"
    local _pgq_n=0 _pgq_try=0 _pgq_locked=0

    _push_gate_fd_in_use "$PUSH_GATE_QUEUE_SEQ_FD" && return 1
    eval "exec ${PUSH_GATE_QUEUE_SEQ_FD}<>\"\$_pgq_seq_file\"" 2>/dev/null || return 1
    while [[ "$_pgq_try" -lt 200 ]]; do
        if flock -n "$PUSH_GATE_QUEUE_SEQ_FD" 2>/dev/null; then
            _pgq_locked=1
            break
        fi
        _pgq_try=$(( _pgq_try + 1 ))
    done
    if [[ "$_pgq_locked" -ne 1 ]]; then
        eval "exec ${PUSH_GATE_QUEUE_SEQ_FD}>&-" 2>/dev/null || true
        return 1
    fi

    IFS= read -r _pgq_n <"$_pgq_seq_file" 2>/dev/null || _pgq_n=0
    # A truncated or hand-edited counter restarts the numbering rather than
    # reaching arithmetic. Ordering survives it: ties break on the ticket
    # name's pid suffix, which every scanner sorts identically.
    [[ "$_pgq_n" =~ ^[0-9]+$ ]] || _pgq_n=0
    _pgq_n=$(( _pgq_n + 1 ))
    printf '%s\n' "$_pgq_n" >"$_pgq_seq_file" 2>/dev/null || true
    # Closing the descriptor releases the flock.
    eval "exec ${PUSH_GATE_QUEUE_SEQ_FD}>&-" 2>/dev/null || true
    printf '%s\n' "$_pgq_n"
}

# push_gate_queue_join <queue_dir> <fd_out_var> <ticket_out_var> <max_wait_secs> [label]
#
# Take a queue position. The ticket is created under a dot-name (excluded from
# the ticket glob), locked, and only then renamed into place — the rename is
# atomic and carries the lock with it, so a ticket is never visible to another
# scanner in the unlocked state that would mark it abandoned.
#
# Returns 0 with the fd and ticket path assigned, or 1 if the queue could not
# be joined. A 1 is not fatal: the caller degrades to an unordered acquire.
push_gate_queue_join() {
    local _pgq_dir="$1" _pgq_fd_var="$2" _pgq_ticket_var="$3"
    local _pgq_max_wait="$4" _pgq_label="${5:-}"
    eval "$_pgq_fd_var="
    eval "$_pgq_ticket_var="

    [[ "$_pgq_max_wait" =~ ^[0-9]+$ ]] || _pgq_max_wait=0
    mkdir -p "$_pgq_dir" 2>/dev/null || return 1
    _push_gate_fd_in_use "$PUSH_GATE_QUEUE_FD" && return 1

    local _pgq_seq
    _pgq_seq="$(_push_gate_queue_next_seq "$_pgq_dir")" || return 1

    local _pgq_pending="$_pgq_dir/.pending-$$"
    eval "exec ${PUSH_GATE_QUEUE_FD}<>\"\$_pgq_pending\"" 2>/dev/null || return 1
    if ! flock -n "$PUSH_GATE_QUEUE_FD" 2>/dev/null; then
        eval "exec ${PUSH_GATE_QUEUE_FD}>&-" 2>/dev/null || true
        return 1
    fi

    # "<pid> <iso8601-utc> <deadline-epoch> <label>". Diagnostic, except the
    # deadline, which bounds how long this ticket may hold up the queue.
    printf '%s %s %s %s\n' "$$" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        "$(( $(date +%s) + _pgq_max_wait ))" "$_pgq_label" >"$_pgq_pending" 2>/dev/null || true

    local _pgq_ticket
    _pgq_ticket="$(printf '%s/t-%012d-%s.lock' "$_pgq_dir" "$_pgq_seq" "$$")"
    if ! mv -f "$_pgq_pending" "$_pgq_ticket" 2>/dev/null; then
        eval "exec ${PUSH_GATE_QUEUE_FD}>&-" 2>/dev/null || true
        return 1
    fi

    eval "$_pgq_fd_var=\$PUSH_GATE_QUEUE_FD"
    eval "$_pgq_ticket_var=\$_pgq_ticket"
    return 0
}

# push_gate_queue_leave <fd> [ticket_path]
#
# Give up a queue position: unlink the ticket first so no scanner can still
# count it, then release and close. Safe to call with empty arguments, which
# is what an unqueued (non-blocking) caller passes.
push_gate_queue_leave() {
    local _pgq_fd="${1:-}" _pgq_ticket="${2:-}"
    [[ -n "$_pgq_ticket" ]] && rm -f "$_pgq_ticket" 2>/dev/null
    if [[ "$_pgq_fd" =~ ^[0-9]+$ ]]; then
        flock -u "$_pgq_fd" 2>/dev/null || true
        eval "exec ${_pgq_fd}>&-" 2>/dev/null || true
    fi
    return 0
}

# push_gate_queue_ahead_count <queue_dir> [my_ticket_path]
#
# Print how many live waiters are queued ahead of <my_ticket_path>, or ahead of
# everyone when it is omitted (what an unqueued caller asks). Ticket names are
# zero-padded, so the glob's lexical order is the sequence order.
#
# Two kinds of ticket do not count. An UNLOCKED one is abandoned — its holder
# is gone and the kernel dropped the lock — so it is reaped on sight. An
# EXPIRED one belongs to a waiter that is past its own declared bound and will
# give up on its next poll; skipping it keeps a waiter that is alive but wedged
# from closing the lane indefinitely, which would only trade one unbounded
# starvation for another.
push_gate_queue_ahead_count() {
    local _pgq_dir="$1" _pgq_mine="${2:-}"
    local _pgq_n=0 _pgq_file _pgq_line _pgq_pid _pgq_stamp _pgq_deadline _pgq_now _pgq_rest
    _pgq_now="$(date +%s)"
    for _pgq_file in "$_pgq_dir"/t-*.lock; do
        [[ -e "$_pgq_file" ]] || continue
        if [[ -n "$_pgq_mine" && "$_pgq_file" == "$_pgq_mine" ]]; then
            break
        fi
        if flock -n "$_pgq_file" -c 'exit 0' 2>/dev/null; then
            rm -f "$_pgq_file" 2>/dev/null || true
            continue
        fi
        _pgq_deadline=""
        _pgq_line=""
        IFS= read -r _pgq_line <"$_pgq_file" 2>/dev/null || true
        read -r _pgq_pid _pgq_stamp _pgq_deadline _pgq_rest <<<"$_pgq_line"
        if [[ "$_pgq_deadline" =~ ^[0-9]+$ ]] && [[ "$_pgq_now" -gt "$_pgq_deadline" ]]; then
            continue
        fi
        _pgq_n=$(( _pgq_n + 1 ))
    done
    printf '%s\n' "$_pgq_n"
}

# Acquire one of PUSH_GATE_MAX_CONCURRENT slots, in FIFO order among waiters,
# polling with a bounded wait on contention. See header for the full contract.
push_gate_acquire_slot() {
    local _pgl_slot_dir="$1"
    local _pgl_fd_var="$2"
    local _pgl_label="${3:-}"

    if [[ -z "$_pgl_label" ]]; then
        _pgl_label="${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}"
    fi

    if [[ "${GC_PUSH_GATE_NO_CAP:-}" == "1" ]]; then
        eval "$_pgl_fd_var="
        return 0
    fi

    # flock(1) is the entire mechanism. Without it every slot probe fails and
    # the caller would burn the whole wait bound before reporting a confusing
    # timeout, so degrade best-effort with a diagnostic instead.
    if ! command -v flock >/dev/null 2>&1; then
        echo "push-gate: flock(1) not found — running without a cross-invocation cap (brew install flock)" >&2
        eval "$_pgl_fd_var="
        return 0
    fi

    local _pgl_max _pgl_max_wait _pgl_poll
    _pgl_max="$(_push_gate_tunable PUSH_GATE_MAX_CONCURRENT 2 1)"
    _pgl_max_wait="$(_push_gate_tunable PUSH_GATE_MAX_WAIT_SECONDS 600 0)"
    _pgl_poll="$(_push_gate_tunable PUSH_GATE_POLL_SECONDS 15 1)"
    local _pgl_host
    _pgl_host="$(hostname 2>/dev/null || echo unknown)"

    # An unwritable slot dir (e.g. a parent path component that is a file,
    # as .git is in a linked worktree prior to push_gate_slots_dir's
    # common-dir fix) is a degrade case, not a wait-bound timeout — same
    # `return 1` used to mean both, which sent operators chasing fleet
    # contention that did not exist. Degrade best-effort instead.
    if ! mkdir -p "$_pgl_slot_dir" 2>/dev/null; then
        echo "push-gate: cannot create slot dir $_pgl_slot_dir — running without a cross-invocation cap" >&2
        eval "$_pgl_fd_var="
        return 0
    fi

    # Take a queue position BEFORE the first sweep, so this caller's place in
    # line is fixed by when it arrived rather than by whose poll happens to
    # land first when a slot frees. Only a caller willing to wait takes a
    # ticket; a non-blocking caller has no position to hold and instead yields
    # below to whoever already has one.
    local _pgl_queue_dir _pgl_ticket_fd="" _pgl_ticket="" _pgl_queued=0
    _pgl_queue_dir="$(push_gate_queue_dir "$_pgl_slot_dir")"
    if [[ "$_pgl_max_wait" -gt 0 ]]; then
        if push_gate_queue_join "$_pgl_queue_dir" _pgl_ticket_fd _pgl_ticket \
                "$_pgl_max_wait" "$_pgl_label"; then
            _pgl_queued=1
        else
            # Ordering is layered over the slots, not the mutual exclusion
            # itself: if the queue cannot be set up the gate still has to
            # work. Degrade to the old unordered poll, the same way a missing
            # flock(1) degrades, rather than failing the caller.
            echo "push-gate: could not join the wait queue under $_pgl_queue_dir — acquiring unordered" >&2
        fi
    fi

    local _pgl_i _pgl_slot _pgl_fd _pgl_announced=0 _pgl_start=0
    local _pgl_ahead=0 _pgl_last_ahead=-1

    while :; do
        # Head-of-line check. A queued waiter may take a slot only once every
        # ticket ahead of it is gone, and an unqueued non-blocking caller
        # defers to any live ticket at all. Either way a later arrival cannot
        # overtake an earlier waiter — the property the bare poll-and-race
        # loop lacked, which let a 2700s waiter be passed over for its whole
        # budget by four later arrivals (gascity-3ndv).
        _pgl_ahead=0
        if [[ "$_pgl_queued" -eq 1 ]]; then
            _pgl_ahead="$(push_gate_queue_ahead_count "$_pgl_queue_dir" "$_pgl_ticket")"
        elif [[ "$_pgl_max_wait" -eq 0 ]]; then
            _pgl_ahead="$(push_gate_queue_ahead_count "$_pgl_queue_dir")"
        fi

        if [[ "$_pgl_ahead" -eq 0 ]]; then
            for (( _pgl_i = 0; _pgl_i < _pgl_max; _pgl_i++ )); do
                _pgl_slot="$_pgl_slot_dir/slot-${_pgl_i}.lock"
                _pgl_fd=$(( PUSH_GATE_FD_BASE + _pgl_i ))
                if _push_gate_fd_in_use "$_pgl_fd"; then
                    continue
                fi
                eval "exec ${_pgl_fd}<>\"\$_pgl_slot\"" || continue
                if flock -n "$_pgl_fd"; then
                    printf '%s %s %s %s\n' "$$" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$_pgl_label" "$_pgl_host" >"$_pgl_slot" 2>/dev/null || true
                    eval "$_pgl_fd_var=\$_pgl_fd"
                    if [[ "$_pgl_announced" -eq 1 ]]; then
                        echo "push-gate: slot-${_pgl_i} acquired after wait" >&2
                    fi
                    # The ticket orders the WAIT only. Holding it through the
                    # gate run would block every follower for the length of
                    # that run, collapsing the lane to one slot.
                    push_gate_queue_leave "$_pgl_ticket_fd" "$_pgl_ticket"
                    return 0
                fi
                eval "exec ${_pgl_fd}>&-" || true
            done
        fi

        if [[ "$_pgl_announced" -eq 0 ]]; then
            _pgl_announced=1
            _pgl_start="$(date +%s)"
            if [[ "$_pgl_max_wait" -eq 0 ]]; then
                if [[ "$_pgl_ahead" -gt 0 ]]; then
                    echo "push-gate: ${_pgl_ahead} waiter(s) already queued — yielding rather than overtaking them (PUSH_GATE_MAX_WAIT_SECONDS=0):" >&2
                else
                    echo "push-gate: all $_pgl_max slot(s) busy — not waiting (PUSH_GATE_MAX_WAIT_SECONDS=0):" >&2
                fi
            elif [[ "$_pgl_ahead" -gt 0 ]]; then
                echo "push-gate: queued behind ${_pgl_ahead} earlier waiter(s), waiting up to ${_pgl_max_wait}s (checking every ${_pgl_poll}s):" >&2
            else
                echo "push-gate: all $_pgl_max slot(s) busy, waiting up to ${_pgl_max_wait}s (checking every ${_pgl_poll}s):" >&2
            fi
            push_gate_describe_slots "$_pgl_slot_dir" "$_pgl_max" >&2
            _pgl_last_ahead="$_pgl_ahead"
        elif [[ "$_pgl_queued" -eq 1 && "$_pgl_ahead" -ne "$_pgl_last_ahead" ]]; then
            # The old loop reprinted one unchanging "all slots busy" line every
            # cycle, so being passed over looked exactly like making progress
            # and a starving waiter had no way to tell the difference. Position
            # only ever moves toward the front now, so report it when it moves.
            echo "push-gate: queue position: ${_pgl_ahead} waiter(s) ahead" >&2
            _pgl_last_ahead="$_pgl_ahead"
        fi

        if (( $(date +%s) - _pgl_start >= _pgl_max_wait )); then
            # A zero wait bound never waited, so reporting an expired wait
            # sends operators chasing a saturated host when one slot was busy
            # for one instant. Holders were already named in the announce
            # above; do not repeat them.
            if [[ "$_pgl_max_wait" -eq 0 ]]; then
                if [[ "$_pgl_ahead" -gt 0 ]]; then
                    echo "push-gate: no slot taken — ${_pgl_ahead} waiter(s) queued ahead (non-blocking acquire)" >&2
                else
                    echo "push-gate: no free slot (non-blocking acquire)" >&2
                fi
            elif [[ "$_pgl_ahead" -gt 0 ]]; then
                # Naming the position distinguishes the two ways a wait ends:
                # the lane was saturated for the whole bound, or this waiter
                # simply never reached the front. They call for different
                # responses and used to be indistinguishable.
                echo "push-gate: timed out after ${_pgl_max_wait}s — still behind ${_pgl_ahead} earlier waiter(s)" >&2
                push_gate_describe_slots "$_pgl_slot_dir" "$_pgl_max" >&2
            else
                echo "push-gate: timed out after ${_pgl_max_wait}s waiting for a free slot" >&2
                push_gate_describe_slots "$_pgl_slot_dir" "$_pgl_max" >&2
            fi
            push_gate_queue_leave "$_pgl_ticket_fd" "$_pgl_ticket"
            return 1
        fi

        sleep "$_pgl_poll"
    done
}

# Explicitly release + close the slot FD. Normally unnecessary.
push_gate_release_slot() {
    local _pgl_fd="$1"
    [[ "$_pgl_fd" =~ ^[0-9]+$ ]] || return 0
    # Unlock before closing: the flock -u releases the open-file-description
    # itself, so any descendant that inherited a copy of this FD stops
    # holding the slot too.
    flock -u "$_pgl_fd" 2>/dev/null || true
    eval "exec ${_pgl_fd}>&-" 2>/dev/null || true
}
