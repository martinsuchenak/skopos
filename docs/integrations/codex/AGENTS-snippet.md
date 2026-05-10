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

Environment:
- `SKOPOS_SERVER_URL` — Skopos server URL (default: `http://localhost:8080`)
- `SKOPOS_API_KEY` — API key for write endpoints
- `SKOPOS_SESSION_ID` — optional; set to share a session across agents in the same workspace
