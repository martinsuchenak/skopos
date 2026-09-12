#!/usr/bin/env bash
# skopos Stop hook — end-of-session knowledge extraction reminder.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=skopos-common.sh
source "$SCRIPT_DIR/skopos-common.sh"
skopos_hook_ready || exit 0

INPUT=$(cat)
TURN_COUNT=$(echo "$INPUT" | jq -r '.num_turns // 0')
[ "$TURN_COUNT" -lt 3 ] && exit 0

MSG="[skopos] Session ending ($TURN_COUNT turns). Before finishing, extract durable facts to the blackboard (blackboard_write) — only what a future session needs:

1. ARCHITECTURAL DECISIONS — why something is built the way it is
2. NEGATIVE DECISIONS — what was tried and rejected, and why
3. INTER-MODULE CONTRACTS — implicit interfaces the code doesn't show
4. CONVENTIONS — naming, patterns, file-structure rules
5. BUGS / DEBT — entry_type bug or debt so they float across branches

Then: archive finished plans (plan_archive), and report_status with your final state."

skopos_hook_emit Stop "$MSG"
