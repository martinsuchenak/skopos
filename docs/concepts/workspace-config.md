# Workspace config on the server

The part of a workspace's configuration that every worker serving it shares
can live on the server: branches, setup and check commands, timeouts, push
settings, the MCP lists and what each stage's agent is given. Workers keep
only what is true of their own machine. Runner and wrapper definitions live
on the server too, so a runner name means the same model on every worker.

Nothing changes until you save a config for a workspace: a workspace without
one keeps using each worker's file.

## What lives where

| On the server | In each worker's file |
|---|---|
| `default_branch`, `branch_prefix`, `branch_template`, `checkout`, `protected_branches` | `id`, `state_dir`, the server URL and the worker's key |
| `setup`, `checks`, `check_fix_rounds`, `preflight`, `preflight_interval` | per repo: the clone's `path`, `setup_env`, `push.token_env` |
| `setup_timeout`, `check_timeout`, `quiet_timeout`, `plan_timeout`, `impl_timeout` | accounts: name, kind, credentials, priority |
| `push.kind`, `push.remote_url`, `push.pull_request`, `push.steps` | `contained_by`, `server_commands`, `auto_upgrade` |
| `mcp_deny`, `mcp_servers`; `mcp`: which MCP servers the runs get | `mcp_config`, for workspaces whose config sets no `mcp` |
| `stages`: each stage's tools, env and settings, and which wrapper goes around it | what contains a stage on this host: `contained_by`, a profile's `wrapper` and sandbox settings; `vars` |
| runner definitions, wrapper definitions, MCP server definitions and their secrets | |

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
`skopos group config show --group fortix` prints it, and
`skopos group config clear --group fortix` removes it.

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
own host. Each stage's block ([Stages](#stages)) counts with them: the
tools and env it gives an agent, and the wrapper it names, decide what
executes there.

- **A worker takes them from the server only with
  `server_commands = true`** in its `[worker]` section. Without it those
  fields stay the file's, even for a managed workspace; the other fields
  still come from the server. `skopos worker init` sets it for a new
  worker.
- **Whoever may change a workspace's config can run commands on every
  worker serving it** that has `server_commands` on. That is the admin
  permission, held by the root key and by keys created with `--admin`.
- **Their values are recorded:** in full in the revision, and in the audit
  note up to 1,500 characters per field. Keep secrets out of them: use
  `${VAR}` from the worker's environment.
- **A worker logs every command change it applies,** and the workers view
  shows which workers take commands from the server.
- **A run on a worker that keeps its own commands says so** at the top of
  its log: the run's config revision does not describe those fields
  there.

`mcp_deny` works the other way round: a deny list in the worker's file
stays in force for a managed workspace, added to the workspace's. The
operator of a host can always forbid a tool there.

## Stages

A stage block says what a stage's agent is given beyond the defaults:

```sh
skopos workspace config set \
  --set 'stages.implement={"tools":["Bash(./cli_tools/dothis:*)","Bash(mago:*)"],"env":{"CODECEPTION_ENV":"local"}}' \
  --set 'stages.review={"tools":["Bash(mago:*)"]}'
```

```toml
# the same in a config file (skopos workspace config set --file, or fleet.toml)
[stages.implement]
tools = ["Bash(./cli_tools/dothis:*)", "Bash(mago:*)"]
env = { CODECEPTION_ENV = "local", CACHE = "${CACHE_DIR}/ff3" }

[stages.review]
tools = ["Bash(mago:*)"]
```

- **Stages:** `plan`, `implement`, `review`, `qa`. A second opinion and a
  branch review run as a review. Compare and synthesize runs have no block:
  they are given nothing and no rule wraps them.
- **`tools`:** extra allowed tools, such as `Bash(npm run:*)`.
- **`env`:** extra variables for the agent. `${NAME}` is filled in on each
  worker from its `[worker.vars]` table, and from nothing else. A worker
  that does not declare a name the block uses reports the workspace
  unhealthy, with the name.
- **`settings`:** merged into the agent's settings. It may hold
  `permissions` (`allow`, `deny`, `additionalDirectories`) and `sandbox`.
  In JSON it is an object (`"settings":{"permissions":{"allow":[…]}}`); in
  a TOML file it is a string holding that JSON.
- **`wrap`:** which wrapper goes around the stage's runs; see
  [Wrappers](#wrappers).
- **One block per stage.** A workspace's block for a stage replaces its
  group's for that stage whole; the two are never merged. An empty block
  (`stages.review = {}`) gives the stage nothing, whatever the group says.
  That includes the group's `wrap` rules: a workspace's block for the stage
  drops them unless it repeats them.
- **A managed workspace's stages come from the server.** A stage without a
  block is given nothing. The tools, env and settings a worker's file sets
  for the workspace are not used, and `skopos worker check` names them.
- **What contains a stage stays the worker's.** `contained_by`, and a
  `wrapper` or sandbox settings in the worker's file, are kept for a
  managed workspace. So is what the file's settings deny
  (`permissions.deny`): those rules are added to the block's.
- **A stage is given its block only where something contains the run:** a
  wrapper, a host that is a container (`contained_by`), or sandbox settings
  (the worker's file's, or the block's own `sandbox`) for a runner that
  honours them. A worker where nothing contains the stage takes no run of
  it, and says so in its log and in `skopos worker check`. The other stages
  are not affected.
- **A variable of the runner's account wins.** A stage's env never replaces
  a variable the account on the worker sets.
- **A block a worker cannot use stops the workspace there.** When a block
  names a `${NAME}` the worker does not declare, or holds something the
  worker refuses, the worker takes none of the workspace's runs and reports
  why. Its own profiles do not apply instead.
- **Stage blocks are command fields.** A worker takes them only with
  `server_commands = true`; without it the worker's own profiles apply as
  before. Their values are in the revision in full and in the audit note
  (up to 1,500 characters per field), like `setup` and `checks`.

A stage's env is for the repository's own tools. It may not set:

- a proxy or endpoint variable (`HTTPS_PROXY`, `NO_PROXY`,
  `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `OPENCODE_CONFIG`, …): where a
  runner sends its requests is the runner's, and a policy is checked
  against it;
- a variable that configures the agent CLI or its provider: any
  `ANTHROPIC_`, `CLAUDE_`, `OPENAI_`, `OPENCODE_`, `CODEX_`, `GEMINI_`,
  `GOOGLE_`, `VERTEX_`, `AWS_` or `AZURE_` variable (the model, the
  credentials, the cloud account);
- a variable that selects the agent's login or its own configuration
  (`HOME`, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `XDG_CONFIG_HOME`,
  `XDG_DATA_HOME`, `XDG_STATE_HOME`);
- a name that looks like a credential (`*TOKEN*`, `*SECRET*`,
  `*PASSWORD*`, `*_KEY`): the env is recorded;
- a variable that changes what the agent's processes load or how git
  connects (`LD_*`, `DYLD_*`, `NODE_OPTIONS`, `NODE_EXTRA_CA_CERTS`,
  `SSH_AUTH_SOCK`, `GIT_SSH*`, `GIT_CONFIG*`, `BASH_ENV`).

These refusals catch a block that would quietly change which model, login
or provider a run uses. They are not what keeps a server admin out of a
worker: whoever may edit a managed workspace's config can already run
commands on every worker with `server_commands` on.

### Wrappers

A wrapper is a command a run starts behind, such as `bwrap` with its
arguments. It is defined once on the server and attached to a stage by
rule:

```sh
skopos wrapper set opencode-bwrap \
  --argv '["bwrap","--ro-bind","/","/","--bind","{dir}","{dir}","--bind","{gitdir}","{gitdir}","--bind","{rundir}","{rundir}"]' \
  --check '["bwrap","--ro-bind","/","/","true"]'

skopos group config set --group personal \
  --set 'stages.review={"wrap":[{"kind":"opencode","wrapper":"opencode-bwrap"}]}'
```

- **What goes inside it:** the agent, and the worker's setup and check
  commands of the stage.
- **Rules:** each names a `wrapper` and may match the runner's `kind`,
  `provider`, `model` and `runner` name (the last three as globs). The
  first rule that matches decides. A runner no rule matches gets no
  wrapper. `wrapper = "none"` leaves the runs it matches unwrapped.
- **Placeholders:** `{dir}`, `{gitdir}`, `{login}`, `{rundir}`, `{cli}`,
  `{run}`, `{stage}`, `{workspace}`, filled in for each run, and `${NAME}`
  from the worker's `[worker.vars]`. See
  [Containing agent runs](../deployment/worker.md#containing-agent-runs).
- **What a run records:** executor `wrapper:<name>:<hash>`. The hash covers
  the argv as defined, so it is the same on every worker and changes when
  the argv does. A policy rule can require it:
  `executor=wrapper:opencode-bwrap:*`, or `executor=isolated`.
- **`check`** is a command that shows the wrapper can start on a host. A
  worker runs it when it first needs the wrapper, when the definition
  changes and every five minutes. It shows that the wrapper starts, not
  that it isolates anything.
- **A worker that cannot use the wrapper takes no run it is for:** its
  check fails there, it uses a `${NAME}` the worker does not declare, the
  worker refuses the definition, or the definition is gone. A definition
  the worker refuses stays refused whatever its check says. The worker reports executor `none:wrapper` for
  those runs and the server hands them to another worker. A run nobody can
  take is marked stranded, with the reason. Nothing runs unwrapped
  instead.
- **A worker's own wrapper stays.** Where the worker's file sets a wrapper
  for the stage, that one goes around the run and the rule is not used.
- **Only a worker with `server_commands = true`** runs a wrapper from the
  server.
- **A rule can only name a wrapper that is defined.**
- **`skopos wrapper list`** shows each wrapper with its argv, its check,
  the executor id its runs record, and which workspaces and groups name it.
- **`skopos wrapper delete <name>`** removes one. While stage blocks still
  name it the delete is refused; `--force` deletes it anyway, and the runs
  those rules match then wait.
- **An argv that only sets variables** (`env K=V`) is refused: it puts
  nothing around the run. A worker whose whole host is a container says so
  with `contained_by`. A leading `env K=V …` before the wrapper's command
  may not set what a stage's env may not set.

A rule attaches a wrapper; a [runner policy](runner-policy.md#containment)
requires one. On a worker without `server_commands`, a rule from the server
is not used and the run starts in whatever that worker's file gives it. To
keep such a run from starting, the policy needs a rule with `executor`.

## MCP servers

The MCP servers a workspace's runs may use (a tool router, a search
service) can be defined on the server, with their credentials, and given to
workspaces by name.

```sh
# define the server; its credential is named, not written
skopos mcp-server set llmrouter --url https://llmrouter.example.com/mcp \
    --header 'Authorization=Bearer ${secret:token}' --for 'github.com/fortix/*'

# store the credential (asked for at the terminal, or piped, or --from-env)
skopos mcp-server set-secret llmrouter token

# give it to a group's runs, and allow its tools
skopos group config set --group fortix \
    --set 'mcp=["llmrouter"]' --set 'mcp_servers=["mcp__llmrouter"]'
```

- **Two kinds of server:** `http` (a URL and headers) and `command` (a
  command the worker runs, with env):
  `skopos mcp-server set scriptling --command '["scriptling","mcp"]'`.
  `${NAME}` in a command server's command and env is filled in on each
  worker from its `[worker.vars]`.
- **A credential is never part of a definition.** It is named
  `${secret:NAME}` in the URL, a header or an env value. A definition is
  stored, exported and recorded as written, so what looks like a credential
  is refused unless it is a secret: a header, variable or query parameter
  named like one (`Authorization`, a token, a key, a password, a session),
  a value that reads as a generated token (in a URL's path too), a URL with
  a user and password, a credential passed as a command argument. The check
  errs on the side of asking for a secret; an id that reads as a token goes
  in a secret as well.
- **A server says which workspaces it is for** (`--for`, `workspaces` in a
  file): patterns on the workspace id, like a workspace group's, and `*`
  for every workspace. A server that names secrets has to say. A config can
  only name a server that is for its workspace (for a group: for every
  workspace in it), so a workspace's own admin cannot pull another team's
  server and its credentials into their runs. The claim checks it again.
- **A secret's value is set apart and never shown.** `set-secret` reads it
  from the terminal without echo, from piped input, or from an environment
  variable (`--from-env`); never from the command line. It is at least 8
  characters. No command, endpoint, export or audit note returns it.
- **`mcp` says which servers a workspace's runs get;** `mcp_servers` says
  which of their tools a run may call (`mcp__llmrouter` for all of them,
  `mcp__llmrouter__search` for one), and `mcp_deny` which it may not. A
  server in `mcp` without an entry in `mcp_servers` that allows its tools
  is refused.
- **Once a workspace's config sets `mcp`, the server holds its runs' MCP
  servers.** The runs get exactly the servers listed, plus skopos itself,
  and nothing from a worker's own `mcp_config`. `mcp = []` gives them
  none. A group can set it for its managed workspaces; a workspace's own
  `mcp` replaces the group's.
- **Not set at any level:** each worker's `mcp_config` applies, as before.
- **A run's servers come with its claim.** The answer to a worker's claim
  carries the servers for that run's workspace, with the secrets filled in.
  They are not part of the config a worker fetches, so a worker holds a
  workspace's credentials only while it runs that workspace's run. The
  worker writes them into the run's own MCP file, keeps their values out of
  the run's log, and removes the file when the run ends.
- **A server that cannot be delivered is left out, and the run's log says
  so:** a secret that is not set, a secret sealed under a key the server no
  longer holds, a `command` server for a worker without
  `server_commands = true`, a `${NAME}` the worker does not declare. The
  worker's own `mcp_config` never takes its place.
- **claude and opencode runs get them.** A runner that takes no MCP
  servers (codex) gets none.
- **A worker older than this feature gets no runs of a workspace whose
  config sets `mcp`,** and shows as unhealthy for it until it upgrades.
- **A secret is for the server it was set for.** Changing an http server's
  host, or a command server's command, deletes its stored secrets: set
  them again. A change to the path, a header or the workspaces keeps them.
- **`skopos mcp-server list`** shows each server, the workspaces it is
  for, which of its secrets are set (and by whom, never the value), and
  which workspaces and groups name it.
- **`skopos mcp-server unset-secret <server> <name>`** deletes a stored
  value. Runs go without the server until it is set again.
- **`skopos mcp-server delete <name>`** removes a server and its stored
  secrets. While a config still names it the delete is refused; `--force`
  deletes it anyway.

### The secret key

Secrets are stored sealed under the server's secret key,
`integrations.secret_key` (`SKOPOS_INTEGRATIONS_KEY`): the same key that
seals webhook signing secrets.

- **Without it** the server stores no secret. MCP servers that need none
  still work.
- **Keep a copy of the key apart from the database and its replica.**
  Whoever has the database without the key cannot open the secrets, and
  neither can you.
- **`skopos secret-key status`** shows the id of the current key (a
  fingerprint, not the key) and how many stored values each key sealed.
- **To change the key:**
  1. Set the new key as `integrations.secret_key` and the old one as
     `integrations.previous_secret_key`, and restart the server. It opens
     what either key sealed and seals with the new one.
  2. `skopos secret-key rotate`: every stored secret is sealed again under
     the new key, all of them or none.
  3. Remove `integrations.previous_secret_key` and restart.
- **A key changed without those steps** leaves the stored secrets
  unreadable: the server says so when it starts, the servers that need them
  are left out of runs, and webhook deliveries fail. Setting the old key as
  `integrations.previous_secret_key` and rotating puts it right.
- **A webhook's previous signing secret** that is past its day of use is
  dropped by a rotation, not sealed again.
- **An earlier release cannot open what this one seals.** After a webhook
  is created or its secret rotated on this release, going back to an
  earlier release leaves that webhook unable to sign.
- **Root key only** for the status and the rotation.

## Who may change it

| What | Who |
|---|---|
| a workspace's config | a key with the admin permission and the workspace in scope |
| a group's config | an admin key whose scope covers every workspace in the group |
| runner definitions, wrapper definitions | an admin key that reaches every workspace |
| MCP server definitions and their secrets | an admin key that reaches every workspace; any admin key may list them |
| the secret key's status and rotation | the root key |
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
  ignored, which commands stay local, whether each wrapper the stages name
  starts, and which stages the worker cannot run.
- **A run is compared with its claim after its config is in place.** What
  contains a run depends on the config revision the claim named. When the
  worker would run it in something other than what the claim matched, it
  gives the run back and its next claim reports the new state.

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

[wrappers.opencode-bwrap]
argv = ["bwrap", "--ro-bind", "/", "/", "--bind", "{dir}", "{dir}"]
check = ["bwrap", "--ro-bind", "/", "/", "true"]

[mcp.llmrouter]
type = "http"
url = "https://llmrouter.example.com/mcp"
headers = { Authorization = "Bearer ${secret:token}" }
workspaces = ["github.com/fortix/*"]

[groups.fortix]
workspaces = ["github.com/fortix/freedom3"]
patterns = ["github.com/fortix/*"]

[groups.fortix.policy]
allow = [{ provider = "anthropic" }, { provider = "openai", stages = ["second_opinion"] }]

[workspaces."github.com/fortix/freedom3".config]
setup = ["./cli_tools/dothis setup"]
checks = ["./cli_tools/dothis check"]

[workspaces."github.com/fortix/freedom3".config.stages.implement]
tools = ["Bash(./cli_tools/dothis:*)", "Bash(mago:*)"]
env = { CODECEPTION_ENV = "local" }

[workspaces."github.com/fortix/freedom3".pipeline.stages]
review = { members = "security@claude-opus,tests@claude-opus", policy = "any-blocks" }
```

- **The file is authoritative for what it lists.** A workspace or group in
  the file gets exactly the policy, pipeline and config the file gives it;
  a section left out is cleared.
- **`[fleet]`, `[runners]`, `[wrappers]` and `[mcp]` count only when
  present.** With `[fleet]`, the file states the ceiling and the default
  policy. With `[runners]`, `[wrappers]` or `[mcp]`, it is the full set of
  those definitions: one not in the file is deleted.
- **`[mcp]`, when present, is the full set of MCP server definitions.**
  The file never holds a secret: a definition names its secrets, and
  applying it keeps the values stored for the ones it still names. The plan
  says which named secret has no value yet. A file with a credential
  written into a definition is refused.
- **A wrapper or MCP server the file drops** is deleted after the configs
  in the file have changed. When a workspace or group outside the file
  still names it, the apply is refused unless forced.
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
  policy. The changes are applied in order — runner, wrapper and MCP
  server definitions, groups, policies, config, pipelines, then the
  wrappers and MCP servers the file drops — and an apply that stops says how many were made. Applying the corrected file again continues from there.
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
  `push.token_env`). What the repo's profile gives each stage (tools, env,
  settings) becomes its stage block. A profile's `wrapper` and the sandbox
  block of its settings stay in the file. With `--runners`, also a definition for each runner
  the file's accounts name. A definition sets that runner's model on every
  worker that offers the name. Its provider is the one the file's accounts
  resolve to on the machine the import runs on; check it in the output.
- **It shows the difference** from what the server holds, and changes
  nothing without `--yes`.
- **Two files that disagree** about a workspace or a runner are a
  conflict: it is shown, and that workspace or runner is not imported.
- **MCP servers are not imported.** A worker's `mcp_config` holds
  credentials; define the servers with `skopos mcp-server set` and set
  their secrets by hand.
- **Then, on each worker:** set `server_commands = true` to take the
  workspace's commands and stage blocks from the server. The fields the
  server now holds can go from the file; the worker ignores them either
  way. A worker inside a container declares it with `contained_by`; a
  `${NAME}` an imported env value uses goes into `[worker.vars]`.

## Audit

| Entity | Actions |
|---|---|
| `workspace_config` | `config.update` (the fields changed, old and new revision, command fields in full), `config.import`, `config.delete` |
| `runner_def` | `runner_def.create`, `runner_def.update`, `runner_def.delete` |
| `wrapper` | `wrapper.create`, `wrapper.update` (the argv and check in full, the executor id), `wrapper.delete` |
| `mcp_server` | `mcp_server.create`, `mcp_server.update` (the definition: secret names, never values), `mcp_server.delete`, `mcp_server.secret_set`, `mcp_server.secret_unset` (the secret's name) |
| `secret_key` | `secret_key.rotate` (how many values, the key ids) |
| `fleet_config` | `config.apply` (file hash, commit, the changes) |
| `worker` | `worker.facts` now includes whether the worker takes commands from the server; `worker.config_applied` when it applies new runner definitions |

## REST

| Method | Path | |
|---|---|---|
| `GET` `PUT` `DELETE` | `/api/workspaces/{id}/config` | a workspace's config |
| `GET` | `/api/workspaces/{id}/config/effective` | what applies now |
| `GET` | `/api/workspaces/{id}/config/{rev}` | what a revision names |
| `GET` `PUT` `DELETE` | `/api/groups/{id}/config` | a group's config |
| `GET` | `/api/fleet/config` | the runner and wrapper definitions and their revision |
| `GET` | `/api/fleet/runners` | the runner definitions with their revisions |
| `PUT` `DELETE` | `/api/fleet/runners/{name}` | one runner definition |
| `GET` | `/api/fleet/wrappers` | the wrapper definitions with their revisions |
| `PUT` `DELETE` | `/api/fleet/wrappers/{name}` | one wrapper definition (`DELETE ?force=1` while stage blocks name it) |
| `GET` | `/api/fleet/mcp-servers` | the MCP server definitions and which secrets are set |
| `PUT` `DELETE` | `/api/fleet/mcp-servers/{name}` | one MCP server definition |
| `PUT` `DELETE` | `/api/fleet/mcp-servers/{name}/secrets/{secret}` | a secret's value: set or deleted, never read |
| `GET` | `/api/fleet/secret-key` | which key sealed the stored secrets |
| `POST` | `/api/fleet/secret-key/rotate` | seal them all again under the current key |
| `GET` | `/api/config/export` | everything as one document |
| `POST` | `/api/config/apply` | apply a document, or plan it with `"plan": true` |
