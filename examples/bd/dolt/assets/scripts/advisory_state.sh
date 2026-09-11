#!/bin/sh
# advisory_state.sh — collapse a persistent dolt-health advisory into a single
# rolling notification instead of one mayor bead per doctor tick (#3409).
#
# mol-dog-doctor runs as a cooldown order every 5 minutes. Any sustained warning
# (latency at threshold, connections high, orphan DBs, stale backups) would
# otherwise mail a fresh "Dolt health advisory [MEDIUM]" every tick, flooding the
# mayor inbox with identical beads for one root cause. These helpers persist a
# signature of the last-sent advisory and suppress re-sends until that signature
# changes, clearing it when the server is healthy so the next occurrence
# re-alerts.
#
# The caller builds the signature from the *set of active conditions*, not their
# tick-volatile measurements (exact latency ms, connection count, backup age),
# so a steady condition yields exactly one advisory while a changed condition set
# re-alerts. The CRITICAL "server unreachable" escalation is intentionally NOT
# routed through this dedup, so a true outage always alerts.
#
# The state file also carries the id of the message bead the advisory created,
# so the emitter can withdraw that bead once the condition clears (gascity-9xr4:
# human-addressed advisories had a 0% auto-close rate across the whole history
# of the queue and were only ever swept by hand). Format is one value per line —
# line 1 the signature, line 2 the bead id — which keeps a state file written
# before the bead id existed readable: line 2 is simply absent and retraction
# becomes a no-op.
#
# Sourced by mol-dog-doctor.sh; unit-tested by test/dolt/advisory_dedup_test.sh.

# advisory_changed SIGNATURE STATE_FILE — exit 0 when SIGNATURE differs from the
# signature recorded in STATE_FILE (or none is recorded yet); exit 1 when they
# are identical. Read-only: never writes. With no STATE_FILE it fails open
# (treats the advisory as changed) so a misconfiguration degrades to the
# pre-dedup behavior — a repeated alert — never to silence.
advisory_changed() {
  _adv_sig="${1:-}"
  _adv_file="${2:-}"
  [ -n "$_adv_file" ] || return 0
  [ -f "$_adv_file" ] || return 0
  IFS= read -r _adv_prev < "$_adv_file" 2>/dev/null || _adv_prev=""
  if [ "$_adv_prev" = "$_adv_sig" ]; then
    return 1
  fi
  return 0
}

# advisory_record SIGNATURE STATE_FILE [MESSAGE_ID] — persist SIGNATURE as the
# last-sent advisory, plus the message bead the send created so it can later be
# withdrawn. Call only after a successful send, so a failed escalation does not
# suppress the retry on the next tick. Best-effort: a write failure is ignored
# (fails open — the worst case is a duplicate alert, not a missed one).
advisory_record() {
  _adv_sig="${1:-}"
  _adv_file="${2:-}"
  _adv_id="${3:-}"
  [ -n "$_adv_file" ] || return 0
  _adv_dir=$(dirname "$_adv_file")
  [ -d "$_adv_dir" ] || mkdir -p "$_adv_dir" 2>/dev/null || true
  ( umask 077; printf '%s\n%s\n' "$_adv_sig" "$_adv_id" > "$_adv_file" ) 2>/dev/null || true
}

# advisory_recorded_id STATE_FILE — print the message bead id recorded alongside
# the last-sent signature, or nothing when none was recorded (no state file, or
# a file written before the id was tracked). Read-only: never writes.
advisory_recorded_id() {
  _adv_file="${1:-}"
  [ -n "$_adv_file" ] || return 0
  [ -f "$_adv_file" ] || return 0
  sed -n '2p' "$_adv_file" 2>/dev/null || true
}

# advisory_clear STATE_FILE — forget the last-sent signature (and its message
# bead id) so the next warning re-alerts. Call when the server is healthy AND
# the recorded advisory has been withdrawn: the state file's lifetime is the
# advisory bead's lifetime, so clearing before a successful retraction would
# strand that bead open forever — the exact defect this state exists to close.
# Best-effort.
advisory_clear() {
  [ -n "${1:-}" ] || return 0
  rm -f "$1" 2>/dev/null || true
}
