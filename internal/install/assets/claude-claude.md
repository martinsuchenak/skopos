<!-- skopos:begin -->
## Skopos — shared memory and code intelligence

You have access to **skopos** MCP tools: shared memory (blackboard), plans, agent status, and a central code index (symbols, callers, impact). In any skopos-served workspace they are the fastest path to context.

### Code exploration — skopos first

For code structure questions (how something works, who calls what, where a symbol lives, blast radius of a change), use skopos tools BEFORE grep/find/Glob or Explore agents:

- Symbols: `code_search` (names/signatures), `code_symbol` (exact definitions with file:line)
- Structure: `code_outline` (a file's definitions), `code_callers` / `code_callees`
- Risk: `code_impact` (transitive "what breaks if I change this"), `code_branch_diff`

Fall back to grep only for: string literals, config values, env vars, non-symbol text, or exhaustive "find ALL occurrences" listings.

### Shared memory — mandatory during the session

Proactively record knowledge as it emerges; do not wait to be asked or for the session to end:

- Decisions (why) → `blackboard_write` entry_type `decision`
- Bugs found/fixed → `bug`; known debt → `debt` (these float across branches — always visible)
- Conventions/patterns → `context` or `finding`
- Scope: `branch` by default (shared on this branch), `project` for repo-wide facts
- Obsolete entries → `blackboard_delete`

Recall with `blackboard_read` (pass `workspace_id` + `branch`). Entry IDs are needed for delete.

### Session cadence

- Start: `skopos_context` (workspace_id + branch) — prior knowledge, active plans, in-flight sessions
- State changes: `report_status` (never "stuck"/"orphaned" — server-set)
- Planning: `plan_create` / `plan_add_item` / `plan_update_item`; archive with `plan_archive` when done or abandoned
<!-- skopos:end -->
