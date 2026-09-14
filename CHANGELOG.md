# Changelog

All notable changes to this project are documented here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/).

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
