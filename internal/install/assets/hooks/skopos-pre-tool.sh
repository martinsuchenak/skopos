#!/usr/bin/env bash
# skopos PreToolUse hook — nudge broad code searches toward the indexed tools.
# Never rewrites commands; only adds context.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=skopos-common.sh
source "$SCRIPT_DIR/skopos-common.sh"
skopos_hook_ready || exit 0

INPUT=$(cat)
TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // .toolName // empty')

NUDGE=""
case "$TOOL_NAME" in
  Grep)
    PATTERN=$(echo "$INPUT" | jq -r '.tool_input.pattern // empty')
    # Symbol-shaped patterns benefit from the graph (callers/impact); literal
    # strings legitimately belong to grep.
    if echo "$PATTERN" | grep -qE '^[A-Za-z_][A-Za-z0-9_:.-]*$'; then
      if skopos_hook_is_remote; then
        NUDGE="[skopos] Symbol-shaped grep. code_callers/code_impact (MCP) answer caller and blast-radius questions the search can't; try them first, fall back to grep for exhaustive listings."
      else
        NUDGE="[skopos] Symbol-shaped grep. 'skopos who-calls' / 'skopos impact' (Bash) answer caller and blast-radius questions the search can't; try them first, fall back to grep for exhaustive listings."
      fi
    fi
    ;;
  Agent)
    SUBAGENT=$(echo "$INPUT" | jq -r '.tool_input.subagent_type // empty')
    PROMPT=$(echo "$INPUT" | jq -r '.tool_input.prompt // empty')
    if [ "$SUBAGENT" = "Explore" ] || echo "$PROMPT" | grep -qiE 'search|find|explore|locate|where is|which file|how does'; then
      NUDGE="[skopos] Before dispatching: skopos search/code_symbol may answer this from the index in one call."
    fi
    ;;
  Bash)
    CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
    if echo "$CMD" | grep -q "skopos"; then exit 0; fi
    if echo "$CMD" | grep -qE '^\s*(rg|grep|ag|find|fd)\b'; then
      # Exhaustive listings are legitimately grep's job — only nudge lookups.
      if ! echo "$CMD" | grep -qiE 'all (usages|occurrences|references|instances)|list all|-r'; then
        if skopos_hook_is_remote; then
          NUDGE="[skopos] For symbol/code-structure lookups, code_search/code_symbol (MCP) use the shared index — faster and it understands callers."
        else
          NUDGE="[skopos] For symbol/code-structure lookups, 'skopos search/symbol' (Bash) use the shared index — faster and it understands callers."
        fi
      fi
    fi
    ;;
esac

skopos_hook_emit PreToolUse "$NUDGE"
