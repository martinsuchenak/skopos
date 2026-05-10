#!/usr/bin/env bash
# Claude Code lifecycle hook — reports agent status to Skopos.
# Called by Claude Code with event JSON on stdin.
# Usage: hooks.sh <pre-tool|post-tool|stop>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../shared/skopos-session.sh
source "$SCRIPT_DIR/../shared/skopos-session.sh"

EVENT="${1:-stop}"
SKOPOS_SERVER_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}"
SKOPOS_API_KEY="${SKOPOS_API_KEY:-}"
AGENT_ID="claude-code-$(hostname -s)"
SESSION_ID="$(skopos_session_id "$PWD")"

INPUT=$(cat)

case "$EVENT" in
  pre-tool)
    TOOL_NAME=$(printf '%s' "$INPUT" | jq -r '.tool_name // "unknown"' 2>/dev/null || echo "unknown")
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      --api-key "$SKOPOS_API_KEY" \
      --agent-id "$AGENT_ID" \
      --agent-type "claude-code" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status running \
      --message "using $TOOL_NAME" || true
    ;;
  post-tool)
    TOOL_NAME=$(printf '%s' "$INPUT" | jq -r '.tool_name // "unknown"' 2>/dev/null || echo "unknown")
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      --api-key "$SKOPOS_API_KEY" \
      --agent-id "$AGENT_ID" \
      --agent-type "claude-code" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status running \
      --message "$TOOL_NAME complete" || true
    ;;
  stop)
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      --api-key "$SKOPOS_API_KEY" \
      --agent-id "$AGENT_ID" \
      --agent-type "claude-code" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status succeeded \
      --message "session complete" || true
    ;;
esac
