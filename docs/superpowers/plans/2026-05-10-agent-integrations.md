# Agent Integrations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create integration files (MCP configs, hook scripts, README instructions) for Claude Code, Gemini CLI, Codex, Kiro, and OpenCode so each agent automatically reports status to the Skopos dashboard.

**Architecture:** Two layers per agent — MCP server config at `http://localhost:9000/mcp` enables rich voluntary reporting via `report_status` tool; shell hook scripts fire `skopos report` CLI automatically on lifecycle events. A shared `skopos-session.sh` helper resolves session IDs from `$SKOPOS_SESSION_ID` env var, `.skopos-session` workspace file, or a deterministic `shasum` hash. All hook calls use `|| true` so a downed Skopos server never blocks agent work.

**Tech Stack:** Bash, JSON, YAML, `jq` (JSON parsing in hook scripts), `shasum` (cross-platform hashing), `skopos` CLI binary (built from this repo)

---

## File Structure

**Create:**
- `docs/integrations/shared/skopos-session.sh` — session ID resolver, sourced by all hook scripts
- `docs/integrations/shared/test-smoke.sh` — end-to-end smoke test against a live server
- `docs/integrations/shared/setup.sh` — interactive setup walkthrough (prints instructions per agent)
- `docs/integrations/claude-code/hooks.sh` — hook script called by Claude Code on lifecycle events
- `docs/integrations/claude-code/settings-snippet.json` — MCP server entry + PreToolUse/PostToolUse/Stop hook entries
- `docs/integrations/claude-code/skopos-skill.md` — skill file for manual `/skopos-report` slash command
- `docs/integrations/claude-code/README.md` — step-by-step setup instructions
- `docs/integrations/gemini-cli/hooks.sh` — hook script for Gemini lifecycle events
- `docs/integrations/gemini-cli/settings-snippet.json` — MCP server entry for `~/.gemini/settings.json`
- `docs/integrations/gemini-cli/README.md`
- `docs/integrations/codex/config-snippet.yaml` — MCP server entry for `~/.codex/config.yaml`
- `docs/integrations/codex/AGENTS-snippet.md` — hook instructions block for `AGENTS.md`
- `docs/integrations/codex/README.md`
- `docs/integrations/kiro/mcp.json` — MCP server config for `.kiro/settings/mcp.json`
- `docs/integrations/kiro/hooks-snippet.json` — lifecycle hook entries for Kiro's hook system
- `docs/integrations/kiro/README.md`
- `docs/integrations/opencode/config-snippet.json` — MCP + hook config block for OpenCode
- `docs/integrations/opencode/README.md`

**Modify:**
- `.gitignore` — add `.skopos-session` so workspace session ID files are not committed

---

## Task 1: Install skopos binary into PATH

**Files:**
- Read: `Taskfile.yml`

- [ ] **Step 1: Build the local binary**

```bash
task build-local
```

Expected output: creates `bin/skopos`.

- [ ] **Step 2: Symlink into PATH**

```bash
sudo ln -sf "$(pwd)/bin/skopos" /usr/local/bin/skopos
```

- [ ] **Step 3: Verify**

```bash
skopos --help
```

Expected: prints skopos usage. If this fails, the binary is not in PATH — stop and fix before continuing.

- [ ] **Step 4: Add `.skopos-session` to `.gitignore`**

Open `.gitignore` and append:

```
# Skopos session ID files (auto-generated per workspace)
.skopos-session
```

- [ ] **Step 5: Commit**

```bash
git add .gitignore
git commit -m "chore: add .skopos-session to gitignore"
```

---

## Task 2: Shared session ID helper

**Files:**
- Create: `docs/integrations/shared/skopos-session.sh`

- [ ] **Step 1: Create the test script**

Create `docs/integrations/shared/test-session.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/skopos-session.sh"

PASS=0; FAIL=0

check() {
  local desc="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "  PASS: $desc"
    ((PASS++))
  else
    echo "  FAIL: $desc — got '$got', want '$want'"
    ((FAIL++))
  fi
}

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# Test 1: env var takes priority
SKOPOS_SESSION_ID="env-session" check "env var" "$(skopos_session_id "$TMPDIR")" "env-session"
unset SKOPOS_SESSION_ID

# Test 2: file fallback
echo "file-session" > "$TMPDIR/.skopos-session"
check "file fallback" "$(skopos_session_id "$TMPDIR")" "file-session"
rm "$TMPDIR/.skopos-session"

# Test 3: hash generation is stable (same dir+date returns same ID twice)
id1=$(skopos_session_id "$TMPDIR")
id2=$(skopos_session_id "$TMPDIR")
check "hash stability" "$id1" "$id2"

# Test 4: hash is 12 chars
check "hash length" "${#id1}" "12"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
```

- [ ] **Step 2: Run the test to confirm it fails**

```bash
bash docs/integrations/shared/test-session.sh
```

Expected: error — `skopos-session.sh: No such file or directory`.

- [ ] **Step 3: Create the session helper**

Create `docs/integrations/shared/skopos-session.sh`:

```bash
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
  id=$(printf '%s-%s' "$workspace" "$(date +%Y-%m-%d)" | shasum -a 256 | cut -c1-12)
  echo "$id" > "$session_file"
  echo "$id"
}
```

- [ ] **Step 4: Run the test to confirm it passes**

```bash
bash docs/integrations/shared/test-session.sh
```

Expected:
```
  PASS: env var
  PASS: file fallback
  PASS: hash stability
  PASS: hash length

Results: 4 passed, 0 failed
```

- [ ] **Step 5: Commit**

```bash
git add docs/integrations/shared/skopos-session.sh docs/integrations/shared/test-session.sh
git commit -m "feat(integrations): add shared session ID helper"
```

---

## Task 3: Claude Code hook script

**Files:**
- Create: `docs/integrations/claude-code/hooks.sh`

Claude Code pipes a JSON object to the hook command's stdin on each event. `PreToolUse` sends `{"session_id":"...","tool_name":"...","tool_input":{...}}`. `Stop` sends `{"session_id":"...","stop_hook_active":false}`.

- [ ] **Step 1: Create the test**

Create `docs/integrations/claude-code/test-hooks.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

HOOKS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hooks.sh"
PASS=0; FAIL=0

check() {
  local desc="$1" code="$2"
  if [ "$code" -eq 0 ]; then
    echo "  PASS: $desc"
    ((PASS++))
  else
    echo "  FAIL: $desc — exited $code"
    ((FAIL++))
  fi
}

# These tests require a live skopos server on localhost:8080 to actually accept reports,
# but the hook must exit 0 even if the server is down (|| true).
# We test exit code only — visual confirmation via the dashboard.

echo '{"session_id":"test","tool_name":"Bash","tool_input":{}}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" pre-tool
check "pre-tool exits 0 (server down)" $?

echo '{"session_id":"test","tool_name":"Read","tool_response":{}}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" post-tool
check "post-tool exits 0 (server down)" $?

echo '{"session_id":"test","stop_hook_active":false}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" stop
check "stop exits 0 (server down)" $?

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
```

- [ ] **Step 2: Run the test to confirm it fails**

```bash
bash docs/integrations/claude-code/test-hooks.sh
```

Expected: error — `hooks.sh: No such file or directory`.

- [ ] **Step 3: Create the hook script**

Create `docs/integrations/claude-code/hooks.sh`:

```bash
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
```

- [ ] **Step 4: Run the test to confirm it passes**

```bash
bash docs/integrations/claude-code/test-hooks.sh
```

Expected:
```
  PASS: pre-tool exits 0 (server down)
  PASS: post-tool exits 0 (server down)
  PASS: stop exits 0 (server down)

Results: 3 passed, 0 failed
```

- [ ] **Step 5: Commit**

```bash
git add docs/integrations/claude-code/hooks.sh docs/integrations/claude-code/test-hooks.sh
git commit -m "feat(integrations): add Claude Code hook script"
```

---

## Task 4: Claude Code settings snippet, skill, and README

**Files:**
- Create: `docs/integrations/claude-code/settings-snippet.json`
- Create: `docs/integrations/claude-code/skopos-skill.md`
- Create: `docs/integrations/claude-code/README.md`

- [ ] **Step 1: Create and validate the settings snippet**

Create `docs/integrations/claude-code/settings-snippet.json`:

```json
{
  "_comment": "Merge this into ~/.claude/settings.json or .claude/settings.json. Replace SKOPOS_HOOKS_PATH with the absolute path to docs/integrations/claude-code/hooks.sh",
  "mcpServers": {
    "skopos": {
      "type": "http",
      "url": "http://localhost:9000/mcp"
    }
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bash SKOPOS_HOOKS_PATH pre-tool"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bash SKOPOS_HOOKS_PATH post-tool"
          }
        ]
      }
    ],
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bash SKOPOS_HOOKS_PATH stop"
          }
        ]
      }
    ]
  }
}
```

Validate the JSON is well-formed:

```bash
jq . docs/integrations/claude-code/settings-snippet.json
```

Expected: JSON pretty-printed with no errors.

- [ ] **Step 2: Create the skill file**

Create `docs/integrations/claude-code/skopos-skill.md`:

```markdown
---
name: skopos-report
description: Report current agent status to the Skopos dashboard. Use when you want to explicitly record what you are doing, planning, or blocked on.
---

Report your current status to Skopos using the report_status MCP tool.

Choose the most accurate status from: pending, thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, cancelled.

Call the report_status tool with:
- agent_id: "claude-code-<hostname>" (use `hostname -s` output)
- agent_type: "claude-code"
- workspace: the current working directory
- status: the status that best describes what you are doing right now
- message: a short human-readable description (e.g. "investigating the auth bug", "waiting for user input", "blocked on missing API key")
- snippet: (optional) a one-line excerpt of relevant output or code
```

- [ ] **Step 3: Create the README**

Create `docs/integrations/claude-code/README.md`:

```markdown
# Claude Code → Skopos Integration

Two layers: MCP (rich voluntary reporting) + hooks (automatic lifecycle reporting).

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH: `sudo ln -sf $(pwd)/bin/skopos /usr/local/bin/skopos`
- `jq` installed: `brew install jq`

## Step 1: Apply MCP + hooks config

Find the absolute path to the hooks script:

```bash
echo "$(pwd)/docs/integrations/claude-code/hooks.sh"
```

Open `~/.claude/settings.json` (create it if it doesn't exist) and merge in the contents of `settings-snippet.json`, replacing `SKOPOS_HOOKS_PATH` with the path above.

If you already have `mcpServers` or `hooks` sections, add the `skopos` entries to the existing objects — do not replace the whole file.

Set your API key in the environment (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 2: Install the skill (optional, for manual reporting)

Copy `skopos-skill.md` to your skills directory:

```bash
cp docs/integrations/claude-code/skopos-skill.md ~/.claude/plugins/skills/
```

Then use `/skopos-report` in any Claude Code session to report rich status.

## Step 3: Verify

Start a Claude Code session in any directory. Open the Skopos dashboard at `http://localhost:8080`. Use a tool (e.g. ask Claude to run `ls`). You should see a new session appear with status `running`.

## Session IDs

Sessions are resolved in this order:
1. `$SKOPOS_SESSION_ID` env var
2. `.skopos-session` file in the workspace root
3. Auto-generated hash (stable per workspace per day)

To share a session across agents, set `export SKOPOS_SESSION_ID=my-session` in your shell before starting any agents.
```

- [ ] **Step 4: Validate all files**

```bash
jq . docs/integrations/claude-code/settings-snippet.json && echo "JSON valid"
bash -n docs/integrations/claude-code/hooks.sh && echo "hooks.sh syntax ok"
```

Expected: both print their success lines.

- [ ] **Step 5: Commit**

```bash
git add docs/integrations/claude-code/
git commit -m "feat(integrations): add Claude Code MCP config, hooks, skill, and README"
```

---

## Task 5: Gemini CLI integration

**Files:**
- Create: `docs/integrations/gemini-cli/hooks.sh`
- Create: `docs/integrations/gemini-cli/settings-snippet.json`
- Create: `docs/integrations/gemini-cli/README.md`

> **Before writing the config snippet:** Run the following to check Gemini CLI's current settings format and MCP server config schema, then adapt the snippet to match:
> ```bash
> cat ~/.gemini/settings.json 2>/dev/null || echo "(no settings file yet)"
> gemini --help 2>/dev/null | head -20
> ```
> The MCP server entry format may differ from Claude Code's. The key field is likely `url` or `httpUrl`. Verify before writing.

- [ ] **Step 1: Check Gemini CLI's current config format**

```bash
cat ~/.gemini/settings.json 2>/dev/null || echo "(no settings file)"
gemini --help 2>/dev/null | grep -i mcp || echo "(no mcp flag)"
```

Note the exact field names for MCP server config. If the file doesn't exist, check Gemini CLI's documentation at `https://github.com/google-gemini/gemini-cli` for the current `settings.json` schema.

- [ ] **Step 2: Create the hook script**

Create `docs/integrations/gemini-cli/hooks.sh`:

```bash
#!/usr/bin/env bash
# Gemini CLI lifecycle hook — reports agent status to Skopos.
# Called by Gemini's hook mechanism with event type as $1.
# Usage: hooks.sh <start|stop>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../shared/skopos-session.sh
source "$SCRIPT_DIR/../shared/skopos-session.sh"

EVENT="${1:-stop}"
SKOPOS_SERVER_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}"
SKOPOS_API_KEY="${SKOPOS_API_KEY:-}"
AGENT_ID="gemini-$(hostname -s)"
SESSION_ID="$(skopos_session_id "$PWD")"

case "$EVENT" in
  start)
    skopos report \
      --server-url "$SKOPOS_SERVER_URL" \
      --api-key "$SKOPOS_API_KEY" \
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
      --api-key "$SKOPOS_API_KEY" \
      --agent-id "$AGENT_ID" \
      --agent-type "gemini" \
      --workspace "$PWD" \
      --session-id "$SESSION_ID" \
      --status succeeded \
      --message "session complete" || true
    ;;
esac
```

- [ ] **Step 3: Create the settings snippet**

Create `docs/integrations/gemini-cli/settings-snippet.json` using the schema verified in Step 1. A likely format (verify against actual Gemini CLI docs):

```json
{
  "_comment": "Merge into ~/.gemini/settings.json. Verify mcpServers schema matches your Gemini CLI version.",
  "mcpServers": {
    "skopos": {
      "url": "http://localhost:9000/mcp"
    }
  }
}
```

Validate:

```bash
jq . docs/integrations/gemini-cli/settings-snippet.json && echo "JSON valid"
```

- [ ] **Step 4: Create the README**

Create `docs/integrations/gemini-cli/README.md`:

```markdown
# Gemini CLI → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve`
- `skopos` binary in PATH
- Gemini CLI installed

## Step 1: Apply MCP config

Merge `settings-snippet.json` into `~/.gemini/settings.json`.

> Verify the `mcpServers` field names match your installed Gemini CLI version before applying.

## Step 2: Wire hooks

Gemini CLI hook support varies by version. Check `gemini --help` or the Gemini CLI docs for the current hook configuration method. Wire `hooks.sh start` and `hooks.sh stop` to session start and stop events respectively.

Set environment variables:

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start a Gemini CLI session. Open the Skopos dashboard at `http://localhost:8080`. You should see a `gemini-<hostname>` agent appear.

## Session IDs

Same resolution order as other agents — see `shared/skopos-session.sh`.
```

- [ ] **Step 5: Validate and commit**

```bash
jq . docs/integrations/gemini-cli/settings-snippet.json && echo "JSON valid"
bash -n docs/integrations/gemini-cli/hooks.sh && echo "syntax ok"
git add docs/integrations/gemini-cli/
git commit -m "feat(integrations): add Gemini CLI integration"
```

---

## Task 6: Codex integration

**Files:**
- Create: `docs/integrations/codex/config-snippet.yaml`
- Create: `docs/integrations/codex/AGENTS-snippet.md`
- Create: `docs/integrations/codex/README.md`

> **Before writing the config:** Check the installed Codex CLI config format:
> ```bash
> cat ~/.codex/config.yaml 2>/dev/null || cat ~/.codex/config.json 2>/dev/null || echo "(no config)"
> codex --help 2>/dev/null | head -20
> ```
> MCP server config field names may differ. Verify against `https://github.com/openai/codex` docs.

- [ ] **Step 1: Check Codex config format**

```bash
cat ~/.codex/config.yaml 2>/dev/null || cat ~/.codex/config.json 2>/dev/null || echo "(no config)"
codex --help 2>/dev/null | grep -i mcp || echo "(no mcp flag)"
```

Note the exact schema for MCP servers.

- [ ] **Step 2: Create the config snippet**

Create `docs/integrations/codex/config-snippet.yaml` (adjust field names from Step 1 if needed):

```yaml
# Merge into ~/.codex/config.yaml
# Verify mcpServers schema matches your Codex CLI version.
mcpServers:
  skopos:
    transport: http
    url: http://localhost:9000/mcp
```

- [ ] **Step 3: Create the AGENTS snippet**

Create `docs/integrations/codex/AGENTS-snippet.md`:

```markdown
## Skopos Integration

When starting work, run:
```bash
skopos report --agent-id "codex-$(hostname -s)" --agent-type codex --workspace "$PWD" --status running --message "session started" || true
```

When finishing work, run:
```bash
skopos report --agent-id "codex-$(hostname -s)" --agent-type codex --workspace "$PWD" --status succeeded --message "session complete" || true
```

Environment variables:
- `SKOPOS_SERVER_URL` (default: `http://localhost:8080`)
- `SKOPOS_API_KEY`
- `SKOPOS_SESSION_ID` (optional, auto-generated per workspace if not set)
```

- [ ] **Step 4: Create the README**

Create `docs/integrations/codex/README.md`:

```markdown
# Codex → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve`
- `skopos` binary in PATH
- Codex CLI installed

## Step 1: Apply MCP config

Merge `config-snippet.yaml` into `~/.codex/config.yaml`.

> Verify the `mcpServers` field names match your installed Codex CLI version.

## Step 2: Wire hooks

Append `AGENTS-snippet.md` to your project's `AGENTS.md`. Codex reads AGENTS.md for behavioral instructions including shell commands to run at session boundaries.

Set environment variables:

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start a Codex session. Open `http://localhost:8080`. You should see a `codex-<hostname>` agent appear.
```

- [ ] **Step 5: Validate and commit**

```bash
# Validate YAML (requires yq or python)
python3 -c "import sys,yaml; yaml.safe_load(open('docs/integrations/codex/config-snippet.yaml'))" && echo "YAML valid"
git add docs/integrations/codex/
git commit -m "feat(integrations): add Codex integration"
```

---

## Task 7: Kiro integration

**Files:**
- Create: `docs/integrations/kiro/mcp.json`
- Create: `docs/integrations/kiro/hooks-snippet.json`
- Create: `docs/integrations/kiro/README.md`

> **Before writing:** Check Kiro's MCP and hook config format:
> ```bash
> cat .kiro/settings/mcp.json 2>/dev/null || echo "(no kiro mcp config)"
> ls .kiro/ 2>/dev/null || echo "(no .kiro dir)"
> ```
> Verify against Kiro's documentation for the exact MCP server and hook schema.

- [ ] **Step 1: Check Kiro config format**

```bash
cat .kiro/settings/mcp.json 2>/dev/null || echo "(no kiro mcp config)"
ls .kiro/ 2>/dev/null || echo "(no .kiro dir)"
```

Note the schema.

- [ ] **Step 2: Create the MCP config**

Create `docs/integrations/kiro/mcp.json` (adjust from Step 1 findings):

```json
{
  "_comment": "Place at .kiro/settings/mcp.json in your workspace. Verify schema matches your Kiro version.",
  "mcpServers": {
    "skopos": {
      "url": "http://localhost:9000/mcp"
    }
  }
}
```

Validate:

```bash
jq . docs/integrations/kiro/mcp.json && echo "JSON valid"
```

- [ ] **Step 3: Create the hooks snippet**

Create `docs/integrations/kiro/hooks-snippet.json` (adjust schema from Kiro docs):

```json
{
  "_comment": "Merge into .kiro/hooks/ or your Kiro hook config. Verify hook schema matches your Kiro version.",
  "hooks": [
    {
      "event": "session.start",
      "command": "skopos report --agent-id \"kiro-$(hostname -s)\" --agent-type kiro --workspace \"$PWD\" --status running --message \"session started\" || true"
    },
    {
      "event": "session.stop",
      "command": "skopos report --agent-id \"kiro-$(hostname -s)\" --agent-type kiro --workspace \"$PWD\" --status succeeded --message \"session complete\" || true"
    }
  ]
}
```

Validate:

```bash
jq . docs/integrations/kiro/hooks-snippet.json && echo "JSON valid"
```

- [ ] **Step 4: Create the README**

Create `docs/integrations/kiro/README.md`:

```markdown
# Kiro → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve`
- `skopos` binary in PATH
- Kiro installed

## Step 1: Apply MCP config

Copy `mcp.json` to `.kiro/settings/mcp.json` in your workspace:

```bash
mkdir -p .kiro/settings
cp docs/integrations/kiro/mcp.json .kiro/settings/mcp.json
```

> Verify the field names match your Kiro version before applying.

## Step 2: Wire hooks

Merge `hooks-snippet.json` into your Kiro hook configuration. The exact location depends on your Kiro version — check Kiro's docs for the hook config path.

Set environment variables:

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start a Kiro session. Open `http://localhost:8080`. You should see a `kiro-<hostname>` agent.
```

- [ ] **Step 5: Commit**

```bash
git add docs/integrations/kiro/
git commit -m "feat(integrations): add Kiro integration"
```

---

## Task 8: OpenCode integration

**Files:**
- Create: `docs/integrations/opencode/config-snippet.json`
- Create: `docs/integrations/opencode/README.md`

> **Before writing:** Check OpenCode's config format:
> ```bash
> cat ~/.config/opencode/config.json 2>/dev/null || echo "(no opencode config)"
> opencode --help 2>/dev/null | head -20
> ```
> Verify against `https://github.com/sst/opencode` docs for the current MCP server and hook schema.

- [ ] **Step 1: Check OpenCode config format**

```bash
cat ~/.config/opencode/config.json 2>/dev/null || echo "(no opencode config)"
opencode --help 2>/dev/null | grep -i mcp || echo "(no mcp flag)"
```

- [ ] **Step 2: Create the config snippet**

Create `docs/integrations/opencode/config-snippet.json` (adjust from Step 1 findings):

```json
{
  "_comment": "Merge into ~/.config/opencode/config.json. Verify schema matches your OpenCode version.",
  "mcp": {
    "skopos": {
      "url": "http://localhost:9000/mcp",
      "type": "remote"
    }
  }
}
```

Validate:

```bash
jq . docs/integrations/opencode/config-snippet.json && echo "JSON valid"
```

- [ ] **Step 3: Create the README**

Create `docs/integrations/opencode/README.md`:

```markdown
# OpenCode → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve`
- `skopos` binary in PATH
- OpenCode installed

## Step 1: Apply MCP config

Merge `config-snippet.json` into `~/.config/opencode/config.json`.

> Verify the `mcp` field names match your OpenCode version.

## Step 2: Wire hooks

OpenCode's hook system varies by version. Check OpenCode's docs for lifecycle hook configuration. Use the following commands for start/stop events:

```bash
# On session start:
skopos report --agent-id "opencode-$(hostname -s)" --agent-type opencode \
  --workspace "$PWD" --status running --message "session started" || true

# On session stop:
skopos report --agent-id "opencode-$(hostname -s)" --agent-type opencode \
  --workspace "$PWD" --status succeeded --message "session complete" || true
```

Set environment variables:

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start an OpenCode session. Open `http://localhost:8080`. You should see an `opencode-<hostname>` agent.
```

- [ ] **Step 4: Validate and commit**

```bash
jq . docs/integrations/opencode/config-snippet.json && echo "JSON valid"
git add docs/integrations/opencode/
git commit -m "feat(integrations): add OpenCode integration"
```

---

## Task 9: Smoke test script

**Files:**
- Create: `docs/integrations/shared/test-smoke.sh`

This test requires a live Skopos server. It exercises the CLI, REST, and MCP layers end-to-end.

- [ ] **Step 1: Create the smoke test**

Create `docs/integrations/shared/test-smoke.sh`:

```bash
#!/usr/bin/env bash
# End-to-end smoke test for Skopos integrations.
# Requires: skopos server running on localhost:8080 (MCP on :9000)
# Usage: SKOPOS_API_KEY=your-key bash docs/integrations/shared/test-smoke.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/skopos-session.sh"

BASE_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}"
API_KEY="${SKOPOS_API_KEY:-}"
SESSION_ID="smoke-test-$(date +%s)"
PASS=0; FAIL=0

check() {
  local desc="$1" code="$2"
  if [ "$code" -eq 0 ]; then
    echo "  PASS: $desc"
    ((PASS++))
  else
    echo "  FAIL: $desc"
    ((FAIL++))
  fi
}

echo "==> Smoke test against $BASE_URL"

# 1. CLI report
echo "--- Layer 1: CLI"
skopos report \
  --server-url "$BASE_URL" \
  --api-key "$API_KEY" \
  --agent-id "smoke-test-agent" \
  --agent-type "claude-code" \
  --workspace "$PWD" \
  --session-id "$SESSION_ID" \
  --status running \
  --message "smoke test via CLI"
check "CLI report accepted" $?

# 2. REST report
echo "--- Layer 2: REST"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST "$BASE_URL/api/reports" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -d "{\"agent_id\":\"smoke-rest\",\"agent_type\":\"claude-code\",\"workspace\":\"$PWD\",\"session_id\":\"$SESSION_ID\",\"status\":\"running\",\"message\":\"smoke test via REST\"}")
[ "$STATUS" = "200" ] || [ "$STATUS" = "201" ]
check "REST report accepted (status $STATUS)" $?

# 3. Session appears in API
echo "--- Verification"
SESSIONS=$(curl -s "$BASE_URL/api/sessions")
echo "$SESSIONS" | jq -e ".[] | select(.id == \"$SESSION_ID\")" > /dev/null 2>&1 || \
  echo "$SESSIONS" | jq -e ".[].id" | grep -q "$SESSION_ID"
check "Session visible in GET /api/sessions" $?

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
```

- [ ] **Step 2: Run the smoke test against a live server**

Start the server in a separate terminal first:

```bash
SKOPOS_API_KEY=test-key skopos serve
```

Then run:

```bash
SKOPOS_API_KEY=test-key bash docs/integrations/shared/test-smoke.sh
```

Expected:
```
==> Smoke test against http://localhost:8080
--- Layer 1: CLI
  PASS: CLI report accepted
--- Layer 2: REST
  PASS: REST report accepted (status 200)
--- Verification
  PASS: Session visible in GET /api/sessions

Results: 3 passed, 0 failed
```

- [ ] **Step 3: Commit**

```bash
git add docs/integrations/shared/test-smoke.sh
git commit -m "feat(integrations): add smoke test script"
```

---

## Task 10: Setup script and final commit

**Files:**
- Create: `docs/integrations/shared/setup.sh`

- [ ] **Step 1: Create setup.sh**

Create `docs/integrations/shared/setup.sh`:

```bash
#!/usr/bin/env bash
# Interactive setup walkthrough for Skopos agent integrations.
# Run from the skopos repo root.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOOKS_PATH="$REPO_ROOT/docs/integrations"

echo "=== Skopos Agent Integration Setup ==="
echo ""
echo "Skopos repo: $REPO_ROOT"
echo ""

# Verify binary
if ! command -v skopos &>/dev/null; then
  echo "ERROR: skopos not found in PATH."
  echo "Build and install first:"
  echo "  cd $REPO_ROOT && task build-local"
  echo "  sudo ln -sf $REPO_ROOT/bin/skopos /usr/local/bin/skopos"
  exit 1
fi
echo "skopos binary: $(command -v skopos)"
echo ""

# Verify jq
if ! command -v jq &>/dev/null; then
  echo "WARNING: jq not found. Hook scripts require jq for JSON parsing."
  echo "Install: brew install jq"
  echo ""
fi

echo "=== Claude Code ==="
echo "MCP + hooks settings snippet:"
echo "  $HOOKS_PATH/claude-code/settings-snippet.json"
echo ""
echo "Replace SKOPOS_HOOKS_PATH with:"
echo "  $HOOKS_PATH/claude-code/hooks.sh"
echo ""
echo "Merge into: ~/.claude/settings.json"
echo "Full instructions: $HOOKS_PATH/claude-code/README.md"
echo ""

echo "=== Gemini CLI ==="
echo "Settings snippet: $HOOKS_PATH/gemini-cli/settings-snippet.json"
echo "Merge into: ~/.gemini/settings.json"
echo "Hook script: $HOOKS_PATH/gemini-cli/hooks.sh"
echo "Full instructions: $HOOKS_PATH/gemini-cli/README.md"
echo ""

echo "=== Codex ==="
echo "Config snippet: $HOOKS_PATH/codex/config-snippet.yaml"
echo "Merge into: ~/.codex/config.yaml"
echo "AGENTS block: $HOOKS_PATH/codex/AGENTS-snippet.md"
echo "Full instructions: $HOOKS_PATH/codex/README.md"
echo ""

echo "=== Kiro ==="
echo "MCP config: $HOOKS_PATH/kiro/mcp.json"
echo "Copy to: .kiro/settings/mcp.json (in your workspace)"
echo "Hooks: $HOOKS_PATH/kiro/hooks-snippet.json"
echo "Full instructions: $HOOKS_PATH/kiro/README.md"
echo ""

echo "=== OpenCode ==="
echo "Config snippet: $HOOKS_PATH/opencode/config-snippet.json"
echo "Merge into: ~/.config/opencode/config.json"
echo "Full instructions: $HOOKS_PATH/opencode/README.md"
echo ""

echo "=== Environment Variables (add to ~/.zshrc or ~/.bashrc) ==="
echo "  export SKOPOS_API_KEY=your-key-here"
echo "  export SKOPOS_SERVER_URL=http://localhost:8080"
echo ""

echo "=== Run smoke test ==="
echo "  SKOPOS_API_KEY=your-key bash $HOOKS_PATH/shared/test-smoke.sh"
```

- [ ] **Step 2: Validate and run the setup script**

```bash
bash -n docs/integrations/shared/setup.sh && echo "syntax ok"
bash docs/integrations/shared/setup.sh
```

Expected: prints setup instructions with resolved absolute paths.

- [ ] **Step 3: Final commit**

```bash
git add docs/integrations/shared/setup.sh
git commit -m "feat(integrations): add setup walkthrough script"
```

---

## Self-Review Checklist

After all tasks are complete, run:

```bash
# All JSON snippets are valid
for f in docs/integrations/**/*.json; do jq . "$f" > /dev/null && echo "OK: $f"; done

# All shell scripts pass syntax check
for f in docs/integrations/**/*.sh; do bash -n "$f" && echo "OK: $f"; done

# Full smoke test passes
SKOPOS_API_KEY=test-key bash docs/integrations/shared/test-smoke.sh

# Session helper tests pass
bash docs/integrations/shared/test-session.sh

# Claude Code hook tests pass
bash docs/integrations/claude-code/test-hooks.sh
```
