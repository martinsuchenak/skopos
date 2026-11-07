# API keys

skopos supports two kinds of credential:

- **Root key** — the server's configured key (`SKOPOS_API_KEY` /
  `[server] api_key`). Full access to every workspace, plus exclusive
  rights to manage API keys, the workspace registry (including `git_url`),
  and server-side index refresh. A server without any key configured runs
  unauthenticated on loopback only (development mode).
- **Scoped API keys** — minted via the API, CLI, or dashboard; each key is
  scoped to `*` (all workspaces) or an explicit list of workspace ids.
  Every read and write is checked against the key's scope.

## Semantics

- **Writes require `workspace_id`** — for every principal, root included.
  An unscoped entry would render into every workspace's knowledge bundle;
  there is deliberately no fallback for workspace-less data. Entries with
  no workspace (legacy rows) are unreadable by scoped keys; root sees them
  only via explicit cleanup tooling.
- **Out-of-scope access** returns:
  - `403` with the caller's accessible workspaces when the workspace is an
    explicit parameter (filters, writes, path parameters) — actionable for
    agents;
  - `404` for by-id operations (plan read/update, entry delete/promote,
    session delete) — no cross-tenant existence oracle. MCP maps both to
    `invalid-params`.
- **Unscoped reads** (no workspace filter) return exactly the key's slice
  of the data — never other tenants' rows.
- **Workspace and key management is root-only**: registering workspaces,
  setting `git_url`, dropping indexes, refreshing server-side, and
  minting/revoking keys.
- **Provenance**: the resolved key (id + name) is stamped server-side into
    `report_status` event metadata — an audit counterpart to the
    client-asserted `author_agent_id`.
- **SSE** change notifications are filtered per subscriber by scope (events
  carry the mutation's workspace when derivable; they never carry data).

## Capabilities

A scoped key can also be limited to **capabilities**: what it may use, on
top of which workspaces. A key without capabilities (every key minted
before them, and the root key) keeps full access.

- **Surfaces:** `rest` (every `/api` route, the dashboard's event stream,
  metrics) and `mcp` (`/mcp`).
- **Areas**, each `<area>:read` or `<area>:write` (write implies read):
  `code`, `blackboard`, `plans`, `inbox`, `status`, `runs`, `workspaces`,
  `events`.

How they apply:

- **REST:** the route names the area (`/api/codeindex` is `code`,
  `/api/runs` and `/api/workers` are `runs`, `/api/sessions` and
  `/api/reports` are `status`, and so on). GET needs read, anything else
  write. A capped key gets `403` outside its capabilities. Key and group
  management, metrics and any route not mapped to an area are refused to
  capped keys; `/api/whoami` always answers.
- **MCP:** every tool needs an area. `tools/list` shows a capped key only
  the tools it may call, and a call to any other tool is refused, through
  `tools/call` and lean mode's `execute_tool` alike.
- Workspace scope and the approver permission still apply to a capped key.

**Presets** name common sets. `agent` is what workers hand their agent runs
(`SKOPOS_AGENT_KEY`, see [the worker reference](../deployment/worker.md#skopos-tools-for-agent-runs)):
`mcp`, `code:read`, `blackboard:write`, `plans:write`, `status:read` and
`inbox:read`. It has no REST at all, so an agent can't move items, touch
runs or manage keys.

```sh
skopos key create --name knot-1-agent --workspace github.com/me/app --preset agent
skopos key create --name ci --workspace github.com/me/app --capability rest,code:write
skopos key edit <id> --capability rest,runs:write   # replace the set
skopos key edit <id> --full-access                  # remove every limit
skopos key whoami                                   # shows the key's capabilities
```

The dashboard's key list shows a capped key's capabilities, and its key
dialog offers full access or the agent preset.

## Managing keys

```sh
# with the root key configured as the client key:
skopos key create --name zcode-laptop --workspace github.com/me/repo   # repeatable
skopos key create --name ci --all-workspaces
skopos key list
skopos key edit <id> --name new-name                                  # partial: only given fields change
skopos key edit <id> --workspace github.com/me/other                  # replaces the scope list
skopos key revoke <id>                                                # soft: keeps the audit trail, cuts access
skopos key delete <id>                                                # hard: removes an old (revoked) key
skopos whoami
# offline helper for the server's root credential:
skopos key generate-root [--quiet]
```

Editing takes effect on the key's next request (lookups are not cached);
revoked keys are frozen — mint a new one instead of editing one back to
life. Hard-deleting an *active* key is refused unless `--force` is passed,
because revoking first terminates the key's open SSE streams.

REST: `POST/GET /api/keys`, `PATCH /api/keys/{id}`,
`DELETE /api/keys/{id}` (revoke) and `DELETE /api/keys/{id}?hard=true`
(hard delete), `GET /api/whoami`. Dashboard: the **Keys** view (root key
only) mints, edits, revokes, and deletes keys, with a copy-to-clipboard
button on the one-time secret; the workspace filter adapts to the
signed-in key's scope.

The plaintext secret (`sk_…`, 256 bits) is shown exactly once at creation;
only its SHA-256 hash and a display prefix are stored. Revocation is soft —
rows remain for audit. Keys are recognized immediately after creation and
cut off immediately on revocation.

## Rolling out

1. Generate a root key (`skopos key generate-root`) and configure it on the
   server (`[auth] api_key` in skopos-config.toml, or `SKOPOS_API_KEY`).
   Behavior is identical to a single-key server until other keys exist.
2. Mint one key per agent/machine, scoped to the workspaces it may touch
   (`skopos install --api-key <scoped-key>` wires agents).
3. Rotate the root key last if desired — the root key is only needed for
   management operations.
