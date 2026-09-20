# Design: Inbox — a workspace-scoped capture queue for unprocessed work

Status: implemented (v0.4.0, 2026-09-20). Naming, markdown rendering,
tags, the board layout, partial priorities, and unfiled captures were
decided across three review rounds; decisions 1–13 below are as shipped.
Motivation: rough ideas and unprocessed tasks currently live as notes in an
Obsidian vault, disconnected from the agent coordination skopos already
provides. The wanted workflow: capture an item (title + markdown content
with pointers to code/files, tagged) → when ready, an AI agent claims it,
enriches the content, converts it into a plan → when the plan completes,
the item is completed. Workspace-scoped; surfaced via API, MCP, UI, and CLI.

## Options investigated

1. **New `inbox` domain (chosen).** A dedicated `internal/inbox`
   package (handler → service → storage) with its own table and lifecycle.
   Pros: the lifecycle (claim → enrich → convert → done/discarded) and the
   item→plan link are first-class; blackboard and plans keep their
   semantics; every surface (REST/MCP/CLI/UI/events/cleanup) follows the
   established per-domain pattern, so the cost is mostly mechanical.
   Cons: one more domain package to maintain.
2. **Blackboard `entry_type: idea`.** Rejected: blackboard entries are
   durable knowledge with scope-promotion semantics — no lifecycle, no
   claiming, no link to plans. Floating rules ("bug"/"debt" cross branches)
   are wrong for a personal queue, and conversion would leave no traceable
   connection to the plan it produced.
3. **Plans with a `backlog` status.** Rejected: plans are structured,
   multi-item work units with dependency machinery and auto-completion; a
   raw markdown note is not that. Mixing raw captures into plan lists breaks
   plan semantics and clutters every plan consumer (dashboard, skopos_context).

Naming: **`inbox`** — it names the container ("things waiting to be
processed") rather than a direction, and matches the lifecycle's GTD shape.
`incoming` was the runner-up. Surfaces: `internal/inbox`, `/api/inbox`,
MCP `inbox_*`, CLI `skopos inbox`, SSE type `inbox`, UI view "Inbox",
items are "inbox items" (`inbox_items` table, model `Item`).

## Decisions

1. **Workspace-only scoping, strict like plans.** Writes require
   `workspace_id` for every principal; by-id ops on foreign items → uniform
   404 (`requireInboxScopeQuiet` pattern); explicit-workspace ops out of
   scope → 403 with accessible list; unscoped list for scoped keys
   loop-merges the principal's workspace slice. No branch/session scope —
   an inbox item is pre-work; the branch (if any) is a property of the plan
   it becomes. New row in the api-keys enforcement matrix, rule = membership.
2. **Lifecycle** (one `status` column, transitions enforced in the service):

   | status | meaning | exits |
   |---|---|---|
   | `open` | captured, unprocessed | → `in_progress` (claim), `discarded` |
   | `in_progress` | claimed by an agent, being enriched | → `converted` (convert), `open` (release), `discarded` |
   | `converted` | linked plan created (`plan_id` set) | → `done` (auto, plan completed), `discarded` |
   | `done` | the linked plan completed | terminal (derived from the plan) |
   | `discarded` | rejected / won't do | → `open` (restore — the undo for an
   accidental discard, a real risk with drag-to-discarded; clears any stale
   claim), retention-cleaned |

   Title/content/tags are editable only in `open`/`in_progress` (enrichment);
   `converted`/`done`/`discarded` are frozen — mirroring the plan freeze
   semantics (`assertTransitionAllowed`).
3. **Convert links, does not create.** `convert` takes an existing
   `plan_id` (must exist, same workspace, in caller scope) and sets
   `plan_id` + status `converted`. The agent authors the plan with the
   existing plan tools (items, phases, deps) — no plan-creation logic is
   duplicated in the inbox service and the services stay decoupled.
   Converting an already-converted item → 409.
4. **Plan completion auto-completes the item.** `plans.Service` gains
   `SetCompletionHook(func(planID string))`, fired post-commit whenever a
   plan transitions to completed (explicit PATCH and the all-items-done
   auto path). serve.go wires it (registrar pattern, background context) to
   `inbox.Service.CompleteForPlan`, which flips `converted` → `done` for the
   linked items and publishes an `inbox` event. No import in either
   direction; a deleted plan leaves the item `converted` with a dangling
   `plan_id` (hydration shows no plan; discard stays available).
5. **Claiming mirrors plan items.** `claimed_by_agent_id` +
   compare-and-swap claim (`open` + unclaimed → `in_progress`); losers get
   409 `ErrClaimConflict`. Claim with empty agent id releases (back to
   `open`). `claimed_by` is kept after conversion as provenance.
6. **Content is markdown, enriched in place.** One mutable `content`
   column; no revision history in v1. The `inbox_update` tool/handler
   description carries the convention: preserve the original content and
   append an `## Enrichment — <agent>, <date>` section with findings,
   pointers, and the proposed plan outline — so the raw capture is never
   lost without schema machinery.
7. **Tags in v1.** A normalized set per item: lowercased, trimmed,
   deduped, charset `[a-z0-9._/-]`, ≤ 40 chars each, ≤ 10 tags — invalid
   tokens are dropped server-side, never rejected. Stored as a JSON text
   array (`'["refactor","auth"]'`); list filtering via `?tag=` uses a
   delimited `LIKE '%"tag"%'` (the charset excludes the `"` delimiter, so
   matches cannot straddle tokens). Tags ride create/update payloads as a
   `tags` string array everywhere (REST, MCP, CLI `--tag` repeatable, UI
   chips).
8. **Rendered markdown via goldmark, server-side.** Storage stays raw
   markdown (the MCP/CLI surface — agents want source, not HTML). For the
   dashboard, the server renders at read time with
   `github.com/yuin/goldmark` (CommonMark + GFM; default config **without**
   `html.WithUnsafe`, so raw HTML in the source is escaped, not passed
   through) and sanitizes the output with
   `github.com/microcosm-cc/bluemonday` (`UGCPolicy` — blocks
   `javascript:`/`data:` hrefs, scripts, event handlers). Both deps are
   pure Go (the no-CGO constraint holds). `GET /api/inbox/{id}` returns
   `content_html` alongside `content`; list responses omit `content_html`
   and carry a plain-text excerpt (~200 chars) for card previews, matching
   the expand-fetches-detail UI flow. **Dashboard injection is imperative,
   not `x-html`**: the Alpine CSP build prohibits the html directive
   outright (verified live — it errors on sight and renders an empty
   element), so `reloadInboxItem` sets `innerHTML` from first-party TS by
   querying a `:data-md-body` row attribute (`$refs` does not resolve
   inside `x-for` templates either); a template guard test
   (`TestTemplatesAvoidXHtml`) pins this. CSP is unchanged
   (`script-src 'self'` stays the backstop). Rendering lives in
   `internal/inbox/render.go` — extract to a shared package if the
   blackboard adopts it later. Sanitization is pinned by adversarial unit
   tests (script injection, `onerror` handlers, `javascript:`/`data:`
   links, nested raw HTML).
9. **Markdown editing with CodeMirror 6.** The Create/Edit modals edit the
   raw markdown in an embedded CodeMirror 6 editor
   (`@codemirror/{state,view,commands,language,lang-markdown}`; a lean
   markdown setup tree-shakes with the existing Vite/Bun build to roughly a
   third of Ace's footprint). Chosen over `ace.c9.io`: Ace's mode/theme
   machinery historically relies on `new Function` code paths — under this
   dashboard's strict `script-src 'self'` no-eval CSP it needs special
   prebuilt variants or an `'unsafe-eval'` relaxation, whereas CM6 is
   designed for strict CSP (no eval; its injected styles are covered by the
   existing `style-src 'unsafe-inline'`). Theming follows the dashboard's
   `.dark`/`.light` classes via CSS. A small plain-TS wrapper (mount into a
   modal container on open, read `view.state.doc.toString()` on submit)
   keeps the document out of Alpine's reactive state; the wrapper is
   reusable if the blackboard entry modal adopts markdown later. The
   editor carries a markdown `HighlightStyle` whose colors reference CSS
   variables (defined per theme in style.css) — CM6 applies zero syntax
   styling by default, and CSS vars let dark/light switching work without
   swapping editor facets. Close-with-unsaved-edits (backdrop, Escape,
   Cancel) asks for confirmation against a baseline snapshot; an untouched
   modal closes immediately. Cmd/Ctrl+Enter submits.
10. **Cleanup:** retention deletes `discarded` and `done` items past the
   cutoff (same rule as completed/archived plans); `open`, `in_progress`,
   and `converted` are never auto-deleted.

11. **Partial priority ordering.** Items may carry a `priority` (integer
    >= 1); the shared sort is `prioritized ascending first, then the
    unprioritized tail newest-first` — items without a priority always sit
    after prioritized ones, exactly the "I only rank some" shape. Set/clear
    rides create/PATCH (`0` clears); `POST /api/inbox/reorder {ids}`
    renumbers a whole ordered prefix 1..N in one atomic call (the board's
    drag-and-drop; every id must be open/in_progress). Priorities are
    editable only while open/in_progress (the enrichment freeze applies).
    MCP: `priority` on create (>= 1) and update (-1 clears, 0/absent
    unchanged — ToolRequest cannot express absence). CLI:
    `--priority N` on add, `--priority N|clear` on update, `#n` in list.
12. **Board layout (swimlanes) beside the list.** A list/board toggle
    (persisted in localStorage, `skopos:inboxLayout`) switches between the
    filtered list and a kanban board: one lane per status, the board always
    showing every lane (status chips are list-only and restored on
    switch-back). Drag & drop (plain HTML5 DnD — method handlers, CSP-safe):
    open -> in_progress = claim, in_progress -> open = release, anything
    actionable -> discarded = discard; converted/done are marked "auto"
    (convert links a plan; done follows the plan). Within a lane, dropping a
    card ON a ranked card takes that rank (the prefix renumbers via
    reorder, suffix included); dropping at the lane's end unprioritizes.
    Cross-lane moves compact the source lane's ranks. The list view gains
    Pin/Unpin (pin = rank first via reorder) and `#rank` badges; lane cards
    expand inline with the same rendered-markdown injection as list rows.

13. **Unfiled captures (workspace may be TBD).** A create may omit
    `workspace_id` — the item is stored with a NULL workspace and is visible
    ONLY to root principals (unfiltered reads include it; concrete-workspace
    reads never do; scoped keys get uniform 404s by id). Root-only because a
    scoped key's unfiled item would be invisible to its own creator — scoped
    creates must name a workspace. This deliberately diverges from the
    blackboard's no-floating-entries rule: unfiled inbox items are personal
    captures that merge into NO workspace's view, so there is no
    cross-tenant poisoning surface (the blackboard's floating rows rendered
    into every bundle — these render into none). Filing (`workspace_id` on
    PATCH, open/in_progress only) is an explicit-workspace operation:
    out-of-scope targets get the actionable 403; re-filing is allowed while
    editable; converting an unfiled item is impossible by construction (no
    plan's workspace can match ""). Items are single-workspace by design —
    "belongs to multiple" is answered by filing it once and converting per
    workspace if it truly spans repos (multi-parent items would puncture the
    authz matrix: scoped reads, SSE attribution, and convert validation are
    all single-workspace). The dashboard capture modal carries an explicit
    workspace picker (inline validation error when unanswered — the failure
    that motivated this was a silent toast), an "Unfiled — decide later"
    option for root, unfiled badges, and a "file into…" select on unfiled
    cards.

Deferred (revisit after use): a `ready` substate for "action this next"
(v1: the human simply tells the agent, or the agent picks from
`skopos_context`), full-text search beyond `q` LIKE, revision history,
tag autocomplete.

## Schema (additive, internal/db/schema.sql)

```sql
CREATE TABLE IF NOT EXISTS inbox_items (
    id                  TEXT PRIMARY KEY,
    workspace_id        TEXT NOT NULL,
    title               TEXT NOT NULL,
    content             TEXT NOT NULL DEFAULT '',   -- markdown (source of truth)
    tags                TEXT NOT NULL DEFAULT '[]', -- JSON array, normalized
    status              TEXT NOT NULL DEFAULT 'open',
    claimed_by_agent_id TEXT,
    author_agent_id     TEXT NOT NULL,
    plan_id             TEXT,                        -- set on convert
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_workspace ON inbox_items(workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_inbox_plan ON inbox_items(plan_id);
```

UUIDv7 ids (`internal/ids`), `workspace_id` required on insert (the
NULL-row lesson from the blackboard migration: no floating items ever).
Detail reads hydrate a `plan: {id, name, status}` summary via LEFT JOIN
when `plan_id` is set. Service follows the house pattern: `Store`
interface, `now func() time.Time`, `SetPublisher` (publishes
`events.TypeInbox` with the item's authoritative workspace post-commit),
`RunInTx` for transitions.

## REST API

| Method | Path | Notes |
|---|---|---|
| POST | `/api/inbox` | `{workspace_id, title, content?, tags?, author_agent_id}` → 201 item |
| GET | `/api/inbox` | filters `workspace_id`/`workspace`, `status`, `tag`, `q` (LIKE title+content); scoped keys see their slice; rows carry a plain-text excerpt |
| GET | `/api/inbox/{id}` | full item: `content`, `content_html`, `tags`, hydrated plan summary |
| PATCH | `/api/inbox/{id}` | `{title?, content?, tags?}` — enrichment; frozen after `in_progress` |
| POST | `/api/inbox/{id}/claim` | `{agent_id}` → `in_progress`; empty agent_id releases → `open`; 409 on foreign claim |
| POST | `/api/inbox/{id}/convert` | `{plan_id}` → `converted`; 409 if already converted; 400 plan/item workspace mismatch |
| POST | `/api/inbox/{id}/discard` | → `discarded` |
| DELETE | `/api/inbox/{id}` | hard delete (confirm-gated in UI) |

Errors map like plans: `ErrInvalidInput` → 400, `ErrNotFound`/scope-quiet →
404, `ErrOutOfScope` explicit → 403, `ErrClaimConflict`/already-converted →
409. Registered via `routes.RegisterInbox` + `inbox_routes.go` `init()`
(self-registration), `RegisterRoutes` gains the handler param.
`openapi.yaml` updated in lockstep (TestOpenAPIMatchesRoutes).

## MCP

Per-tool files in `cmd/mcp` registering via a new `RegisterInboxTool`
(slice + `NewMCPHandler` gains the inbox service — 6th param):

- `inbox_create` `{workspace_id, title, content, tags?, author_agent_id}`
- `inbox_list` `{workspace_id?, status?, tag?, q?}` — compact rows (id, title, status, age, tags)
- `inbox_read` `{item_id}` — full markdown content + plan link
- `inbox_update` `{item_id, title?, content?, tags?}` — enrichment (append-convention in the description)
- `inbox_claim` `{item_id, agent_id}` — claim/release (empty = release)
- `inbox_convert` `{item_id, plan_id}`
- `inbox_discard` `{item_id}`

All inherit enforcement through the service (no MCP-layer authz). Not added
to `coreTools` — in lean mode they stay behind `tool_search` (the inbox is
an occasional workflow, not a daily driver). `skopos_context` gains an
`inbox` section (open count + up to 5 `{id, title, age}` with
`flattenText` titles) by extending the context-tool registration signature;
the MCP instructions text gains one line: capture → `inbox_create`,
process → claim/enrich/convert via plan tools.

## CLI (`cmd/inbox.go`, mirrors `cmd/plans.go`)

```
skopos inbox add     --title "..." [--content s | --file note.md | stdin] [--tag a --tag b] [--workspace]
skopos inbox list    [--status open] [--tag x] [--workspace]   # ID STATUS AGE TAGS TITLE (+ →plan)
skopos inbox show    --id                                      # raw markdown + tags + plan link
skopos inbox update  --id [--title] [--content-file] [--tag ...]
skopos inbox claim   --id [--agent-id]
skopos inbox convert --id --plan-id
skopos inbox discard --id
```

`--file -` reads stdin, `--file path` reads a markdown file — the Obsidian
migration path is a shell loop over vault notes. Usual
`--server-url`/`--api-key`/`SKOPOS_AGENT_ID` flags; `workspaceOrDefault`
resolves the workspace.

## UI

New `inbox` view (sidebar, after Plans): status filter chips
(Open / In progress / Converted / Done / Discarded) and an active-tag chip
over a card list — title, status badge, age, author, claim, tags, plain-text
excerpt; converted cards show the linked plan's name + status badge.
Click expands (fetches the detail): **rendered markdown** (`content_html`
injected imperatively — see decision 8) + actions: **Edit** (modal: title, tags input, raw markdown
in the embedded CodeMirror 6 editor — decision 9), Claim, Convert (opens
the existing plan-create modal prefilled with the item title, then calls
convert with the new plan id), **Discard**, and **Delete** (hard delete
behind the existing confirm pattern). The Create modal uses the same
editor + fields, workspace resolved via `writeWorkspace()` like other
writes. Clicking a tag chip sets the `tag` filter. `main.ts`: `fetchInbox`, SSE frame handler for `inbox`,
`inboxStatusClass` colors (open amber, in_progress cyan, converted violet,
done emerald, discarded zinc), delete kinds wired into `confirmDelete`.

## Events, cleanup, docs

- `events.TypeInbox = "inbox"`; published service-layer post-commit on
  every mutation incl. the completion hook.
- `internal/cleanup`: `DELETE FROM inbox_items WHERE status IN ('discarded','done') AND updated_at < ?`.
- Docs: new `docs/concepts/inbox.md`, README feature list, CHANGELOG,
  `internal/install/assets/agent-block.md` (skopos block version bump —
  new MCP surface).

## Implementation phases

1. **Domain:** schema, `internal/inbox` (models/storage/service/handler +
   `render.go` goldmark+bluemonday with adversarial tests; go.mod deps) +
   unit + scope tests; serve wiring; events type; cleanup row; openapi.
2. **CLI:** `cmd/inbox.go` + httptest-based tests (existing pattern).
3. **MCP:** `inbox_*` tools + tests, `skopos_context` section, instructions.
4. **UI:** web deps (CodeMirror 6) + editor wrapper; main.ts state +
   base.html view (rendered content, editor-backed create/edit modals, tag
   chips) + SSE handling.
5. **Coupling + polish:** plans completion hook → `CompleteForPlan`,
   integration test (plan completes → item done + event), docs, changelog.

## Test plan

- `internal/inbox` render: adversarial vectors — `<script>` in raw HTML
  blocks/inline, `onerror` attributes, `[x](javascript:...)`,
  `[x](data:text/html,...)`, autolinked URLs; assert escaped/stripped in
  `content_html` and that benign markdown (headings, lists, links, code,
  tables) survives.
- `internal/inbox` service: validation (title required, workspace required
  on writes, frozen-after-converted), tag normalization (case, charset,
  dedupe, caps, delimiter-safety), claim CAS (409), convert (missing plan,
  cross-workspace plan, double-convert 409), `CompleteForPlan` (only
  `converted` flips), cleanup predicate.
- Scope suite (`scope_test.go` pattern): operation × principal (root /
  all-workspaces / scoped) × workspace (member / non-member / missing).
- `cmd/routes` integration: lifecycle end-to-end incl. uniform 404s;
  `content_html` present on detail, absent on list.
- `cmd/mcp` e2e: create → claim → enrich → plan_create → convert; scoped
  key sees only its slice.
- CLI: lifecycle against httptest server; `--file`/stdin content input;
  repeatable `--tag`.
