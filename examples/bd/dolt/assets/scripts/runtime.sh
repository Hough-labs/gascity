#!/bin/sh

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LEGACY_GC_DIR="$GC_CITY_PATH/.gc"

if [ -d "$PACK_STATE_DIR" ] || [ ! -d "$LEGACY_GC_DIR/dolt-data" ]; then
  DOLT_STATE_DIR="$PACK_STATE_DIR"
else
  DOLT_STATE_DIR="$LEGACY_GC_DIR"
fi

# Data lives under .beads/dolt (gc-beads-bd canonical path). Honor
# GC_DOLT_DATA_DIR first so shell pack commands target the same managed data
# directory as the Go lifecycle and doctor code.
DOLT_BEADS_DATA_DIR="${GC_DOLT_DATA_DIR:-$GC_CITY_PATH/.beads/dolt}"
if [ -n "${GC_DOLT_DATA_DIR:-}" ]; then
  DOLT_DATA_DIR="$GC_DOLT_DATA_DIR"
elif [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="${GC_DOLT_LOG_FILE:-$DOLT_STATE_DIR/dolt.log}"
DOLT_PID_FILE="${GC_DOLT_PID_FILE:-$DOLT_STATE_DIR/dolt.pid}"
if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
  DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi
DOLT_PROVIDER_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh"

# is_local_dolt_host returns 0 (true) when the argument names the local managed
# Dolt server — loopback, the unspecified address, or an unset/empty host — and
# 1 (false) for a configured external endpoint. The health, status, and logs
# commands share it so they agree on whether GC owns a local managed process or
# is merely pointed at a remote server it cannot inspect on-disk. Mirrors the
# gc-beads-bd `is_remote` classification (gastownhall/gascity su-deol8).
is_local_dolt_host() {
  case "$1" in
    ""|127.0.0.1|0.0.0.0|localhost|::1|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
  case "$value" in
    true|false)
      printf '%s\n' "$value"
      ;;
  esac
)

read_runtime_state_number() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

canonical_path() (
  path="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

print(os.path.realpath(sys.argv[1]))
PY
    return $?
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$path" 2>/dev/null && return 0
  fi
  printf '%s\n' "$path"
)

same_path() (
  left="$1"
  right="$2"
  [ "$left" = "$right" ] && return 0
  [ "$(canonical_path "$left")" = "$(canonical_path "$right")" ]
)

pid_is_running() (
  pid="$1"

  case "$pid" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi

  if command -v ps >/dev/null 2>&1; then
    ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
    [ "$ps_pid" = "$pid" ] && return 0
  fi

  return 1
)

managed_runtime_listener_pid() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 0
      ;;
  esac

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
    | while IFS= read -r holder_pid; do
        case "$holder_pid" in
          ''|*[!0-9]*)
            continue
            ;;
        esac
        if pid_is_running "$holder_pid"; then
          printf '%s\n' "$holder_pid"
          break
        fi
      done
)

managed_runtime_tcp_reachable() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
    return $?
  fi

  return 1
)

managed_runtime_port() (
  state_file="$1"
  expected_data_dir="$2"

  [ -f "$state_file" ] || return 0

  running=$(read_runtime_state_flag "$state_file" running)
  pid=$(read_runtime_state_number "$state_file" pid)
  port=$(read_runtime_state_number "$state_file" port)
  data_dir=$(read_runtime_state_string "$state_file" data_dir)

  [ "$running" = "true" ] || return 0
  [ -n "$pid" ] || return 0
  [ -n "$port" ] || return 0
  if ! same_path "$data_dir" "$expected_data_dir"; then
    printf 'dolt runtime: managed state data_dir=%s does not match expected data_dir=%s\n' \
      "$data_dir" "$expected_data_dir" >&2
    return 0
  fi
  pid_is_running "$pid" || return 0

  holder_pid=$(managed_runtime_listener_pid "$port" || true)
  if [ -n "$holder_pid" ]; then
    [ "$holder_pid" = "$pid" ] || return 0
    printf '%s\n' "$port"
    return 0
  fi

  if ! managed_runtime_tcp_reachable "$port"; then
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
# runtime state over stale inherited env, then falls back to GC_DOLT_PORT as an
# operator seed, and exits 78 if neither yields a port.
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?

# Resolve a bounded-execution helper. Prefer gtimeout (coreutils on
# macOS), fall back to timeout (coreutils on Linux), then to running
# the command directly if neither is installed. Running unbounded is
# still better than letting a wedged dolt client hang the caller, but
# patrol callers need a hard upper bound wherever possible.
if command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
else
  TIMEOUT_BIN=""
fi

_run_bounded_warned_no_timeout=""

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. If no bounded execution mechanism is available, fail closed rather
# than running a potentially wedged Dolt client unbounded.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$_t" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}

# ---------------------------------------------------------------------------
# Endpoint provenance (gascity-0zw)
#
# Every command in this pack resolves ONE endpoint — the managed city server,
# or the external endpoint GC_DOLT_HOST pins — and can observe nothing else. A
# rig whose own .beads/config.yaml sets `gc.endpoint_origin: explicit` lives on
# a different server entirely, and the managed server can hold a same-named but
# EMPTY database for it. Output that does not name its endpoint therefore does
# not merely omit that rig: it returns a confident wrong answer (0 beads) that
# reads as data loss. The helpers below let each command carry its own
# provenance and make the scopes it did not check visible rather than silent.
# ---------------------------------------------------------------------------

# beads_config_value FILE KEY — print the scalar value of a `KEY: value` line
# from a beads config.yaml, with trailing comments, quotes and surrounding
# whitespace stripped. Prints nothing when the file or key is absent. Keys are
# dotted (dolt.port, gc.endpoint_origin), so the dot is escaped before use as a
# grep pattern.
beads_config_value() (
  _bcv_file="$1"
  _bcv_key="$2"
  [ -f "$_bcv_file" ] || return 0
  _bcv_pattern=$(printf '%s' "$_bcv_key" | sed 's/[.[\*^$]/\\&/g')
  grep "^${_bcv_pattern}:" "$_bcv_file" 2>/dev/null \
    | head -1 \
    | sed "s/^[^:]*:[[:space:]]*//; s/[[:space:]]*#.*$//; s/[\"']//g; s/[[:space:]]*\$//"
)

# dolt_endpoint_description — one-line description of the endpoint the calling
# command reports on. The managed form names the data directory too, because
# "which store does this describe" is the question an escalation needs answered.
dolt_endpoint_description() {
  _ded_host="${GC_DOLT_HOST:-127.0.0.1}"
  if is_local_dolt_host "$_ded_host"; then
    printf 'managed city server 127.0.0.1:%s (data_dir %s)\n' "$GC_DOLT_PORT" "$DOLT_DATA_DIR"
  else
    printf 'configured external endpoint %s:%s\n' "$_ded_host" "$GC_DOLT_PORT"
  fi
}

# dolt_endpoint_host — the host the calling command targets, with the managed
# default applied. Kept beside dolt_endpoint_description so JSON emitters and
# human emitters agree on the value they name.
dolt_endpoint_host() {
  printf '%s\n' "${GC_DOLT_HOST:-127.0.0.1}"
}

# dolt_write_rig_paths OUTFILE [TIMEOUT_SECS] — write one `NAME<TAB>PATH` row
# per rig registered with the city into OUTFILE. Returns non-zero (leaving
# OUTFILE empty) when the enumeration could not be performed at all — no gc on
# PATH, gc wedged past the bound, or empty/unparseable output. gc being broken
# is one of the conditions these commands exist to diagnose, so "no rigs" and
# "we could not look" must never render the same, and only the caller can
# decide how to say so.
#
# The result lands in a FILE rather than on stdout so a command needing both rig
# metadata and rig endpoints pays for one bounded `gc rig list` call and reads
# it twice. An in-memory memo cannot do that job: every `$(...)` capture runs in
# a subshell, so the cached value would die with it and the call would silently
# be paid for again on every patrol tick.
#
# Without jq the name column is empty — pairing JSON object fields is not
# reliably recoverable with sed — so callers that need a name fall back to the
# path's basename. Consumers must split each row on the FIRST tab with
# parameter expansion, never with `IFS=<tab> read -r name path`: TAB is IFS
# whitespace, so `read` silently swallows a leading one and an empty-name row
# lands the path in the name variable. jq's @tsv escapes any literal tab in a
# value, so the first tab is always the field separator.
dolt_write_rig_paths() {
  _dwr_out="$1"
  _dwr_bound="${2:-$GC_DOLT_RIG_LIST_TIMEOUT_SECS}"
  : > "$_dwr_out" || return 1
  command -v gc >/dev/null 2>&1 || return 1
  _dwr_json=$(run_bounded "$_dwr_bound" gc rig list --json 2>/dev/null) || return 1
  [ -n "$_dwr_json" ] || return 1
  _dwr_tab=$(printf '\t')
  if command -v jq >/dev/null 2>&1; then
    _dwr_rows=$(printf '%s' "$_dwr_json" | jq -r '.rigs[]? | [.name, .path] | @tsv' 2>/dev/null) || return 1
  else
    _dwr_rows=$(printf '%s' "$_dwr_json" \
      | grep '"path"' \
      | sed "s/.*\"path\"[[:space:]]*:[[:space:]]*\"//; s/\".*//; s/^/${_dwr_tab}/")
  fi
  [ -n "$_dwr_rows" ] || return 1
  printf '%s\n' "$_dwr_rows" > "$_dwr_out"
}

# dolt_pinned_rig_endpoints_from ROWSFILE — print `NAME|HOST:PORT` for every rig
# in ROWSFILE (as written by dolt_write_rig_paths) that pins its own Dolt
# endpoint (gc.endpoint_origin: explicit) and is therefore NOT observable from
# the endpoint the calling command targets. A pinned rig that happens to name
# the very endpoint we are reporting on IS covered, so it is filtered out.
dolt_pinned_rig_endpoints_from() {
  _dpr_rows_file="$1"
  _dpr_self_host=$(dolt_endpoint_host)
  _dpr_tab=$(printf '\t')
  while IFS= read -r _dpr_row; do
    _dpr_name="${_dpr_row%%"$_dpr_tab"*}"
    _dpr_path="${_dpr_row#*"$_dpr_tab"}"
    [ -n "$_dpr_path" ] || continue
    _dpr_cfg="$_dpr_path/.beads/config.yaml"
    [ -f "$_dpr_cfg" ] || continue
    if [ "$(beads_config_value "$_dpr_cfg" 'gc.endpoint_origin')" != "explicit" ]; then
      continue
    fi
    _dpr_host=$(beads_config_value "$_dpr_cfg" 'dolt.host')
    _dpr_port=$(beads_config_value "$_dpr_cfg" 'dolt.port')
    [ -n "$_dpr_host" ] || _dpr_host="127.0.0.1"
    [ -n "$_dpr_port" ] || _dpr_port="unknown"
    if [ "$_dpr_port" = "$GC_DOLT_PORT" ]; then
      if is_local_dolt_host "$_dpr_host" && is_local_dolt_host "$_dpr_self_host"; then
        continue
      fi
      if [ "$_dpr_host" = "$_dpr_self_host" ]; then
        continue
      fi
    fi
    if [ -z "$_dpr_name" ]; then
      _dpr_name=$(basename "$_dpr_path")
    fi
    printf '%s|%s:%s\n' "$_dpr_name" "$_dpr_host" "$_dpr_port"
  done < "$_dpr_rows_file"
}

# dolt_pinned_rig_endpoints [TIMEOUT_SECS] — one-shot form for commands that
# need only the pinned-endpoint list: enumerate the rigs and filter in a single
# bounded `gc rig list` call. Returns non-zero when the rigs could not be
# enumerated.
dolt_pinned_rig_endpoints() {
  _dpe_rows_file=$(mktemp) || return 1
  if dolt_write_rig_paths "$_dpe_rows_file" "$@"; then
    dolt_pinned_rig_endpoints_from "$_dpe_rows_file"
    rm -f "$_dpe_rows_file"
    return 0
  fi
  rm -f "$_dpe_rows_file"
  return 1
}

# dolt_print_endpoint_scope [TIMEOUT_SECS] — emit the human-readable provenance
# block: which endpoint this output describes, that it describes ONLY that
# endpoint, and which rigs were consequently not checked. The unavailable case
# prints UNKNOWN rather than nothing, because an omission reads as "no pinned
# rigs" — the same wrong answer in a quieter form.
#
# The default bound is the shared GC_DOLT_RIG_LIST_TIMEOUT_SECS. Do not tighten
# it per-command hoping to keep a command "fast": `gc rig list --json` costs
# ~8s on a healthy busy host, so a short bound makes UNKNOWN the routine answer
# and retires the signal.
dolt_print_endpoint_scope() {
  printf 'Endpoint: %s\n' "$(dolt_endpoint_description)"
  printf 'Scope: this report covers ONLY that endpoint.\n'
  if _dpe_pinned=$(dolt_pinned_rig_endpoints "$@"); then
    if [ -n "$_dpe_pinned" ]; then
      printf 'Not checked (rigs pinned to their own endpoint — run gc doctor to probe them):\n'
      printf '%s\n' "$_dpe_pinned" | while IFS='|' read -r _dpe_name _dpe_addr; do
        [ -n "$_dpe_name" ] || continue
        printf '  %s -> %s\n' "$_dpe_name" "$_dpe_addr"
      done
    else
      printf 'Not checked: none (no rig pins its own Dolt endpoint)\n'
    fi
  else
    printf 'Not checked: UNKNOWN — rig enumeration unavailable; run gc doctor to probe every rig endpoint\n'
  fi
}
