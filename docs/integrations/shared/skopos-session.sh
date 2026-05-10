#!/usr/bin/env bash
# Source this file to get the skopos_session_id function.
# Usage: SESSION_ID=$(skopos_session_id "$PWD")

skopos_session_id() {
  local workspace="${1:-$PWD}"

  if [ -n "${SKOPOS_SESSION_ID:-}" ]; then
    echo "$SKOPOS_SESSION_ID"
    return
  fi

  local session_file="$workspace/.skopos-session"
  if [ -f "$session_file" ]; then
    cat "$session_file"
    return
  fi

  local id
  id=$(printf '%s-%s' "$workspace" "$(date +%Y-%m-%d)" | (shasum -a 256 2>/dev/null || sha256sum) | cut -c1-12)
  echo "$id" > "$session_file" 2>/dev/null || true
  echo "$id"
}
