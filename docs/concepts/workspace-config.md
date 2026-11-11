# Workspace config on the server

The part of a workspace's configuration that every worker serving it shares
can live on the server: branches, setup and check commands, timeouts, push
settings and the MCP lists. Workers keep only what is true of their own
machine. Runner definitions live on the server too, so a runner name means
the same model on every worker.

Nothing changes until you save a config for a workspace: a workspace without
one keeps using each worker's file.

## What lives where

| On the server | In each worker's file |
|---|---|
| `default_branch`, `branch_prefix`, `branch_template`, `checkout`, `protected_branches` | `id`, `state_dir`, the server URL and the worker's key |
| `setup`, `checks`, `check_fix_rounds`, `preflight`, `preflight_interval` | per repo: the clone's `path`, `setup_env`, `push.token_env` |
| `setup_timeout`, `check_timeout`, `quiet_timeout`, `plan_timeout`, `impl_timeout` | accounts: name, kind, credentials, priority |
| `push.kind`, `push.remote_url`, `push.pull_request`, `push.steps` | `contained_by`, `server_commands`, `auto_upgrade` |
| `mcp_deny`, `mcp_servers` | stage profiles and `mcp_config` |
| runner definitions | |

The fields mean what they mean in a worker's file; see
[Configuration](../configuration.md#worker).

## Managed workspaces

A workspace is **managed** when it has a config of its own on the server.

```sh
skopos workspace config set --workspace github.com/you/repo \
  --set default_branch=main \
  --set 'setup=["npm ci"]' \
  --set 'checks=["npm run lint", "npm test"]' \
  --set push.kind=github \
  --set push.remote_url=https://github.com/you/repo.git \
  --set push.pull_request=draft
```

- **Every shared field then comes from the server.** A field the server
  does not set has its default, not the value in a worker's file. The
  worker logs once which fields of its file it ignores for the workspace.
- **`--set field=value`** changes one field of the stored config; the value
  is JSON (`'["a", "b"]'`, `3`, `false`) or plain text. **`--unset field`**
  stops setting it. **`--file config.toml`** replaces the whole config.
- **A save that changes single fields is refused when someone else saved in
  between** (run it again). A field name the server does not know is an
  error, in `--set`, in a file and over REST.
- **An empty list is a value:** `--set 'checks=[]'` means no checks, where
  leaving `checks` out means the group's checks, or none.
- **`skopos workspace config show`** prints what applies and where each
  field comes from; `--toml` prints the stored config in the form `--file`
  takes.
- **`skopos workspace config clear`** removes the config: the workspace is
  no longer managed and its workers go back to their own files.
- **In the dashboard:** Workspaces → Pipeline shows the config under the
  pipeline, and an admin can edit the stored config there as JSON.

### Groups

A group can hold a config too: `skopos group config set --group fortix …`.

- **A group's values are defaults for its managed workspaces,** field by
  field. The workspace's own value wins.
- **A group never makes a workspace managed.** A group config reaches only
  the workspaces that have a config of their own, so adding a pattern to a
  group never switches a dozen repositories at once. A workspace becomes
  managed by saving a config for it, even one that sets nothing:
  `skopos workspace config set --workspace <id>`, or `{}` in the dashboard.
- **Two groups may not set the same field** for a workspace both contain:
  the second save is refused. If a membership change creates that later,
  the first group by name keeps the field and the conflict is shown.

### What is refused

- A value the worker would refuse: an unknown `checkout`, a branch template
  without `{tail}` or `{ref}`, a timeout outside its range.
- A result that cannot work: `push.kind` without `push.remote_url`, a pull
  request without a push. This is checked on the merged config, so a group
  save, or removing a group's config, that would break a managed member is
  refused as well.
- A body with a field the server does not know, or without the `config`
  wrapper: it would otherwise read as a config that sets nothing.
- A `push.remote_url` with credentials in it. The token comes from the
  worker's environment (`push.token_env` in its file).

## Commands from the server

`setup`, `checks` and `preflight` are commands. A worker runs them on its
own host.

- **A worker takes them from the server only with
  `server_commands = true`** in its `[worker]` section. Without it those
  three fields stay the file's, even for a managed workspace; the other
  fields still come from the server. `skopos worker init` sets it for a new
  worker.
- **Whoever may change a workspace's config can run commands on every
  worker serving it** that has `server_commands` on. That is the admin
  permission, held by the root key and by keys created with `--admin`.
- **Their values are recorded in full** in the audit log and in the
  revision. Keep secrets out of them: use `${VAR}` from the worker's
  environment.
- **A worker logs every command change it applies,** and the workers view
  shows which workers take commands from the server.
- **A run on a worker that keeps its own commands says so** at the top of
  its log: the run's config revision does not describe those three fields
  there.

`mcp_deny` works the other way round: a deny list in the worker's file
stays in force for a managed workspace, added to the workspace's. The
operator of a host can always forbid a tool there.

## Who may change it

| What | Who |
|---|---|
| a workspace's config | a key with the admin permission and the workspace in scope |
| a group's config | an admin key whose scope covers every workspace in the group |
| runner definitions | an admin key that reaches every workspace |
| reading a workspace's config | any key with the workspace in scope |

The root key passes all of them. A worker's or an agent's key never holds
the admin permission.

## Revisions

Every save is a new revision; nothing is changed in place or removed.

- **What applies to a workspace has a revision of its own:** its groups'
  revisions and its own, in the order they apply.
- **The claim names it.** When a worker claims a run, the run records that
  revision as `config_rev`. The worker fetches exactly that revision and
  runs under it. A change saved while the run is out affects the next
  claim, not this run.
- **It reads back the same later:**
  `GET /api/workspaces/{id}/config/{rev}` returns the config as it was,
  whatever has been saved since.
- **A run whose config cannot be fetched is not run.** The worker gives it
  back and it is retried. It never falls back to the file for a managed
  workspace.
- **Idle work** (preflight, cleanup, indexing) uses the newest revision the
  worker has fetched. Claim answers carry one value for the config of the
  workspaces a worker offers, and the worker refetches when it changes.
- **A worker from before this feature gets no runs of a managed
  workspace.** It would run them from its own file. It shows as unhealthy
  for the workspace until it is upgraded; workers upgrade themselves when
  idle.

## On the worker

- **At start** the worker asks the server for each workspace's config and
  for the runner definitions. It waits for a server it cannot reach: it
  could not claim anything anyway. A server that answers and refuses (the
  worker's key may not read workspaces) is not waited for: the worker says
  so and starts.
- **What it cannot use is reported, not run.** A config that pushes while
  the worker's file names no `push.token_env` for the repo makes the
  workspace unhealthy on that worker, with the reason, and the server hands
  it none of the workspace's runs until that is fixed.
- **`skopos worker check`** asks the server too and checks the merged
  result: which workspaces are managed, which fields of the file are
  ignored, which commands stay local.

## Runner definitions

A runner definition names a runner once, for every worker:

```sh
skopos runner set claude-opus --kind claude --model opus --effort high --provider anthropic
skopos runner set glm-5-3 --kind opencode --model zai-coding-plan/glm-5.3 --provider zai
skopos runner list
```

A worker's account then only says which CLI it is a login for:

```toml
[[worker.accounts]]
name = "claude-1"
kind = "claude"
priority = 1
env = { CLAUDE_CONFIG_DIR = "/var/lib/skopos/accounts/claude-1" }

[[worker.accounts]]
name = "opencode-zai"
kind = "opencode"
models = ["zai-coding-plan/*"]
```

- **An account with `kind` feeds every defined runner of that kind,** with
  the definition's model and effort. `models` narrows it to the runners
  whose model matches one of the patterns, for a provider that is set up
  only on some hosts.
- **One login, one quota.** A limit hit on `claude-opus` pauses the same
  login's `claude-sonnet` too.
- **An account that names a `runner` keeps working.** When the server
  defines that name, the definition's model and effort replace the
  account's, and the worker says so.
- **The provider is what a [runner policy](runner-policy.md) matches.**
  A worker whose endpoint contradicts the declared provider (declared
  `local`, the endpoint is a cloud API) does not offer the runner.
- **Kinds:** `claude`, `opencode`, `codex`, `antigravity`. A `command`
  runner's argv runs on the worker's host, so it stays in the worker's
  file (`runner_config`).
- **Not offered** when the worker's file gives the name another kind, or
  when the CLI fails its lockdown check there. `skopos worker check` and
  the worker's log say which and why.
- **Workers pick up a change between runs.** Every claim answer names the
  definitions' revision; the workers view shows the revision each worker
  holds. A run already claimed finishes with the definitions it was
  claimed under.
- **`worker.runners`,** when the file sets it, still restricts what the
  worker offers: a defined runner it leaves out is not offered, and the
  worker says so.

Once a runner is defined with a provider, a pipeline save that newly names
it for a stage the workspace's policy never allows is refused. A runner the
server does not define is only warned about, as before.

## One file

Everything the server holds about how runs are configured exports as one
TOML file, and the file applies back:

```sh
skopos config export --out fleet.toml
skopos config apply fleet.toml --plan     # what would change
skopos config apply fleet.toml
```

```toml
[fleet.default]
allow = [{ provider = "anthropic" }, { provider = "local" }]

[runners.claude-opus]
kind = "claude"
model = "opus"
effort = "high"
provider = "anthropic"

[groups.fortix]
workspaces = ["github.com/fortix/freedom3"]
patterns = ["github.com/fortix/*"]

[groups.fortix.policy]
allow = [{ provider = "anthropic" }, { provider = "openai", stages = ["second_opinion"] }]

[workspaces."github.com/fortix/freedom3".config]
setup = ["./cli_tools/dothis setup"]
checks = ["./cli_tools/dothis check"]

[workspaces."github.com/fortix/freedom3".pipeline.stages]
review = { members = "security@claude-opus,tests@claude-opus", policy = "any-blocks" }
```

- **The file is authoritative for what it lists.** A workspace or group in
  the file gets exactly the policy, pipeline and config the file gives it;
  a section left out is cleared.
- **`[fleet]` and `[runners]` count only when present.** With `[fleet]`,
  the file states the ceiling and the default policy. With `[runners]`, it
  is the full set of runner definitions: one not in the file is deleted.
- **What the file does not list is left as it is,** and the plan names it.
- **`--plan`** lists each change (create, update, clear, delete) and makes
  none. It is what the reviewer of a pull request reads.
- **Every change is checked against the server as it is, before anything
  is written:** permission, validation, conflicts between groups, what a
  policy would strand (`--force` to save it anyway). A file with a section
  that fails those checks applies nothing.
- **Two kinds of change are decided when they are applied,** and the plan
  marks them: the settings of a group or workspace the same file creates,
  and a pipeline the current policy refuses when the file also changes a
  policy. The changes are applied in order — runner definitions, groups,
  policies, config, pipelines — and an apply that stops says how many were
  made. Applying the corrected file again continues from there.
- **The apply is recorded** with the file's sha256 and its git commit
  (`--commit`, or the commit of the file's checkout, marked `+modified`
  when the file differs from it). Each change inside it is recorded by its
  own action with the surface `apply`.
- **Each change needs the permission its own save needs.** A key that
  administers one workspace can apply a file that changes only that
  workspace's policy and config; changing its pipeline also needs the
  approver permission, as in the dashboard. Changing a group's members or
  a group's pipeline needs the root key. A section the file leaves as it is
  needs no permission.
- **A key that does not administer every workspace** may only list what it
  administers, and the plan names only its own workspaces as not in the
  file.
- **A key in the file the server does not read is an error,** not
  something applied around.

## Moving a worker's settings to the server

```sh
skopos workspace config import --from skopos-config.toml
skopos workspace config import --from skopos-config.toml --yes
skopos workspace config import --from a.toml --from b.toml --runners --yes
```

- **It proposes** the config for every workspace the file has a repo for:
  the fields the file sets, never the machine's own (`path`, `setup_env`,
  `push.token_env`). With `--runners`, also a definition for each runner
  the file's accounts name. A definition sets that runner's model on every
  worker that offers the name. Its provider is the one the file's accounts
  resolve to on the machine the import runs on; check it in the output.
- **It shows the difference** from what the server holds, and changes
  nothing without `--yes`.
- **Two files that disagree** about a workspace or a runner are a
  conflict: it is shown, and that workspace or runner is not imported.
- **Then, on each worker:** set `server_commands = true` to take the
  workspace's commands from the server. The fields the server now holds
  can go from the file; the worker ignores them either way.

## Audit

| Entity | Actions |
|---|---|
| `workspace_config` | `config.update` (the fields changed, old and new revision, command fields in full), `config.import`, `config.delete` |
| `runner_def` | `runner_def.create`, `runner_def.update`, `runner_def.delete` |
| `fleet_config` | `config.apply` (file hash, commit, the changes) |
| `worker` | `worker.facts` now includes whether the worker takes commands from the server; `worker.config_applied` when it applies new runner definitions |

## REST

| Method | Path | |
|---|---|---|
| `GET` `PUT` `DELETE` | `/api/workspaces/{id}/config` | a workspace's config |
| `GET` | `/api/workspaces/{id}/config/effective` | what applies now |
| `GET` | `/api/workspaces/{id}/config/{rev}` | what a revision names |
| `GET` `PUT` `DELETE` | `/api/groups/{id}/config` | a group's config |
| `GET` | `/api/fleet/config` | the runner definitions and their revision |
| `GET` | `/api/fleet/runners` | the definitions with their revisions |
| `PUT` `DELETE` | `/api/fleet/runners/{name}` | one definition |
| `GET` | `/api/config/export` | everything as one document |
| `POST` | `/api/config/apply` | apply a document, or plan it with `"plan": true` |
