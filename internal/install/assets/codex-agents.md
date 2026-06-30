## Skopos Integration

When starting any session, report status to Skopos by running:
```bash
skopos report --agent-id "codex-$(hostname -s)" --agent-type codex \
  --workspace "$PWD" --status running --message "session started" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

When completing a session successfully, run:
```bash
skopos report --agent-id "codex-$(hostname -s)" --agent-type codex \
  --workspace "$PWD" --status succeeded --message "session complete" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

If a session ends with errors, run:
```bash
skopos report --agent-id "codex-$(hostname -s)" --agent-type codex \
  --workspace "$PWD" --status failed --message "session ended with error" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

## Skopos Blackboard

Use the blackboard to share knowledge across sessions and agents.

At session start, read prior findings for the current branch:

```bash
skopos blackboard read --branch "$(git branch --show-current)" || true
```

When you discover something worth recording (bug, decision, finding), write an entry:

```bash
skopos blackboard write \
  --scope branch --branch "$(git branch --show-current)" \
  --type finding --title "short description" --content "details" \
  --agent-id "codex-$(hostname -s)" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

Use `--type bug` or `--type debt` for critical issues — these are visible to all agents regardless of branch.
Use `--scope project` to share a finding with all agents on all branches.

Environment:
- `SKOPOS_SERVER_URL` — Skopos server URL (default: `http://localhost:8080`)
- `SKOPOS_API_KEY` — API key for write endpoints
- `SKOPOS_SESSION_ID` — optional; set to share a session across agents in the same workspace

## Skopos Plans

Use plans to coordinate multi-step work across sessions.

At task start, create a plan:

```bash
PLAN_ID=$(skopos plan create --name "Task name" --branch "$(git branch --show-current)" \
  --agent-id "codex-$(hostname -s)" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} 2>&1 | grep -o 'id=[^ ]*' | cut -d= -f2) || true
```

Add work items:

```bash
skopos plan item add --plan-id "$PLAN_ID" --title "Short description" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

Mark items done as you complete them:

```bash
skopos plan item done --plan-id "$PLAN_ID" --item-id "ITEM_ID" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```
