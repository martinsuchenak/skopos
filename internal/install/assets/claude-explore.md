---
name: skopos
description: Explore code via the skopos code index — symbols, definitions, callers, impact. Use before grep for any code-structure question.
argument-hint: <query or symbol name>
---

Answer using the skopos code index, not grep: $ARGUMENTS

1. Detect the surface (Bash): `skopos mode`
   - `remote <url>` → use the skopos MCP tools
   - `local` → use the CLI equivalents via Bash with `--json`

2. Pick the query path for the request:
   - "where is X / what is X" → `code_search` or `skopos search "$ARGUMENTS" --json`
   - "who calls X" → `code_callers` or `skopos who-calls X`
   - "what breaks if I change X" → `code_impact` or `skopos impact X`
   - "what's in file F" → `code_outline` or `skopos outline F`

3. Present hits as `fqn kind path:line`, read the source of the few most relevant hits, then answer.

If the index returns nothing (or `skopos index status` fails), say the index has no match and fall back to grep.
