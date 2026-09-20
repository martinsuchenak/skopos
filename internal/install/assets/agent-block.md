<!-- skopos:version:6 -->

## Code exploration: pick by question shape

- **Question names a symbol/function/class** → skopos code_search / code_symbol / code_callers (structure, callers, blast radius — grep can't answer these cheaply).
- **Question describes a role or behavior with no name** ("which class renders errors as HTML?") → skopos code_find (semantic).
- **Literal string, config value, or exhaustive listing** → grep/find is the right tool.

Rule: one skopos query before grep for anything structural; never grep-call-graph by hand.

Check once per session which skopos surface you have (Bash: `skopos mode`):
- `remote <url>` — skopos MCP tools: `code_search` (names/signatures), `code_symbol` (exact definitions with file:line), `code_outline` (a file's definitions), `code_callers` / `code_callees` (call graph), `code_impact` (what transitively breaks), `code_branch_diff` (index diff against the default branch)
- `local` — no MCP tools; same queries via the CLI: `skopos search`, `skopos symbol`, `skopos outline`, `skopos who-calls`, `skopos call-tree`, `skopos impact`, `skopos branch-diff` (all accept `--json`)

Scope queries to a subtree with `path` (CLI `--path`, MCP `path` param). Visibility, modifiers, and attributes are searchable text — e.g. `code_search q="private cache"`, or a route name to find its handler.

Only fall back to grep for: string literals, config values, env vars, non-symbol text, or exhaustive "find ALL occurrences" listings.
{{SKILL_LINE}}

## Shared memory — mandatory behavior

When a skopos server is reachable (remote mode), you MUST proactively save knowledge to the blackboard **during** the session, immediately when something is decided or discovered — do not wait to be asked or for the session to end:

- Architectural decisions (the why) → `blackboard_write` entry_type `decision`
- Bugs found or fixed → `bug`; known debt → `debt` (these float — always visible regardless of branch)
- Conventions and patterns → `context` or `finding`
- Scope: `branch` by default (shared with everyone on this branch), `project` for repo-wide facts
- Obsolete entries → `blackboard_delete` (entry IDs come from `blackboard_read`)

Be selective — only facts useful in a future session. Skip task details and temporary state.

## Session cadence (remote mode)

- Start of task: `skopos_context` with `workspace_id` (this repo's id, e.g. `github.com/owner/repo` — print it with `skopos workspace` or derive from `git remote get-url origin`) and `branch` — unscoped reads span every workspace on the server
- State changes: `report_status` with agent_type "{{AGENT_TYPE}}" and `workspace_id` (never "stuck"/"orphaned" — server-set). Report at least every ~10 minutes on long tasks — the server marks silent agents stuck after 15. Reuse ONE `session_id` for the whole task (omitting it creates a new session per call, fragmenting the timeline); the session hook prints the current id. Tool-hook heartbeats cover the gaps automatically.
- Multi-step work: `plan_create` / `plan_add_item` / `plan_update_item`; archive with `plan_archive` when done or abandoned
- Inbox work (when the user asks you to process captured ideas): `inbox_list` / `inbox_read` → `inbox_claim` → enrich via `inbox_update` (preserve the original content, append an `## Enrichment` section) → build a plan with the plan tools → `inbox_convert` with the new plan id
