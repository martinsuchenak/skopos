# Inbox

The inbox is a workspace-scoped capture queue for unprocessed work: rough
ideas, future tasks, and notes not yet ready to action — the replacement for
a personal notes vault, wired into agent coordination. Design and decisions:
[docs/design/inbox.md](../design/inbox.md).

## Lifecycle

| Status | Meaning | Moves to |
|--------|---------|----------|
| `open` | captured, unprocessed | `in_progress` (claim), `discarded` |
| `in_progress` | claimed by an agent, being enriched | `converted`, `open` (release), `discarded` |
| `converted` | linked to a plan (`plan_id`) | `done` (auto), `discarded` |
| `done` | the linked plan completed | terminal |
| `discarded` | won't do / superseded | → `open` (restore), retention-cleaned |

Title, content, and tags are editable only while `open`/`in_progress` —
once converted, the item is a frozen record. Claiming is compare-and-swap:
another agent's `in_progress` item returns 409.

## The workflow

1. **Capture** — `inbox_create` (or `skopos inbox add`, or the dashboard).
   Title + markdown content; rough notes and code pointers are fine. Tags
   are normalized server-side (lowercase, `[a-z0-9._/-]`, ≤ 10, ≤ 40 chars).
2. **Claim** — an agent picks the item up (`inbox_claim`), moving it to
   `in_progress`.
3. **Enrich** — `inbox_update` appends findings to the content. The
   convention (carried in the tool description): preserve the original
   content and append an `## Enrichment — <agent>, <date>` section.
4. **Convert** — the agent authors a plan with the regular plan tools and
   links it with `inbox_convert {item_id, plan_id}`. The plan must live in
   the same workspace.
5. **Done** — completing the plan (explicitly or by finishing all items)
   automatically flips the linked items to `done`.

## Surfaces

- **REST**: `POST/GET /api/inbox`, `GET/PATCH/DELETE /api/inbox/{id}`,
  `POST /api/inbox/{id}/claim|convert|discard`. List rows carry a plain-text
  excerpt; `GET /{id}` returns the full markdown plus `content_html`
  (rendered server-side with goldmark + bluemonday — safe by construction,
  pinned by adversarial tests).
- **MCP**: `inbox_create`, `inbox_list`, `inbox_read`, `inbox_update`,
  `inbox_claim`, `inbox_convert`, `inbox_discard`. `skopos_context` includes
  an `inbox` section (open count + top 5 titles). In lean mode the tools are
  behind `tool_search`.
- **CLI**: `skopos inbox add|list|show|update|claim|convert|discard|restore|delete`.
  `add`/`update` take `--content`, `--file <path>`, or `--file -` (stdin) —
  migrating a notes vault is a shell loop. `--tag` is repeatable.
- **Dashboard**: the Inbox view has two layouts (remembered per browser):
  a filtered list and a kanban board — one lane per status, drag between
  lanes to claim/release/discard, drag onto a card to rank above it, drop
  at a lane's end to unpin. Both render content as markdown and edit
  through an embedded CodeMirror 6 editor.

## Priorities

Items are **partially ordered**: a `priority` (>= 1, smaller first) can be
set on any subset — items without one always sort after all prioritized
items (newest first). Priorities are editable while a item is
open/in_progress (the enrichment freeze applies). Surfaces: `priority` in
create/PATCH (`0` clears), `POST /api/inbox/reorder {ids}` renumbers the
ordered prefix 1..N, the dashboard's Pin/Unpin and drag ranking, and
`--priority N|clear` in the CLI.

## Unfiled items

Rough ideas legitimately precede the "where does this belong" decision. A
create may omit `workspace_id` (root key only): the item is **unfiled** —
visible only to the root key (in the "All workspaces" view, marked
`unfiled`), never merged into any workspace's list, and invisible to scoped
keys. File it later from the dashboard ("file into…"), the CLI
(`skopos inbox file --id X --workspace Y`), or PATCH `workspace_id`.
Filing/re-filing works while the item is open/in_progress.

## Scoping

Workspace-only, strict like plans (with the unfiled exception above): writes
require `workspace_id` for scoped principals; by-id operations on foreign
items return uniform 404s;
explicit-workspace reads out of scope return 403; unscoped reads for scoped
keys return exactly the key's workspace slice. There is no branch or session
scope — an inbox item is pre-work; the branch belongs to the plan it
becomes.

## Retention

`discarded` and `done` items are deleted by the cleanup worker after the
retention window (`cleanup.retention_days`); `open`, `in_progress`, and
`converted` items are never auto-deleted.
