#!/usr/bin/env bash
# Interactive setup walkthrough for Skopos agent integrations.
# Run from the skopos repo root: bash docs/integrations/shared/setup.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HOOKS_PATH="$REPO_ROOT/docs/integrations"

echo "=== Skopos Agent Integration Setup ==="
echo ""
echo "TIP: 'skopos install --agent <name|all>' automates the MCP config merge"
echo "     (and steering/skill copies) for each agent below. Use --url/--api-key"
echo "     for a remote skopos, --scope project for the current dir, --dry-run to preview."
echo "     The per-agent steps below are the manual fallback."
echo ""
echo "Skopos repo: $REPO_ROOT"
echo ""

# Verify binary
if ! command -v skopos &>/dev/null; then
  echo "WARNING: skopos not found in PATH."
  echo "Build and install first:"
  echo "  cd $REPO_ROOT && task build-local"
  echo "  sudo ln -sf $REPO_ROOT/bin/skopos /usr/local/bin/skopos"
  echo ""
else
  echo "skopos binary: $(command -v skopos)"
  echo ""
fi

# Verify jq
if ! command -v jq &>/dev/null; then
  echo "WARNING: jq not found. Claude Code hook scripts require jq for JSON parsing."
  echo "Install: brew install jq"
  echo ""
fi

echo "=== Claude Code ==="
echo "1. Merge MCP + hooks config:"
echo "   $HOOKS_PATH/claude-code/settings-snippet.json"
echo "   -> into: ~/.claude/settings.json"
echo "   Replace SKOPOS_HOOKS_PATH with: $HOOKS_PATH/claude-code/hooks.sh"
echo ""
echo "2. Install skill (optional):"
echo "   cp $HOOKS_PATH/claude-code/skopos-skill.md ~/.claude/plugins/skills/"
echo ""
echo "   Full instructions: $HOOKS_PATH/claude-code/README.md"
echo ""

echo "=== Gemini CLI ==="
echo "1. Merge MCP config:"
echo "   $HOOKS_PATH/gemini-cli/settings-snippet.json"
echo "   -> into: ~/.gemini/settings.json"
echo ""
echo "2. Create wrapper script ~/bin/gemini-skopos (see README for script content)"
echo "   Hook script: $HOOKS_PATH/gemini-cli/hooks.sh"
echo ""
echo "   Full instructions: $HOOKS_PATH/gemini-cli/README.md"
echo ""

echo "=== Codex ==="
echo "1. Merge MCP config:"
echo "   $HOOKS_PATH/codex/config-snippet.toml"
echo "   -> into: ~/.codex/config.toml"
echo ""
echo "2. Append hooks to your project AGENTS.md:"
echo "   cat $HOOKS_PATH/codex/AGENTS-snippet.md >> AGENTS.md"
echo ""
echo "   Full instructions: $HOOKS_PATH/codex/README.md"
echo ""

echo "=== Kiro ==="
echo "1. Merge MCP config:"
echo "   $HOOKS_PATH/kiro/mcp.json"
echo "   -> into: ~/.kiro/settings/mcp.json (global) or .kiro/settings/mcp.json (project)"
echo ""
echo "2. Copy steering doc to your project:"
echo "   mkdir -p .kiro/steering"
echo "   cp $HOOKS_PATH/kiro/steering-snippet.md .kiro/steering/skopos.md"
echo ""
echo "   Full instructions: $HOOKS_PATH/kiro/README.md"
echo ""

echo "=== OpenCode ==="
echo "1. Merge MCP config:"
echo "   $HOOKS_PATH/opencode/config-snippet.json"
echo "   -> into: ~/.config/opencode/opencode.json"
echo ""
echo "2. Add Skopos reporting instructions to your project AGENTS.md"
echo "   (see README for the block to append)"
echo ""
echo "   Full instructions: $HOOKS_PATH/opencode/README.md"
echo ""

echo "=== Environment Variables ==="
echo "Add to ~/.zshrc or ~/.bashrc:"
echo ""
echo "  export SKOPOS_API_KEY=your-key-here"
echo "  export SKOPOS_SERVER_URL=http://localhost:8080"
echo ""
echo "To share a session across agents in the same workspace:"
echo "  export SKOPOS_SESSION_ID=my-session-name"
echo ""

echo "=== Smoke Test ==="
echo "After applying at least one integration and starting skopos serve:"
echo ""
echo "  SKOPOS_API_KEY=your-key bash $HOOKS_PATH/shared/test-smoke.sh"
echo ""
echo "=== Done ==="
echo "Start the Skopos server and dashboard: skopos serve"
echo "Open the dashboard: http://localhost:8080"
