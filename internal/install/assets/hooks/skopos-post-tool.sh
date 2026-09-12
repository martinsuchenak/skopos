#!/usr/bin/env bash
# skopos PostToolUse hook — after source edits, remind to record the decision.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=skopos-common.sh
source "$SCRIPT_DIR/skopos-common.sh"
skopos_hook_ready || exit 0

INPUT=$(cat)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // .toolName // empty')

case "$TOOL_NAME" in
  Edit|Write|MultiEdit)
    FILE=$(echo "$INPUT" | jq -r '.tool_input.file_path // .tool_input.path // empty')
    if echo "$FILE" | grep -qE '\.(php|go|py|rs|ts|tsx|js|jsx|java|cs|svelte|vue|rb)$'; then
      if skopos_hook_throttled edit-memory 300; then
        exit 0
      fi
      skopos_hook_emit PostToolUse "[skopos] You just modified $FILE. If a decision, convention, or non-obvious constraint drove this change, record it now via blackboard_write (scope=branch) — future agents start with what you learned."
    fi
    ;;
esac
exit 0
