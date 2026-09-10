#!/usr/bin/env bash
# skopos SessionStart hook — project briefing: index status, branch knowledge, plans.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=skopos-common.sh
source "$SCRIPT_DIR/skopos-common.sh"
skopos_hook_ready || exit 0

STATUS=$(skopos index status 2>/dev/null | head -5)
BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
BLACKBOARD=""
PLANS=""

if [ -n "$BRANCH" ]; then
  # Branch knowledge: titles only, top entries.
  BLACKBOARD=$(skopos blackboard read --branch "$BRANCH" 2>/dev/null | grep -E '^- \\*\\*' | head -8)
  PLANS=$(skopos plan list --branch "$BRANCH" 2>/dev/null | head -5)
fi

MSG="[skopos] Shared knowledge server is active. Start every task with skopos_context (workspace_id + branch). Prefer code_* MCP tools (code_search, code_symbol, code_callers, code_impact) over grep for code structure."

[ -n "$STATUS" ] && MSG="$MSG

Indexed branches:
$STATUS"

[ -n "$BLACKBOARD" ] && MSG="$MSG

Branch knowledge ($BRANCH):
$BLACKBOARD"

[ -n "$PLANS" ] && MSG="$MSG

Active plans:
$PLANS"

MSG="$MSG

Auto-memory is ON: record decisions, bugs, conventions via blackboard_write (scope=branch by default; bug/debt float across branches). Check in with report_status when you start and whenever state changes."

skopos_hook_emit SessionStart "$MSG"
