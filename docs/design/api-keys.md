# Design: Multi-Key Workspace Scoping

Status: approved design (2026-09-14) — implementation pending.
Motivation: a single shared API key grants every AI agent access to every
workspace; the only workaround today is deploying multiple server copies.
Also the foundation for the pentest-recommended "per-agent credentials with
server-attested authorship".

## Decisions (locked)

1. **Root key** — the configured key (`SKOPOS_API_KEY` / `[server] api_key`)
   keeps full access plus exclusive key/workspace management. Bootstrap: a
   fresh server with no DB keys behaves identically to v0.1.2.
2. **DB-backed keys with workspace scope** — created via API/CLI/UI; scope is
   `*` (all workspaces) or an explicit list. Enforcement on every read/write.
3. **Enforcement lives in the service layer** — REST, MCP, and the remote CLI
   all funnel through the same services, so one check per operation covers
   every surface. Principals travel in the request context.
4. **Writes require `workspace_id` for every principal, including root.**
   Rationale: floating entries caused the original cross-workspace noise;
   unscoped entries render into every workspace's bundle (widest
   poisoning-amplification channel); under scoped keys a NULL-workspace entry
   is invisible to every scoped key anyway, so it shares nothing. **No
   read fallback for legacy NULL-workspace rows**: reads always constrain
   `workspace_id` to the principal's set, making such rows unreachable dead
   data — wipe or re-scope them at upgrade time (the last one on the
   production server was re-scoped 2026-09-14; a plain
   `DELETE FROM blackboard_entries WHERE workspace_id IS NULL` is the
   documented cleanup for other deployments). A future explicit
   `scope: global` (deliberate broadcast semantics) is the escape hatch if a
   real need appears — org-wide agent conventions belong in user-level
   instructions (e.g. `~/.zcode/AGENTS.md`), not the blackboard.
5. **Out-of-scope by-id operations → 404** (no cross-tenant existence
   oracle); explicit-workspace operations out of scope → **403 with the
   caller's accessible list** (actionable for agents). MCP maps both to
   invalid-params.
6. **Workspace management and server-side refresh are root-only** — the
   registry and `git_url` feed the server-side clone path (the SSRF surface)
   and define the scoping graph itself. Members can index-push their own
   workspaces.
7. **Server-stamped provenance** — the resolved key (id + name) is written
   into `report_status` event metadata by the server; `author_agent_id`
   remains client-asserted but now has an audit counterpart.

## Principal model

```go
// internal/auth
type Principal struct {
    Root          bool                 // configured root key, or auth disabled
    KeyID, Name   string
    AllWorkspaces bool                 // "*" scope
    Workspaces    map[string]struct{}  // explicit list
}
func (p *Principal) CanAccess(ws string) bool
```

Resolution (once per request, in middleware):
empty key + auth-disabled → root; constant-time match with configured root
key → root; otherwise SHA-256 the bearer token and look up `api_keys`
(hash lookup removes the timing channel; plaintext keys are never stored).
Revoked → 401. In-process cache keyed by hash, explicitly invalidated on
revoke (single-process server). Background callers (health, cleanup,
registrar, refresher) run with a system/root context via `ctxkeys`.

## Schema (additive, internal/db/schema.sql)

```sql
CREATE TABLE IF NOT EXISTS api_keys (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  key_hash       TEXT NOT NULL UNIQUE,  -- sha256 hex of the full key
  key_prefix     TEXT NOT NULL,         -- e.g. "sk_w3hg9f" for display
  all_workspaces INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  last_used_at   TEXT,                  -- throttled async update (>= 5 min)
  revoked_at     TEXT                   -- soft revoke; rows kept for audit
);
CREATE TABLE IF NOT EXISTS api_key_workspaces (
  api_key_id   TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  workspace_id TEXT NOT NULL,
  PRIMARY KEY (api_key_id, workspace_id)
);
```

Key format: `sk_` + 43 chars base64url (32 bytes). Shown exactly once at
creation. New domain package `internal/apikeys` (handler → service →
storage, mirroring the other domains); `auth` exposes a `KeyLookup`
interface so `apikeys → auth` has no cycle.

## Enforcement matrix

| Operation | Workspace source | Rule |
|---|---|---|
| blackboard write/read/search/promote/delete | body/query; by-id → load entry's ws | membership |
| plans create/add/update/read/list/delete | body/query; by-id → load plan's ws | membership |
| report_status; sessions read/delete | body/query; by-id → load session's ws | membership |
| workspaces list | — | filtered to scope (root: all) |
| workspaces create/update/git_url/delete | — | root only |
| key management (create/list/revoke) | — | root only |
| codeindex status/search/push | path param | membership |
| codeindex refresh, drop-workspace | path param | root only |
| SSE stream | event envelope | per-subscriber filtering by scope |
| whoami | — | any authenticated principal |

Writes without `workspace_id` → 400 for every principal.

## Surfaces

**REST additions:** `GET /api/whoami` → `{root, key: {id, name,
all_workspaces, workspaces[]}, workspaces: [accessible]}` (drives UI
adaptation and CLI debugging). `POST /api/keys` `{name, workspaces: [...] |
"*"}` → returns the full key once; `GET /api/keys` (no secrets, prefix
only); `DELETE /api/keys/{id}` (soft revoke). All root-only.

**CLI:** `skopos key create --name ci --workspace a [--workspace b] |
--all-workspaces` (prints key once), `skopos key list`, `skopos key revoke
<id|prefix>`, `skopos whoami`. Existing commands unchanged — scoped keys
simply receive 403/404 with guidance.

**MCP:** all tools inherit enforcement through the services. New
`skopos_workspaces` tool listing the caller's accessible workspaces so a
scoped agent can bootstrap itself.

**UI:** "Keys" sidebar view (visible when `whoami.root`): table of
name/prefix/scope/created/last-used/revoked, create modal with workspace
multi-select + all-workspaces toggle, revoke behind the existing confirm
pattern. Workspace dropdown and write-modals driven by `whoami.workspaces`;
write-modals require a workspace selection. 403 toasts surface the server's
guidance verbatim.

**SSE:** mutation events gain a workspace field; the hub filters per
subscriber by principal scope.

## Compatibility & migration

- Zero-downtime rollout: no DB keys → root-only → behavior = v0.1.2.
- Breaking (changelog "Breaking" section): writes without `workspace_id`
  → 400; out-of-scope by-id → 404 (was 200); `/api/workspaces` list is
  scope-filtered; legacy NULL-workspace entries have **no read path at all**
  (dead data) — wipe or re-scope them at upgrade time; once none remain, a
  follow-up migration may harden the column.
- Docs: `docs/configuration.md` (root key + key management), new
  `docs/concepts/api-keys.md`, agent-integration notes (scoped keys for
  installs), `openapi.yaml` (guard test keeps lockstep).

## Implementation phases

1. **Foundation (no user-visible change):** schema, `Principal`, resolver +
   cache, middleware → context refactor (`authorized(r)` call sites become
   principal retrieval), system context for background callers. Suites green.
2. **Key management:** `internal/apikeys` + REST + CLI + UI Keys view +
   `whoami`.
3. **Enforcement:** per-service authz matrix, writes-require-workspace,
   404/403 semantics, SSE filtering, `skopos_workspaces` MCP tool,
   server-stamped provenance in `report_status` metadata.
4. **Polish:** throttled async `last_used_at` (no per-request writes — see
   the 0.1.2 SQLite write-contention history), docs, openapi, changelog.

## Test plan

- `internal/auth`: resolver table tests (root / DB key / revoked / unknown /
  disabled mode) + cache invalidation.
- `internal/apikeys`: CRUD validation — name required, `*` XOR explicit
  list, workspace existence, revoke idempotence, hash uniqueness.
- **Authz matrix (core):** table-driven service tests per domain —
  operation × principal (root / all-workspaces / scoped) × workspace
  (member / non-member / missing-on-write).
- `cmd/routes` integration: two keys end-to-end (401/403/404 semantics,
  key lifecycle, SSE filtering, whoami).
- `cmd/mcp` e2e: scoped-key tool calls in and out of scope.
- CLI: key lifecycle against httptest server (existing pattern).
- Existing handler tests: constructor change (apiKey string →
  authenticator); mechanical. Concurrency suites from 0.1.2 keep guarding
  the write path.
