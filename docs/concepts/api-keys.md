# API keys

skopos supports three kinds of credential:

- **Root key** — the server's configured key (`SKOPOS_API_KEY` /
  `[server] api_key`). Full access to every workspace, plus exclusive
  rights to manage API keys, the workspace registry (including `git_url`),
  and server-side index refresh. A server without any key configured runs
  unauthenticated on loopback only (development mode).
- **Scoped API keys** — minted via the API, CLI, or dashboard; each key is
  scoped to `*` (all workspaces) or an explicit list of workspace ids.
  Every read and write is checked against the key's scope.
- **Run tokens** — minted by the server for one pipeline run, for that
  run's agent. Nobody creates or manages them; see [Run tokens](#run-tokens).

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

**Presets** name common sets. `agent` is what an agent gets: `mcp`,
`code:read`, `blackboard:write`, `plans:write`, `status:read` and
`inbox:read`. It has no REST at all, so an agent can't move items, touch
runs or manage keys. A [run token](#run-tokens) carries it, and so does an
agent key for a worker on a server from before run tokens
(`SKOPOS_AGENT_KEY`, see [the worker reference](../deployment/worker.md#skopos-tools-for-agent-runs)).

```sh
skopos key create --name knot-1-agent --workspace github.com/me/app --preset agent
skopos key create --name ci --workspace github.com/me/app --capability rest,code:write
skopos key edit <id> --capability rest,runs:write   # replace the set
skopos key edit <id> --full-access                  # remove every limit
skopos key whoami                                   # shows the key's capabilities
```

The dashboard's key list shows a capped key's capabilities, and its key
dialog offers full access or the agent preset.

## Permissions

Four flags sit beside scope and capabilities. Only the root key grants
them, each on `key create` and `key edit` (`--approver` / `--no-approver`
and so on). Key listings and the dashboard's Keys view show them.

| Flag | Lets the key | Give it to |
|---|---|---|
| `--approver` | take the pipeline's human actions: queue, approve, request changes, reject, mark done | a person, or the relay acting for one |
| `--delegate` | name the person it acts for (`X-Skopos-On-Behalf-Of`) | an integration recording people's decisions |
| `--admin` | change policy and configuration within its scope | whoever runs the pipeline's configuration |
| `--worker` | receive the credentials the server holds for runs (a workspace's MCP servers), with the runs it claims | each pipeline worker, and nothing else |

**Admin** is scoped like everything else:

- a key with one workspace in scope administers that workspace;
- a group's settings need a key that reaches every workspace in the group;
- a group that takes in workspaces by pattern needs a key that reaches all
  workspaces: what is set on it also binds the ones that register later;
- fleet-wide settings need a key that reaches all workspaces (`--all-workspaces`).

The root key passes every level. Agent and worker keys must never hold
approver or admin: a worker refuses to start with a key that holds admin,
and refuses an agent key that holds either.

**Worker** marks a pipeline worker's key. Every key with a workspace in
scope can call the claim endpoint, so the server gives the credentials of
a workspace's MCP servers only to a key with this flag. The server refuses
`--worker` together with `--approver` or `--admin`. A worker with an
unmarked key still runs; it gets no runs of a workspace whose MCP servers
the server holds, and the Workers view says why.

```sh
skopos key create --name ops --group acme --admin
skopos key edit <id> --no-admin
```

## Run tokens

When a worker claims a pipeline run, the server mints a token for that
attempt and returns it in the claim answer (`agent_token`, `skr_…`). The
worker gives it to the run's agent as its skopos MCP credential. Compared
with one agent key shared by every run on a worker:

- **Scope:** only the run's own workspace, whatever else the worker's key
  reaches.
- **Lifetime:** valid only while the attempt is claimed or running with a
  live lease. A report, a pause, a requeue, a cancel or an expired lease
  ends it at once. A resumed or re-claimed run gets a new one.
- **Reach:** MCP only, and only the tools a pipeline stage uses: the
  blackboard, `skopos_context`, `plan_read` and the `code_*` tools. An
  implement run may also mark the steps of its own item's plan
  (`plan_update_item`), never another plan's.
- **Identity:** what it writes is recorded as
  `run <id> (<runner> on <worker>)`, not as a shared key.
- **Storage:** only a hash is kept (`run_tokens`).

`GET /api/whoami` with a run token names the run. On a server without
authentication, where every other caller is root, a run token is still a
run.

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
life. Creating, editing, revoking and deleting a key are audit events
(`key.create`, `key.update`, `key.revoke`, `key.delete`): who did it, and
what the key may do afterwards. So are changes to a workspace group
(`group.create`, `group.update`, `group.delete`), because a group's members
and patterns are part of the scope of every key that holds it. The secret
is never recorded. Hard-deleting an *active* key is refused unless `--force` is passed,
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
