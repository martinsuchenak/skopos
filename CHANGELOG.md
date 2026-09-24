# Changelog

All notable changes to this project are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/).

## [0.8.1] — 2026-09-24

### Fixed

- **Dashboard: kanban appeared empty after a redeploy** — two stacked
  causes. While the server restarted, the 5s fallback poll's failed
  fetches *blanked* the board (list rows were kept only by luck of
  timing); `fetchInbox` now keeps the current rows on transient failures
  (the header's connection pill already reports the outage). And after
  recovery, every lane kept a stale "—" empty-state next to its cards:
  the placeholder's `x-show` inside the lane `x-for` reads outer state,
  which the Alpine CSP build never re-evaluates — placeholders are now
  driven imperatively by `syncInboxLanes` (the `syncInboxExpansion`
  pattern) after every items change.
- **Dashboard: inbox card meta layout locked** — created + updated dates
  are one non-breaking, right-anchored group (they previously split
  across lines inconsistently depending on badge widths), and the
  context badges (workspace, claiming agent, plan) wrap as whole units.

## [0.8.0] — 2026-09-24

### Added

- **Inbox: address items by priority number (MCP)** — every
  item-addressing `inbox_*` tool except `inbox_update` (whose `priority`
  param already sets the rank) now accepts `workspace_id` + `priority` in
  place of `item_id`: "claim item #2 in <workspace>" is a single call. The
  number names the open/in-progress item currently holding it; frozen
  items with stale numbers never collide, `item_id` wins when both are
  passed, and a stale number returns not-found pointing at `inbox_list`.
  `inbox_list` advertises the convention, and `skopos_context`'s inbox
  rows now include each item's priority.
- **Dashboard: inbox cards show an "updated Xm ago" chip** next to the
  created age once an item has been touched (claim, convert, edit,
  reorder...). Untouched items show only their age; the bare age remains
  the created date. Interim recency signal ahead of the planned audit
  log, which will later add the qualitative "what happened" layer.

### Fixed

- **Dashboard: pinned/renumbered inbox rows went stale** — after Pin or a
  reorder, reused rows kept the old `#n` badge and Pin/Unpin label (the
  Alpine CSP build does not re-run row-level directives when the row key
  is unchanged). List rows and board cards now key on `item.priority`
  too, so rows re-create when renumbering changes them.
- **Dashboard: Pin/Unpin now visible in the newest/oldest sorts** — the
  buttons were hidden outside the priority lens; pinning is absolute (top)
  and unpin clears, so both are meaningful in any lens. Drag-ranking
  remains priority-lens only.

## [0.7.2] — 2026-09-22

### Fixed

- **Dashboard: mobile layout (WCAG 2.2 AA at phone widths)** — list rows
  in the Inbox and Plans now stack their action buttons below the title
  on narrow screens (the old side cluster was `shrink-0` and overflowed);
  the inbox toolbar's search/tag/sort group wraps; the header drops the
  view title and shrinks the workspace picker below `sm` so nothing
  crowds; compact 24px buttons rise to 32px below `sm` (all regular
  buttons are already 36–44px). Verified at 390px and at the 320px WCAG
  reflow benchmark across every view: no page-level horizontal scrolling
  (the kanban board scrolls internally by design), tap targets ≥ 24px
  AA everywhere.

## [0.7.1] — 2026-09-22

### Fixed

- **Dashboard: stale cached assets after an upgrade** — the dashboard now
  references its scripts and styles through content-versioned URLs
  (`/static/app.js?v=<hash-of-bundle>`, recomputed at startup), so a
  browser holding a cached bundle from an older binary can never run
  stale JavaScript against new markup — the "it works after a hard
  reload" failure mode. The HTML itself is `no-cache`; versioned assets
  are `immutable`.

## [0.7.0] — 2026-09-22

### Added

- **Inbox: manual complete** — items that finished without a plan (or
  ahead of it) can now be marked done manually: drag the card to the
  Done lane, the Done button in the list view,
  `POST /api/inbox/{id}/complete`, `skopos inbox complete --id`, or the
  `inbox_complete` MCP tool. Allowed from open/in_progress/converted
  (completing a converted item early is safe — the later plan-completion
  flip no-ops); done stays terminal and discarded items must be restored
  first.
- **Inbox: bulk purge** — `DELETE /api/inbox?workspace_id=…` permanently
  deletes items in one workspace, every status or one via `&status=`
  (the answer to "how do I wipe the accumulated pile"): `skopos inbox
  purge --workspace … [--status done]`, and Clear buttons on the board
  lanes. Workspace required for every principal; unfiled items are never
  matched; response reports the count. No MCP tool (operator concern,
  like the other purges).

- **Inbox: reopen** — a wrong manual complete can be undone: done → open
  via drag-back-to-Open, the Reopen button, `POST /api/inbox/{id}/reopen`,
  `skopos inbox reopen --id`, or `inbox_reopen`. The claim, plan link,
  and priority are cleared — a fresh cycle.
- **Inbox: UX round** — the unfiled item's "file into workspace" control
  is now a full-width button opening a searchable picker modal (the old
  inline select was crammed into the card's metadata row and could not
  search); the control only appears on editable (open/in-progress)
  items. The modal follows the ARIA combobox pattern: focus stays in the
  search input, arrow keys move the active option, Enter picks, Escape
  closes, and Tab cycles within the dialog (options expose
  `aria-selected`/`aria-activedescendant` for screen readers). Error
  messages now use human terms ("item is in progress") while messages
  listing valid input values keep the API slugs.

## [0.6.0] — 2026-09-21

### Added

- **Bulk deletes** — two operator maintenance endpoints, both reporting
  the count (`{"deleted": N}`), with CLI and dashboard support and no MCP
  surface (keeping the toolset lean, as with audit reads):
  - `DELETE /api/sessions` purges sessions — one workspace via
    `?workspace_id=` (any key scoped to it) or every session on the
    server (root key only). The usual cascades apply: events, agent
    states, and session-scoped blackboard entries go with them; both
    the `sessions` and `blackboard` refresh events fire. CLI:
    `skopos sessions purge [--workspace …]`. Dashboard: "Delete all…"
    in the sessions header (hidden for non-root keys in the
    All-workspaces view).
  - `DELETE /api/blackboard/entries?workspace_id=…&entry_type=…` purges
    every entry of one type in one workspace, across all scopes and
    branches (floating bug/debt included). Both parameters are required
    for every principal. CLI: `skopos blackboard purge --type …
    --workspace …`. Dashboard: a per-type "Delete all" button in the
    blackboard view (shown when a workspace is selected).

## [0.5.3] — 2026-09-21

### Added

- **Dashboard: inbox sort control** — the inbox toolbar gained a sort
  select: priority (server rank order, the default), newest first, or
  oldest first, applied as a display lens over both the list and the
  kanban lanes and persisted across reloads. Date sorts parse the
  timestamps (`RFC3339Nano` strings do not compare lexicographically).
  Rank operations are disabled in date-sorted views — same-lane drop
  markers, Pin/Unpin, and drag-ranking require the rank-ordered view;
  cross-lane drag transitions keep working everywhere.

## [0.5.2] — 2026-09-21

### Fixed

- Kanban workspace badges still clipped longer org names after the
  host-strip change (`martinsuchenak/s…`): the badge cap is widened to
  fit real org/repo pairs; anything longer truncates with the full id
  on hover.

## [0.5.1] — 2026-09-21

### Fixed

- Workspace labels truncated the distinguishing part: badges and the
  header picker cut off from the right, leaving only the shared host
  (`github.com/fortix…`). Labels now display host-stripped
  (`fortix/freedom3`) across all views — kanban and list badges, the
  header picker, write-modal selects, the Sessions view, and index
  groups. Display-only: full ids remain the values everywhere, on
  hover (title attributes), and searchable in the picker; registry
  names still take precedence when set.

## [0.5.0] — 2026-09-21

### Added

- **Dashboard: workspace picker in the header** — the workspace filter
  moved from the sidebar footer to the sticky header next to the view
  title. The trigger is wider (full repo paths truncate with a tooltip)
  and opens a searchable panel: filter by name or id, arrow-key
  navigation, Enter to pick, Escape or click-away to close, checkmark on
  the current selection. The selection persists across reloads
  (localStorage) and is dropped automatically when the key can no longer
  see that workspace.
- **Inbox board: workspace badges and drop indicators** — kanban cards
  show a workspace badge in the All-workspaces view; same-lane drags
  show where the item will land (inset edge on the top or bottom half of
  the hovered card), cross-lane hovers get a dashed outline, and the
  dragged card dims.
- **Inbox: search and tag filter at the top of the page** — a debounced
  search box (title and content) and a tag select in the toolbar, active
  in both list and board layouts. Per-item tag chips keep working.

### Fixed

- Inbox drag-reorder in the All-workspaces board failed with 400
  ("reorder spans multiple workspaces"): reorder ids were computed from
  a cross-workspace fetch and included converted items with stale
  frozen priorities. Ids are now scoped to the dragged item's workspace
  and to open/in_progress items.
- The dragged kanban card never dimmed: a row-level `:class` bound to
  outer-scope state never re-evaluates in the Alpine CSP build (sixth
  documented silent mode, alongside `x-show` on newly added elements);
  both are now driven imperatively from TypeScript.

## [0.4.0] — 2026-09-20

### Added

- **Inbox** — a workspace-scoped capture queue for unprocessed work
  (docs/design/inbox.md): rough ideas in markdown that an agent claims,
  enriches, and converts into a plan; completing the plan completes the
  item automatically (plans completion hook, registrar pattern).
  - Lifecycle: `open` -> `in_progress` (CAS claim, 409 on conflict) ->
    `converted` (links an existing same-workspace plan) -> `done` (auto),
    plus `discarded`; content/tags frozen after `in_progress`.
  - Tags: normalized server-side (lowercase, `[a-z0-9._/-]`, <= 10 x 40
    chars), stored as a JSON array, filtered with a delimiter-safe LIKE.
  - REST: `POST/GET /api/inbox`, `GET/PATCH/DELETE /api/inbox/{id}`,
    `POST /api/inbox/{id}/claim|convert|discard`. Detail reads return
    `content_html` — markdown rendered server-side with goldmark (default
    config, raw source HTML escaped) and sanitized with bluemonday's UGC
    policy; adversarial tests pin the sanitization. Lists carry
    plain-text excerpts only.
  - MCP: `inbox_create/list/read/update/claim/convert/discard` (behind
    `tool_search` in lean mode) + an `inbox` section in `skopos_context`.
  - CLI: `skopos inbox add|list|show|update|claim|convert|discard|delete`
    with `--content`/`--file`/stdin (notes-vault migration) and repeatable
    `--tag`.
  - Dashboard: Inbox view with status chips, tag filter chips, rendered
    markdown, and a CodeMirror 6 markdown editor in the create/edit
    modals (CSP-compatible): markdown syntax highlighting themed via CSS
    variables, list continuation on Enter, active-line highlight,
    placeholder, Cmd/Ctrl+Enter to save. Unsaved edits are guarded —
    backdrop/Escape/Cancel ask for confirmation before discarding (an
    untouched modal still closes immediately). SSE type `inbox`.
    Rendering is injected imperatively: the Alpine CSP build PROHIBITS
    the html directive (a template guard test pins this).
  - Cleanup: `discarded`/`done` items deleted past the retention window.
  - **Partial priority ordering**: items may carry a priority (>= 1,
    smaller first); unprioritized items always sort after prioritized
    ones. `priority` on create/PATCH (0 clears), `POST /api/inbox/reorder`
    renumbers an ordered prefix atomically, `--priority N|clear` in the
    CLI, `priority` on the MCP create/update tools (-1 clears).
  - **Board layout (swimlanes)**: list/board toggle (persisted per
    browser); one lane per status with drag & drop — open→in_progress
    claims, in_progress→open releases, →discarded discards; drag onto a
    card to rank above it (prefix renumbers), drop at the lane end to
    unpin; cross-lane moves compact ranks. Converted/Done lanes are
    plan-driven ("auto"). List view gains Pin/Unpin and `#rank` badges.
  - **Unfiled captures**: creates may omit `workspace_id` (root only) —
    the item is unfiled: visible to root alone until filed via PATCH
    `workspace_id` / `skopos inbox file` / the dashboard's picker and
    "file into…" action. Scoped keys must name a workspace. The capture
    modal now shows an explicit workspace picker with inline validation
    (previously a missing selection failed with only a toast).
  - Fixed a pre-existing dashboard bug: the Alpine CSP build silently fails
    to bind two-statement `@change` expressions, so the plans item-status
    and add-dependency dropdowns never fired — all such handlers are now
    single-statement method calls (element passed in, method resets it).
  - Code-review round fixes: (1) list/board item ordering now sorts by the
    fixed-width UUIDv7 id — RFC3339Nano TEXT timestamps do not sort
    lexicographically (truncated fraction zeros), which made the
    priority/newest-first order flaky; (2) convert no longer leaks foreign
    plans (out-of-scope plan = uniform 404, mismatch detail only for
    in-scope plans) and unfiled items are rejected up-front; (3) MCP
    `toolError` classifies the inbox sentinels (unknown id, claim conflict,
    double-convert, frozen) as invalid-params instead of internal errors;
    (4) the plans completion hook fires only on the actual transition to
    completed; (5) reorder is per-board (one workspace per call);
    (6) `inbox_list` over MCP lets the root key omit workspace_id (unfiled
    items become discoverable); (7) dashboard fixes: item/plan detail
    expansion is driven imperatively (CSP x-show effects inside x-for rows
    do not re-run on outer-state changes — plan expansion was dead in
    production since the CSP migration), the modal X routes through the
    unsaved-edits guard, conversion plans are created in the item's
    workspace (no stray plans), reorder/pin/compaction compute ids from
    the unfiltered server list (tag/status filters no longer corrupt
    ranks), and the sidebar open-count badge only shows when the visible
    filters make it truthful.
  - **Restore from discarded**: `discarded` is no longer terminal — drag
    the card back to Open, the Restore button, `POST /api/inbox/{id}/restore`,
    `skopos inbox restore`, or the `inbox_restore` MCP tool returns it to
    open (stale claim cleared; `done` stays terminal — it belongs to its
    plan).
  - Dashboard polish: the markdown editor's focus ring follows the rounded
    corners (box-shadow on the host, matching the other inputs); the
    capture/edit modal is wider (max-w-4xl) with a much taller editor for
    long notes; board cards no longer expand inline (long content is
    unreadable in a narrow lane — read/edit via Edit or the list view); the
    list view no longer shows a plain-text excerpt while collapsed; the
    light theme now covers the fractional panel surfaces (kanban lanes,
    index/keys rows, workspace picker lists) that previously stayed dark.
  - **WCAG 2.2 AA pass**: primary/danger button fills meet 4.5:1 in every
    state and theme (light theme had dark-on-cyan 3.3:1 via a specificity
    accident); light-theme secondary text 4.4 → 7.7:1; placeholders meet
    4.5:1 in both themes; interactive control boundaries (inputs, buttons)
    at 3:1; card borders nudged for perceivability; small controls raised
    to the 24×24 minimum (lane buttons, tag chips, sidebar New, branch
    filters); the tag-filter chip responds to Space as well as Enter.
  - Escape now closes the inbox capture/edit dialog directly (consistent
    with every other dialog) — previously it toggled the unsaved-edits
    confirm and the dialog never closed; the confirm still guards the
    accidental paths (backdrop, Cancel, X).
  - New deps (pure Go): `github.com/yuin/goldmark`,
    `github.com/microcosm-cc/bluemonday`.

## [0.3.0] — 2026-09-15

The benchmark-response release: compactness, ergonomics, and indexing
accuracy driven by a 224-run index-effectiveness series (llm-bench over
phantom overlays; report in the llm-bench repo).

### Added

- **`code_find`** — the anchorless flagship: semantic-first tool for
  role/description questions with no identifier ("which class renders
  errors as HTML?"). Semantic search when embeddings are configured, with
  an FTS fallback that mines identifier-shaped tokens from the prose.
  Compact top-N by design (name, kind, file:line, matched_by, one-line
  summary). Measured regime: indexes delivered perfect recall where grep
  guessed 1/3 on role-based questions.
- **MCP lean tools** (`[mcp] lean_tools`, default off): hides non-core
  tools from `tools/list` behind the MCP `tool_search`/`execute_tool`
  meta-tools — schema bytes are step-level weight (~20 KB re-sent every
  turn); lean lists 11 tools / 10.7 KB (−47%). Hidden tools stay fully
  callable.
- **`skopos install --profile index|full`**: index profile drops the
  coordination steering and disables session reporting/heartbeats via a
  `SKOPOS_PROFILE=index` hook gate — the ceremony measured at up to ~40%
  of treatment tokens with zero contribution to solving.
- PHP: bare `SomeClass::class` in argument position now emits a
  type-reference edge (previously only the receiver idiom was captured).
- **.gitignore-aware indexing**: git checkouts skip ignored files (a
  fully-built working tree of a 12k-file repo had >100k files on disk —
  8× index inflation). Native matcher (negation, anchoring, dir-only,
  `**`); `SKOPOS_NO_GITIGNORE=1` opts out; non-git directories unaffected.
- `skopos setup --global` writes the client config to `~/.config/skopos`.

### Changed

- **workspace_id is optional** on every MCP read tool when the key is
  scoped to exactly one workspace (defaults to it); ambiguous scopes fail
  closed. The `skopos_workspaces` discovery turn is no longer necessary.
- **Terse results by default**: `code_search`/`code_symbol` return
  name/kind/file:line/signature; doc comments, modifiers, and attributes
  require `detail=true`. Tool descriptions dieted (28 tools: 23.0 →
  20.3 KB); the routing decision tree lives only on `code_search`/`code_find`.
- `code_callers` gains a `kinds` filter (MCP/CLI/REST): `kinds=call` is
  true call sites, excluding the definition/type-reference edges that
  made compliant agents wrong under every provider tested.
- Agent block v5: three-line question-shape decision tree (named
  identifier → code_search/code_symbol; role/description → code_find;
  literal string → grep) replacing the prose that measured as
  quotable-and-ignorable.
- Hook readiness: remote mode is always ready — the session briefing
  prints in every remote-configured repo, indexed or not.
- Config search path: repo-local skopos-config.toml, then
  ~/.config/skopos/skopos-config.toml — every checkout inherits remote
  mode instead of silently reverting to local.

### Fixed

- `skopos index push` failed on large repos: the manifest hit the general
  1 MiB JSON cap (~10k files); now decodes under the commit-sized 64 MiB
  cap, and oversized bodies return 413 instead of a misleading 400.
- `skopos install --workflow remote` always updates the stored key (the
  preserve-if-absent rule kept revoked keys after rotation).
- MCP agent credentials baked per-agent into hook scripts (chmod 600,
  env-overridable) — hook CLI calls authenticate as the agent.

## [0.2.4] — 2026-09-14

### Fixed

- **`skopos index push` failed on large repos**: the manifest step decoded
  with the general 1 MiB JSON cap, but a manifest carries one entry per
  file (~100 bytes) — repos past ~10k files got "invalid request body" and
  could not push at all. The manifest now decodes under the commit-sized
  64 MiB cap (~500k files of headroom); blobs and commit were already
  capped appropriately. Oversized bodies now return 413 with guidance
  instead of masquerading as malformed JSON.
- `skopos install --workflow remote` always updates the stored key (last
  install wins): the previous "write only when absent" rule kept a revoked
  key in the global client config after re-installing with a rotated
  credential, 401-ing the CLI while the hooks kept working.

## [0.2.3] — 2026-09-14

### Added

- **Global client config fallback**: the client config is searched CWD
  first, then `~/.config/skopos/skopos-config.toml` (explicit XDG path on
  every platform) — previously it was repo-local only, so any checkout
  without one silently reverted the CLI and hooks to local mode even with
  a remote MCP server configured globally. In new projects the session
  hook stayed silent and the local-mode instructions steered agents into
  starting their own local skopos server instead of using the remote one.
  `skopos setup --global` writes to the global path.
- **`skopos install --workflow remote|local`** (per agent): remote writes
  the shared global client config (server_url always; api_key only when
  absent, so one agent's install never overwrites another's terminal
  default) and bakes this agent's API key into its hook scripts
  (skopos-common.sh, owner-only, env-overridable) — hook-driven CLI calls
  (heartbeats, index queries) authenticate as the agent. local writes
  nothing.
- Session hooks treat remote mode as always ready — the briefing now
  prints in every remote-configured repo, indexed or not.

### Fixed

- Dashboard Index view with "All workspaces" selected listed every
  workspace's indexes grouped (with a "not indexed" state) instead of
  querying the literal id "default" and showing nothing.

## [0.2.2] — 2026-09-14

Sessions that tell their story: automatic progress data from the hooks, and
a dashboard that reads well with it.

### Added

- **Automatic session heartbeats** (agent hooks, block v4): the
  SessionStart hook begins a skopos session, stores its id in the
  gitignored `.skopos-session`, reports the start, and tells the agent the
  id so `report_status` calls join one timeline (omitting session_id
  mints a new session per call — the source of 1-2 event slivers). The
  PreToolUse hook fires a throttled (5-minute) fire-and-forget heartbeat
  on every tool call, so working agents stay off the stuck list and the
  timeline fills in without agent discipline. Heartbeat events carry
  `metadata.source=hook` / `heartbeat=true`. Agent identity derives from
  the hook install directory + hostname. Instructions now ask for a
  report at least every ~10 minutes on long tasks.
- **Session timeline** (dashboard): chronological events with consecutive
  heartbeats collapsed into compact rows ("N auto heartbeats · alive 12m"),
  an AUTO badge on hook/server events, "silent for Xm" gap markers,
  per-event progress bars and step chips. Session cards show "active Xm
  ago" and "ran Xh Ym"; the detail header adds started-time, duration,
  and last activity.

### Fixed

- The zsh completion script was not installable: it lacked the
  `#compdef skopos` first line (compinit ignores fpath files without it)
  and registered completion for the binary's absolute invocation path
  instead of the command name. Both standard install forms now work.

## [0.2.1] — 2026-09-14

### Fixed

- The `blackboard`, `plan`, and `report` CLI command families ignored the
  `[client]` section of skopos-config.toml (no ConfigPath on their
  `--server-url`/`--api-key` flags), so with a configured client they
  dialed localhost instead of the server. `index` and `key` commands were
  unaffected. Found verifying the v0.2.0 deployment.

## [0.2.0] — 2026-09-14

The multi-tenant release: scoped API keys replace the single shared key, so
each agent, machine, or integration gets a credential limited to chosen
workspaces instead of access to everything (previously the only workaround
was deploying multiple server copies). Hardened by two further external
pentest rounds against the feature.

### Breaking

- **Writes require `workspace_id` for every principal, root included** —
  unscoped entries rendered into every workspace's knowledge bundle (the
  cross-workspace noise and poisoning-amplification channel). Legacy
  workspace-less rows have no read path at all (wipe or re-scope at
  upgrade; `DELETE FROM blackboard_entries WHERE workspace_id IS NULL`).
- **Out-of-scope by-id operations return 404, identical to nonexistent
  ids** (no cross-tenant existence or ownership oracle). Explicit-workspace
  operations return 403 listing the caller's accessible workspaces. MCP
  maps both to invalid-params.
- **Unscoped reads return exactly the caller's slice** of the data — never
  other tenants' rows.
- **Workspace management and server-side index refresh are root-only**:
  registering workspaces, setting `git_url`, dropping indexes, and
  minting/editing/revoking keys.
- **Session workspace bindings are immutable** — a status report can no
  longer re-scope an existing session (it authorizes against the session's
  actual workspace, never the client-declared field).

### Added — multi-key workspace scoping

- DB-backed scoped API keys (`sk_` + 256-bit, stored as SHA-256 hash +
  display prefix, never in plaintext): scope `*` or an explicit workspace
  list; soft revocation keeps the audit trail, hard delete removes old
  keys. `last_used_at` recorded at most once per 5 minutes per key.
- Full key lifecycle on every surface: REST (`POST/GET/PATCH
  /api/keys`, `DELETE` with `?hard=true`), CLI (`skopos key
  create|list|edit|revoke|delete`), and the dashboard's root-only Keys
  view (create with one-time secret + copy-to-clipboard, edit dialog,
  revoke, delete). `skopos key generate-root [--quiet]` mints a strong
  root credential offline with rotation guidance.
- `GET /api/whoami` resolves the caller's identity and accessible
  workspaces; the dashboard adapts its workspace picker and write dialogs
  to the signed-in key.
- Authentication resolves once per request to a principal (root key,
  DB key, or internal caller) carried in the request context; enforcement
  lives in the service layer, covering REST, MCP, and CLI uniformly.
- MCP `skopos_workspaces` tool lists the caller's accessible workspaces
  for agent self-discovery.
- Server-stamped provenance: the resolved key (id + name) is recorded in
  `report_status` event metadata — an audit counterpart to the
  client-asserted `author_agent_id`.
- SSE streams are scope-filtered per subscriber and **terminated
  immediately when their key is revoked**. Events are published by the
  mutating services with the authoritative workspace (no inference from
  requests); read-only MCP calls are silent.

### Security

Two further external pentest rounds (one gray-box against a live keyed
deployment, one against the deployed multi-key build); all findings
remediated with regression tests and live oracle replays:

- **Session takeover via declared-workspace authorization** (CVSS 6.3):
  reports authorized the client-declared workspace and could rebind and
  seize foreign sessions — closed by ownership resolution + immutable
  bindings (the Breaking entries above).
- **Cross-workspace session-events read** (`GET /api/sessions/{id}/events`
  skipped the scope check its siblings enforced) and **agent
  enumeration** via `skopos_context`'s agents section — both scoped.
- **Plan reference injection**: `plan_add/remove_plan_dependency` now
  scope-check the referenced plan, not just the parent.
- **SSE tenant isolation was inoperative** (attribution ran after bodies
  were drained; path values invisible through the middleware chain;
  fail-open delivery): rewritten as service-layer publishing with
  fail-closed delivery.
- `skopos setup` client config is `0600` on every write, not only
  creation.

### Fixed

- All-workspaces keys crashed the dashboard key list (`workspaces: null`
  from the API killed the render); the API now emits `[]` and the UI
  tolerates null.
- The 401 key prompt no longer pre-fills the just-rejected key (pasting
  over it without selecting-all appended and produced another 401).

### Docs

- New `docs/concepts/api-keys.md`; README, getting-started,
- workspaces concept, configuration reference, and all seven agent
  integration guides updated for root/scoped keys; `openapi.yaml` covers
  the new endpoints and the 403/404 scope semantics (guard test in
  lockstep).

## [0.1.2] — 2026-09-14

Security release: second external penetration-test round (gray-box against
a live keyed deployment), all seven confirmed findings remediated with
regression tests and attack-oracle replays.

### Breaking

- **MCP read tools now require `workspace_id`** — `blackboard_read` and
  `skopos_context` fail closed with invalid-params when the scope is
  omitted, instead of silently returning every workspace's entries, plans,
  and sessions. REST and the dashboard keep their explicit
  "All workspaces" filter. Agents should derive the id via
  `skopos workspace` or the git remote (the v3 instruction block and
  session hook already say so).

### Security

- **SSRF & internal repository indexing via `git_url`** (CWE-918):
  server-side code-index refresh now accepts `http(s)` URLs to public
  hosts only — `git://`, `ssh://`, and scp-like transports are rejected,
  as are IP literals in loopback/private/link-local/unspecified ranges,
  `localhost`, embedded credentials, and leading-dash values (git option
  parsing); `git clone` arguments are terminated with `--`. Residual,
  documented: hostnames resolving to internal IPs and redirects to
  internal targets require a deployment-level egress allowlist.
- **Agent-context poisoning** (CWE-74): blackboard entry text rendered
  into the knowledge bundle (and plan-item titles in `skopos_context`)
  is now flattened to single-line, metacharacter-escaped, and
  length-capped, and the bundle opens with a provenance banner telling
  consuming agents that entries are data — instructions inside entries
  must never be followed. Forged "## System Instructions" headings no
  longer round-trip structurally; entry data itself is preserved.
- **Plan state-machine bypass on updates** (CWE-1050): item updates now
  re-validate inside the transaction — no marking items done while
  dependencies are unfinished, no reopening done items, and items of
  completed/archived plans are frozen (the create and dependency-add
  paths already enforced these invariants).

### Fixed

- **Silent write loss under concurrency** (CWE-1291): the SQLite DSN now
  sets `_txlock=immediate` — DEFERRED read-then-write transactions
  deadlocked with SQLITE_BUSY under contention and dropped writes (the
  live test lost 5 of 6 concurrent item adds). Pinned by a dedicated DSN
  test plus barrier-based regression tests.
- **Plan-item claim race** (CWE-362): claiming is now a compare-and-swap
  — exactly one concurrent claimant wins; losers receive HTTP 409 /
  MCP invalid-params instead of a success whose ownership was silently
  overwritten. Re-claim by the owner and release still work.
- **Blackboard promote lost-write** (CWE-367): the scope UPDATE now runs
  inside the deciding transaction and commits (it previously executed on
  the pool outside the transaction and was never committed, so
  concurrent promotes downgraded entries every caller was told had
  promoted).

### Hardening

- `/static/` no longer serves directory listings; the dashboard CSP adds
  `object-src 'none'`.

## [0.1.1] — 2026-09-13

Post-deploy hardening follow-up to 0.1.0.

### Added

- `skopos serve` refuses to start without embedded frontend assets
  (`web.VerifyAssets`): a binary built without `task frontend-build`
  previously served a dead dashboard — every asset 404s, with no error
  anywhere. Release tests now gate the bundle (goreleaser builds the
  frontend before `go test`; unbuilt local checkouts skip).
- Agent instructions (AGENTS block v3) direct agents to pass
  `workspace_id` to `skopos_context` — unscoped reads span every
  workspace on the server — and the session hook now prints the
  repo's workspace ID in remote mode.

### Fixed

- Dashboard silently rendered empty lists when the browser had no
  (or a wrong) API key stored: reads swallowed 401s by design, with
  no prompt anywhere. Any 401 now opens the key modal with a toast
  (latched per key change so polling/SSE retries don't spam), and
  saving the key refreshes immediately instead of waiting for a
  manual Refresh.
- Session-derived workspaces are now auto-registered server-side on
  every accepted status report (`status.Service` workspace
  registrar, wired like the code-index push path). The documented
  behavior previously existed only in the dashboard's JS heuristic —
  workspaces used purely via MCP never persisted to the registry.
- The dashboard Index view no longer falls back to the literal
  workspace id `default` (matches nothing); it uses the first
  registered workspace.
- `web/dist/.gitkeep` is actually tracked now (the `.gitignore`
  comment claimed it was, but the root `dist/` pattern excluded the
  parent directory and blocked the negation) — fresh clones could
  not compile `//go:embed all:dist` at all. The web build script
  recreates the placeholder, since vite empties `dist/` on build.

## [0.1.0] — 2026-09-13

The code-index release: skopos grows from agent coordination into a central,
branch-aware code intelligence server. First pre-1.0 **breaking** release.

### Breaking

- **`skopos init` is removed** — `skopos setup` covers all workflows
  (local index, remote client, run-server).
- **A configured API key now gates every endpoint** — reads, writes, MCP,
  SSE, and metrics. Previously read endpoints were open. Auth-less servers
  remain fully open but bind to loopback only.
- **Default bind is `127.0.0.1`** (was `0.0.0.0`). Exposing the server
  requires `--server-host 0.0.0.0` plus an API key, or the explicit
  `--insecure-no-api-key`.
- **Server-side refresh restricts `git_url`**: `file://`, absolute paths,
  `~`-prefixed, and `..`-traversing values are rejected (SSRF and
  arbitrary-local-repository read were possible before).
- Symbol search queries are capped at 256 bytes (FTS CPU abuse surface).

### Added — code index

- Central, per-workspace, per-branch symbol index over 206 embedded
  tree-sitter grammars; 15 language profiles with per-language extraction.
- Symbol cards carry **doc comments** (PHPDoc, JSDoc, Godoc, docstrings,
  Javadoc, C# XML — summary + `@throws`/`@deprecated`/`@see`; declaration
  data always wins over stale doc tags), **modifiers** (visibility, static,
  abstract, …), and **attributes** (PHP 8 `#[…]`, decorators, annotations).
- Call graph with labeled edges: calls, `new` instantiations, type
  **references** (params/returns/properties/`instanceof`/`catch`),
  inheritance (**extends/implements**, trait use, Go embedding, Ruby
  include, Rust `impl Trait for Type`), and file **imports**.
- Fields, properties, constants, and enum members are symbols.
- Qualified names (`Class::method`); `impact` reaches subclasses and lists
  **overrides**; `dead-code` respects usage kinds.
- Indexing modes: local build, incremental **push** to a central server
  (content-addressed, manifest-negotiated), and **server-side refresh**
  (clone/pull by the server, optional `--refresh-interval` polling).
- Optional **semantic search**: any OpenAI-compatible embeddings endpoint
  (local Ollama works), RRF fusion with `matched_by` provenance
  (`fts`/`vector`/`both`), vector storage embedded in SQLite or external
  **Qdrant**; embeddings are versioned per model+policy and rebuilt
  automatically on change.
- Query surfaces: CLI (`search`, `symbol`, `outline`, `who-calls`,
  `call-tree`, `impact`, `dead-code`, `cycles`, `branch-diff`, `deps` — all
  `--json`), 26 MCP tools, REST under `/api/codeindex/…` (OpenAPI spec
  kept in lockstep by a guard test).
- Path-prefix scoping (`--path` / `path` param) on search, who-calls,
  callees, dead-code, and dependencies; visibility/attributes searchable
  as text; `bm25` column weights so names outrank doc mentions.
- Embedding coverage and last-pass error in `index status`; daily index
  GC of unreferenced blobs/vectors; `skopos index cache-clean`.

### Added — integrations & UX

- `skopos install` for **seven agents** (Claude Code, Codex, Gemini CLI,
  GitHub Copilot, Kiro, opencode, ZCode) with each agent's documented
  config schema, a managed behavioral block in its instructions file, and
  hook suites for Claude Code and ZCode (session briefing, prompt-time
  code pre-fetch, search nudges, memory reminders — mode-aware via
  `skopos mode`).
- `/skopos` exploration skill; agent-builder artifact set.
- `skopos setup` wizard; real shell completion (bash/zsh/fish/PowerShell);
  clean terminal errors; dashboard **Index** view; docs overhaul
  (local/remote guides, configuration reference, per-agent integrations).

### Fixed

- Warm parse cache uploaded **empty indexes** to fresh servers after
  `drop-workspace` or a server switch (major data loss).
- Background embedding failures were silent; passes now log and surface
  in status.
- Semantic ranking pooled RRF credit across duplicate definitions (CSS
  selectors outranked true matches).
- Embedding text now includes split identifier tokens and doc summaries
  (camelCase names are opaque to embedding models).
- MCP `code_search` ignored its `semantic` parameter.
- Per-language profile definitions leaked across languages via a shared
  map alias.
- Rune-unsafe truncation could store invalid UTF-8.

### Security

- Two external penetration-test rounds remediated: index-directory
  traversal via crafted workspace/git URLs, SSRF and local-repo read via
  server-side clone, FTS5 CPU denial-of-service, file-descriptor
  exhaustion (index handles now LRU-bounded), LIKE-wildcard injection in
  blackboard search. Constant-time key comparison, browser-origin
  protection on `/mcp`, Host-header validation for loopback.

## [0.0.11] — 2026-09-09

Dependency refresh, auth improvements, documentation pass.

## [0.0.10] and earlier

See `git log v0.0.9..v0.0.11` — agent status, blackboard, plans,
workspaces, SSE dashboard, MCP server, Docker/Nomad deployment.
