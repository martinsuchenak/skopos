#!/usr/bin/env bash
# skopos hook shared library — sourced by every hook script.
# Guards: never break the agent; any failure is silent (exit 0 downstream).

SKOPOS_HOOK_CACHE="${TMPDIR:-/tmp}/skopos-hook-cache"

skopos_hook_ready() {
  command -v jq &>/dev/null || return 1
  command -v skopos &>/dev/null || return 1
  # "In a skopos project": the index answers (local .skopos or configured server).
  skopos index status &>/dev/null || return 1
  return 0
}

# skopos_hook_emit <event> <message> — prints the Claude hook JSON envelope.
skopos_hook_emit() {
  local EVENT="$1" MSG="$2"
  [ -z "$MSG" ] && exit 0
  jq -n --arg msg "$MSG" --arg event "$EVENT" '{
    "hookSpecificOutput": {
      "hookEventName": $event,
      "additionalContext": $msg
    }
  }'
}

# skopos_hook_throttled <key> <seconds> — true when key fired within window.
skopos_hook_throttled() {
  local KEY="$1" WINDOW="$2" NOW STAMP_FILE
  NOW=$(date +%s)
  STAMP_FILE="${SKOPOS_HOOK_CACHE}-${KEY}.stamp"
  [ -f "$STAMP_FILE" ] || { echo "$NOW" > "$STAMP_FILE"; return 1; }
  local LAST; LAST=$(cat "$STAMP_FILE" 2>/dev/null || echo 0)
  if [ $((NOW - LAST)) -lt "$WINDOW" ]; then
    return 0
  fi
  echo "$NOW" > "$STAMP_FILE"
  return 1
}
