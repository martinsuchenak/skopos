---
id: skopos-search
kind: command
description: Search the skopos code index — symbols, definitions, callers, impact. Use before grep for any code-structure question.
targets: [claude, opencode, codex, copilot, kiro]
arguments:
  - { name: query, required: true, description: "Symbol name or search terms" }
---
Answer from the skopos code index, not grep: `{{arg:query}}`

1. Detect the surface (shell): `skopos mode` — `remote <url>` means the skopos MCP tools below; `local` means the CLI equivalents (`skopos search`, `symbol`, `outline`, `who-calls`, `call-tree`, `impact`, all with `--json`).

2. Pick the query path for the request:
   - "where is X / what is X" → {{tool code_search@skopos workspace_id="ws" q="UserService" limit=10}}
   - "who calls X" → {{tool code_callers@skopos workspace_id="ws" name="UserService::getFullName"}}
   - "what breaks if I change X" → {{tool code_impact@skopos workspace_id="ws" name="UserService::getFullName" depth=3}}
   - "what's in file F" → {{tool code_outline@skopos workspace_id="ws" path="app/Services/UserService.php"}}

   (Replace the example `workspace_id`, query, name, and path with the real values — the workspace id comes from `skopos index status` or the server dashboard.)

3. Present hits as `fqn kind path:line`, read the source of the few most relevant hits, then answer.

If the index returns nothing (or `skopos index status` fails), say the index has no match and fall back to grep.
