#!/usr/bin/env bash
# skopos UserPromptSubmit hook — contextual code pre-fetch + memory checkpoint.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=skopos-common.sh
source "$SCRIPT_DIR/skopos-common.sh"
skopos_hook_ready || exit 0

INPUT=$(cat)
PROMPT=$(echo "$INPUT" | jq -r '.user_prompt // .prompt // empty')
[ ${#PROMPT} -lt 10 ] && exit 0

MSG=""

# Memory checkpoint every 10 turns: long sessions lose unrecorded knowledge.
TURN_COUNT=$(echo "$INPUT" | jq -r '.num_turns // 0')
if [ "$TURN_COUNT" -gt 0 ] && [ $((TURN_COUNT % 10)) -eq 0 ]; then
  MSG="[skopos checkpoint — turn $TURN_COUNT] Review the last 10 turns. Record durable facts now via blackboard_write: decisions (why), bugs, debt, conventions. Then report_status with your current state."
fi

# Keyword search: code pre-fetch only for exploration-shaped prompts,
# deduplicated on a 3-minute cache so repeated prompts stay cheap.
if echo "$PROMPT" | grep -qiE 'how does|where is|who calls|what does|show me|explain|architecture|structure|flow|trace|find|refactor|impact|deps|dependenc|call(er|s)?|uses'; then
  TERMS=$(echo "$PROMPT" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9_]+/ /g' \
    | tr ' ' '\n' \
    | grep -vE '^(how|does|where|is|the|a|an|of|to|in|for|with|and|or|what|who|calls|show|me|can|you|please|it|this|that|from|on|at|by|my|our|i|we|do|find|explain|work|works|about|there|any|which|when|why|not|no|yes|be|are|was|were|use|used|using|get|got|has|have|had|will|would|should|could)$' \
    | head -3 \
    | tr '\n' ' ' \
    | sed 's/ *$//')

  if [ -n "$TERMS" ] && ! skopos_hook_throttled "prompt-$(echo "$TERMS" | tr ' ' '_')" 180; then
    # Multi-term FTS is an implicit AND; when the phrase misses, retry with
    # the single strongest term so one noisy word doesn't blank the pre-fetch.
    fmt_hits() { jq -r '.hits[0:5][] | "\(.qualified // .name)  \(.kind)  \(.path):\(.line)"' 2>/dev/null; }
    HITS=$(skopos search "$TERMS" --json 2>/dev/null | fmt_hits)
    if [ -z "$HITS" ]; then
      FIRST=$(echo "$TERMS" | awk '{print $1}')
      if [ -n "$FIRST" ] && [ "$FIRST" != "$TERMS" ]; then
        HITS=$(skopos search "$FIRST" --json 2>/dev/null | fmt_hits)
      fi
    fi
    if [ -n "$HITS" ]; then
      MSG="${MSG:+$MSG

}[skopos code] Relevant symbols for '${TERMS}':
$HITS
Use code_symbol/code_callers/code_impact for the full picture instead of grep."
    fi
  fi
fi

skopos_hook_emit UserPromptSubmit "$MSG"
