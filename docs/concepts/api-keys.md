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

## Managing keys

```sh
# with the root key configured as the client key:
skopos key create --name zcode-laptop --workspace github.com/me/repo   # repeatable
skopos key create --name ci --all-workspaces
skopos key list
skopos key revoke <id>
skopos whoami
```

REST: `POST/GET /api/keys`, `DELETE /api/keys/{id}`, `GET /api/whoami`.
Dashboard: the **Keys** view (root key only) mints and revokes keys; the
workspace filter adapts to the signed-in key's scope.

The plaintext secret (`sk_…`, 256 bits) is shown exactly once at creation;
only its SHA-256 hash and a display prefix are stored. Revocation is soft —
rows remain for audit. Keys are recognized immediately after creation and
cut off immediately on revocation.

## Rolling out

1. Deploy with the root key configured (behavior identical to a single-key
   server until other keys exist).
2. Mint one key per agent/machine, scoped to the workspaces it may touch
   (`skopos install --api-key <scoped-key>` wires agents).
3. Rotate the root key last if desired — the root key is only needed for
   management operations.
