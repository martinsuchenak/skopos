#!/usr/bin/env bash
# skopos hook shared library — sourced by every hook script.
# Guards: never break the agent; any failure is silent (exit 0 downstream).

# Hooks set SCRIPT_DIR before sourcing; derive it here too so the library
# works standalone (agent-type detection depends on it).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SKOPOS_HOOK_CACHE="${TMPDIR:-/tmp}/skopos-hook-cache"

skopos_hook_ready() {
  command -v jq &>/dev/null || return 1
  command -v skopos &>/dev/null || return 1
  # Remote mode is always ready: the server answers regardless of whether
  # THIS repo is indexed yet. Local mode needs a local index to be useful.
  if skopos_hook_is_remote; then return 0; fi
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

# skopos_hook_mode — "remote <url>" when a server is configured, "local"
# otherwise. MCP tools exist only in remote mode; in local mode the CLI is
# the agent's interface.
skopos_hook_mode() {
  skopos mode 2>/dev/null || echo "local"
}

skopos_hook_is_remote() {
  case "$(skopos_hook_mode)" in remote*) return 0;; *) return 1;; esac
}

# skopos_hook_agent_type — derived from the hook install directory
# (~/.claude/hooks -> claude-code, ~/.zcode/hooks -> zcode, ...).
skopos_hook_agent_type() {
  case "$SCRIPT_DIR" in
    */.claude/hooks*) echo "claude-code";;
    */.codex/hooks*|*/.codex*) echo "codex";;
    */.gemini/hooks*) echo "gemini-cli";;
    */.zcode/hooks*) echo "zcode";;
    */.opencode/hooks*) echo "opencode";;
    *) echo "unknown";;
  esac
}

# skopos_hook_agent_id — stable per machine + agent type.
skopos_hook_agent_id() {
  echo "$(skopos_hook_agent_type)-$(hostname -s)"
}

# skopos_hook_session_file — per-workspace session id store (gitignored).
skopos_hook_session_file() {
  echo ".skopos-session"
}

# skopos_hook_session_id — current session id, or "" when none.
skopos_hook_session_id() {
  cat "$(skopos_hook_session_file)" 2>/dev/null | head -1
}

# skopos_hook_begin_session — mint a fresh session id and report the start.
# Continuity matters: report_status calls that omit session_id each create a
# NEW session, fragmenting the timeline into 1-2 event slivers.
skopos_hook_begin_session() {
  local SID
  SID=$(uuidgen 2>/dev/null | tr "A-Z" "a-z") || SID="sess-$(date +%s)-$$"
  echo "$SID" > "$(skopos_hook_session_file)"
  skopos report     --session-id "$SID"     --agent-id "$(skopos_hook_agent_id)"     --agent-type "$(skopos_hook_agent_type)"     --workspace "$(skopos workspace 2>/dev/null)"     --status running     --message "session started (auto)"     --metadata "{\"source\":\"hook\"}"     >/dev/null 2>&1 &
  echo "$SID"
}

# skopos_hook_heartbeat — throttled progress ping so active agents are never
# marked stuck while working silently between reports.
skopos_hook_heartbeat() {
  local SID; SID="$(skopos_hook_session_id)"
  [ -z "$SID" ] && return 0
  skopos report     --session-id "$SID"     --agent-id "$(skopos_hook_agent_id)"     --agent-type "$(skopos_hook_agent_type)"     --workspace "$(skopos workspace 2>/dev/null)"     --status running     --metadata "{\"source\":\"hook\",\"heartbeat\":true}"     >/dev/null 2>&1 &
}
