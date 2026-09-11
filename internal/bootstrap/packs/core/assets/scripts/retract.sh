#!/usr/bin/env bash
# retract — generic Core retraction hook, the counterpart to escalate.sh.
#
# escalate.sh mails a condition to the escalation recipient and prints the
# message bead it created ("Sent message <id> to <recipient>"). Nothing ever
# closed that bead: the human-addressed queue has no agent draining it, so an
# advisory outlived the condition that raised it and the backlog only grew
# (gascity-9xr4). This script is the missing half — an emitter that can
# re-observe its own condition hands the bead id back here once the condition
# clears, and the alert's lifetime becomes the condition's lifetime.
#
# It is deliberately NOT an age-based reaper. Age cannot tell a resolved
# advisory from a decision still genuinely waiting on a human; only the emitter
# that re-checks the condition can, which is why retraction is caller-driven.
#
# Packs can override retraction by shipping assets/scripts/retract.sh and
# placing that pack earlier in GC_ESCALATE_SEARCH_PACKS — the same override
# mechanism escalate.sh uses, so a pack that redirects escalation keeps
# retraction pointed at the same place.
set -euo pipefail

MESSAGE_ID=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --message-id)
            [ "$#" -ge 2 ] || { echo "retract: --message-id requires a value" >&2; exit 2; }
            MESSAGE_ID="$2"
            shift 2
            ;;
        --)
            shift
            break
            ;;
        *)
            echo "retract: unknown argument $1" >&2
            exit 2
            ;;
    esac
done

if [ -z "$MESSAGE_ID" ]; then
    echo "retract: --message-id is required" >&2
    exit 2
fi

gc mail archive "$MESSAGE_ID"
