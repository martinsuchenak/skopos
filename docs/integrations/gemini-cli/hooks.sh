#!/usr/bin/env bash
# Gemini CLI lifecycle hook — reports agent status to Skopos.
# Usage: hooks.sh <start|stop>
# Wire to session start/stop via a wrapper script (see README).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../shared/skopos-session.sh
source "$SCRIPT_DIR/../shared/skopos-session.sh" 2>/dev/null || true

EVENT="${1:-stop}"
SKOPOS_SERVER_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}"
SKOPOS_API_KEY="${SKOPOS_API_KEY:-}"
AGENT_ID="gemini-$(hostname -s)"
SESSION_ID="$(skopos_session_id "$PWD" 2>/dev/null)" || SESSION_ID="unknown-session"

case "$EVENT" in
  start)
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} \
      --agent-id "$AGENT_ID" \
      --agent-type "gemini" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status running \
      --message "session started" || true
    ;;
  stop)
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} \
      --agent-id "$AGENT_ID" \
      --agent-type "gemini" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status succeeded \
      --message "session complete" || true
    ;;
  *)
    echo "skopos-hook: unknown event '$EVENT'" >&2 || true
    ;;
esac
