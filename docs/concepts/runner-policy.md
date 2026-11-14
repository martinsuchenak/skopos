# Runner policy

A runner policy says which runners may work on a workspace: by who receives
the code, by model, by agent CLI, by stage and by what contains the run. The pipeline settings choose
which runner does a stage; the policy decides which runners may be chosen at
all.

Without a policy every runner is allowed, as before.

## Rules

A policy is a list of **allow rules**. A runner may be used when at least
one rule matches what its worker reports about it.

```sh
skopos policy set --group acme \
  --allow provider=anthropic \
  --allow provider=openai,stages=second_opinion \
  --allow provider=local
```

A rule is comma-separated `key=value` pairs. Every key it sets must match:

| Key | Matches |
|---|---|
| `provider` | who receives the code: `anthropic`, `openai`, `zai`, `google`, `aws`, `local`, or a name a runner declares |
| `model` | the model id, as a glob: `claude-opus-*`, `*glm*` |
| `kind` | the agent CLI: `claude`, `opencode`, `codex`, `antigravity`, `command` |
| `runner` | the runner name, as a glob |
| `stages` | the stages the rule covers, joined with `\|`; all stages when left out |
| `executor` | what must contain the run: `isolated`, `contained`, or a glob on the executor id (see [Containment](#containment)); any executor when left out |

`any` is a rule that matches every runner.

- **Globs:** `provider`, `model` and `runner` are globs. `*` matches any
  run of characters, `/` included; `?` matches one. Case doesn't matter.
  `provider=*` therefore matches every provider, unidentified ones
  included.
- **Matched on where the code goes, not on the CLI.** opencode with a model
  on the worker's own machine is `provider=local`. opencode with GLM is
  `provider=zai`. Claude Code pointed at Bedrock is `provider=aws`.
- **A rule without `provider` matches any provider,** one that could not be
  identified included. Name the provider unless you mean that.
- **Stages** are `plan`, `implement`, `review`, `qa`, `compare`,
  `synthesize`, `second_opinion` and `embeddings` (see
  [Embeddings](#embeddings)). A review-only item's branch review counts as
  `review`.

## Containment

A rule can require that something confines the run, not only that the
provider is acceptable:

```sh
skopos policy set --group personal \
  --allow provider=anthropic \
  --allow provider=local \
  --allow provider=zai,executor=isolated
```

With that policy a GLM run happens only on a worker where something goes
around that run. A worker that serves several repositories can then take
GLM work for one of them, inside a wrapper that shows the run only that
repository.

| `executor=` | Means | Met by |
|---|---|---|
| `isolated` | the run can't reach other workspaces' files, the worker's state or other accounts | a stage wrapper; a runner whose own lockdown is a sandbox (codex on macOS, where that lockdown is verified) |
| `contained` | the run can't reach beyond the worker's host container | everything above, and a worker that declares its host a container (`contained_by`) |
| a glob | that executor. It starts with a class: `local`, `host:`, `wrapper:`, `sandbox:` or `runner:` | `wrapper:bwrap:*`, `runner:codex`, `host:knot`, `sandbox:*` |

- **The executor is the worker's.** Each worker reports, per workspace,
  stage and runner, what would contain a run: `local`, `host:<name>`,
  `wrapper:<name>:<hash>`, `sandbox:<hash>` or `runner:<kind>` (see
  [Executors](../deployment/worker.md#executors)). The rule is matched on
  the one for the run's workspace, stage and runner.
- **A wrapper goes around everything that runs the branch's code:** the
  agent, and the setup and check commands the worker runs for that stage.
- **A wrapper can come from the server.** A wrapper defined once
  (`skopos wrapper set`) is attached to a stage's runs by a rule in the
  workspace's or group's config, by runner kind, provider, model or name.
  Its executor is `wrapper:<name>:<hash>`, the same on every worker, so a
  rule can name it: `executor=wrapper:opencode-bwrap:*`. See
  [Wrappers](workspace-config.md#wrappers). The config attaches a wrapper;
  only a policy rule with `executor` requires one.
- **A host container is not isolation.** `contained_by` says the worker
  can't reach beyond its container. Inside it a run still sees every clone
  and login on that worker, so it meets `contained` and not `isolated`.
- **Claude Code's sandbox settings are neither.** They limit what the
  agent's commands write and reach over the network; other files on the
  worker stay readable unless the settings deny them, and the worker's
  setup and check commands run outside the sandbox. A rule that accepts
  them names them: `executor=sandbox:*`.
- **A runner's own sandbox doesn't cover a branch review.** There the
  worker runs the repository's setup and checks itself, so codex reviewing
  an existing branch counts as `isolated` only inside a wrapper.
- **Compare and synthesize have no stage profile,** so nothing can wrap
  them. Under a rule that needs `isolated` they run only on a runner
  whose lockdown is a sandbox.
- **A rule with `executor` doesn't cover embeddings.** The embeddings
  endpoint has no executor, so only rules without one allow it.
- **A rule without `executor` accepts any executor,** `local` included.
- **A worker that reports no executors** (one from before this feature)
  gets no run whose rule needs one.
- **The worker checks again before it starts.** A run comes with the
  executor the server matched. A worker that would run it in something
  else doesn't start it and gives the run back with that reason; its next
  claim reports what it would run it in.
- **A worker can say it cannot run a stage with a runner at all:**
  `none:wrapper` (the wrapper the workspace's config asks for does not
  start there), `none:uncontained` (the stage is given tools or env and
  nothing contains it there) or `none:config` (the workspace's stage blocks
  cannot be used there). The server hands that worker no such run, with or
  without a policy, and the worker refuses one itself.
- **skopos doesn't verify a wrapper.** `isolated` means the operator put a
  wrapper around the run; what that wrapper lets through is the
  operator's. The [bwrap profile](../deployment/worker.md#a-bwrap-profile)
  in the worker docs has a test that runs it.

## Levels

| Level | Applies to | Set with |
|---|---|---|
| Fleet ceiling | every workspace, always | `--fleet ceiling` |
| Fleet default | workspaces with no policy of their own and in no group that has one | `--fleet default` |
| Group | every workspace in the group | `--group <name>` |
| Workspace | that workspace | `--workspace <id>` |

A workspace's **effective policy** is the ceiling, then its groups' and its
own policies. When it has none of those, the fleet default applies instead.
A runner has to be allowed by every one of them, so each level can only
narrow.

- **The ceiling** is the list nothing in the fleet may go beyond. On a
  server for one organisation it matches the list of approved providers.
- **The default** only fills in. A group or workspace policy replaces it,
  and may allow more than it does (never more than the ceiling).

```sh
skopos policy show --workspace github.com/acme/shop   # everything that applies, with sources
skopos policy show --group acme
skopos policy show --fleet ceiling
skopos policy clear --workspace github.com/acme/shop
```

The dashboard's Pipeline dialog shows the policy that applies to a
workspace, read-only.

## Who may change a policy

A key with the **admin permission** at that level (see
[API keys](api-keys.md#permissions)):

- a workspace: the workspace is in the key's scope;
- a group: the key reaches every workspace in the group;
- a group with patterns: the key reaches all workspaces (the group gains
  members as workspaces register);
- the fleet: the key reaches all workspaces.

The root key passes every level. An approver key does not.

Every change is a new revision and an audit event (`policy.set`,
`policy.delete`) with who made it, the old and the new revision and the
rules. Revisions are never changed or removed.

## What the server checks

**At every claim.** When a worker asks for work, the server resolves the one
runner each run is for, checks it against the workspace's policy with what
that worker reports about the runner, and writes the runner, the policy
revision and those facts to the run. This covers queued runs, a worker's own
interrupted and paused runs, and takeovers.

- **The same runner name can pass on one worker and fail on another,**
  because the check uses each worker's own facts.
- **A run nothing pins** (a stage with no preference) gets the first runner
  in the worker's order that the policy allows and that can run the stage.
  The server picks it, not the worker.
- **A run its owner may no longer resume** (the policy changed while it was
  paused) is released, and another worker the policy allows takes it over
  from the branch.
- **A worker that reports no runner facts** (one from before this feature)
  gets no runs from a workspace that has a policy.
- **A run that comes back for another runner** starts again on an account
  of that runner; the worker drops the account and agent session it used
  before.
- **A runner the server picked binds only that claim.** A retry or a
  takeover by another worker gets that worker's own pick.
- **While the policy can't be read** (a database error), nothing is handed
  out and nothing is given away. It is never treated as "no policy".

**What it doesn't do:**

- **A run already running keeps running** when the policy tightens. The
  check is at the claim, so the change applies from the run's next claim
  (a retry, a resume, the next stage).
- **A change takes up to 10 seconds to reach claims** when it comes from a
  group's membership changing. A policy save applies at once.

**When a run is reported.** The worker says what really contained the
agent, and the server compares it with the executor the claim matched on.
They differ only when the worker's configuration changed between the claim
and the run, or the worker misreports.

- **A difference is always recorded** (`run.executor_mismatch`: what ran,
  what the claim matched, the runner and the worker).
- **When the policy doesn't allow the runner with the executor that really
  ran, the run is recorded as failed** with that reason ("ran local, the
  claim matched wrapper:bwrap:…") and is not retried. What follows is what
  follows any failed run of that stage: an ordinary stage blocks the item;
  a panel member counts as one failed member; a failed compare, synthesize
  or second opinion never moves the item.
- **When the policy allows both,** the run stands.
- **A report that names no executor is not compared.** That is a run in
  which no agent ran in that attempt (one that resumed at its checks).

**When a second opinion is requested.** A runner the policy refuses for
second opinions is turned down at once (403, reason `policy`).

**When a policy is saved.** A change is refused, with a list of what it
would newly strand, when it would:

- leave a configured runner with no worker allowed to run it (for a rule
  with `executor`: no worker that confines that runner's runs in that
  stage);
- leave a stage that names no runner (plan, implement) with no worker
  that has an allowed runner, or with only workers that report no facts;
- drop a workspace's embedded code index.

`--force` saves it anyway, and the audit event says it was forced. Only
workers seen in the last day count, and only what the change itself causes
is listed.

**In the Pipeline dialog.** A configured runner the policy allows on no
worker is flagged there.

**At a pipeline save, when the server defines the runner.** A save that
newly names a runner with a [definition](workspace-config.md#runner-definitions)
that declares its provider is refused when no rule of the workspace's
policy allows that provider and model for the stage: the answer does not
depend on any worker. A rule that asks for an executor is not counted
against it, since some worker may meet it. A runner without a definition,
or one whose definition declares no provider, is only warned about. A
setting that was already there never blocks an unrelated edit.

## Runs that wait

A run waiting for a worker (queued, or released for a takeover) that no
worker may take under the policy is marked **stranded** after two minutes,
with the reason. It shows as "Waiting: no worker offers an allowed runner
for …" on the item. It is recorded as `run.stranded` when the mark is set,
and again if the reason changes. The mark goes when a worker that may take
it appears or the run stops waiting. Workers not seen for a day don't
count.

A run is stranded the same way when the only workers that have its runner
report that they cannot run the stage with it (`none:…`): "Waiting: no
worker can run …", with what stands in the way.

A run waiting for a runner nobody offers at all is not stranded by the
policy; the Pipeline dialog already says no worker offers that runner.

## What a worker reports

With every claim a worker sends **runner facts** for each runner it has:
the CLI, the model, the endpoint the CLI sends its requests to, and the
provider behind it. The dashboard's Workers view shows them.

| CLI | Endpoint |
|---|---|
| claude | `ANTHROPIC_BASE_URL`, else `api.anthropic.com`; the Bedrock, Vertex or Foundry endpoint when `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX` or `CLAUDE_CODE_USE_FOUNDRY` is set |
| codex | `OPENAI_BASE_URL`, else `api.openai.com` |
| opencode | the model's provider (the first part of `zai-coding-plan/glm-5.3`); that provider's `baseURL` in the opencode config when set; `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` for its built-in anthropic and openai providers |
| antigravity | Google |
| command | declared on the runner |

The provider is derived from the endpoint: a known domain gives the company,
a loopback address gives `local`, and anything else gives `unknown:<host>`.
A rule that names a provider doesn't match an unknown one.

- **Only the environment the agent really gets counts:** the account's
  `env` and the service's own environment. A variable from the worker's
  env file never reaches an agent, so it doesn't shape the facts.
- **A cloud's customer hosts aren't the cloud's AI service.** Bedrock's own
  hosts are `aws`, and Azure's AI service hosts are `microsoft`; a machine
  or API gateway at either is `unknown:<host>`.
- **A base URL that is set but unusable** is `unknown:invalid`, not the
  default vendor.
- **Facts are derived at startup.** After changing an account, a runner or
  an opencode config, restart the worker.

**Declare what can't be derived.** A model on another machine on your
network isn't assumed to be local, because a router there may forward to
anyone:

```toml
[worker.runner_config.opencode-local]
kind = "opencode"
provider = "local"        # a model on another machine on this network

[worker.runner_config.opencode-router]
kind = "opencode"
provider = "router"    # a router: match it with provider=router,model=…
```

A worker reports no facts when they would be untrue. It starts, logs each
problem as an error, and takes runs only for workspaces without a runner
policy. `skopos worker check` and `skopos worker install` refuse such a
file. The cases:

- an account's `env` sets `HTTPS_PROXY`, `HTTP_PROXY` or `ALL_PROXY`;
- a stage profile's `env` sets a proxy or an endpoint variable
  (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `CLAUDE_CODE_USE_*`,
  `OPENCODE_CONFIG`): facts are per runner, and a profile would change
  them for one workspace and stage only;
- two accounts of one runner differ in model, provider or endpoint (give
  each its own runner name);
- a declared provider contradicts the endpoint (declared `local`, the
  endpoint is a cloud API).

**Executors.** With the same claim a worker sends what contains its runs,
per workspace and then by stage and runner. The Workers view shows them
too, and `skopos worker runner` lists them per runner (`runs in:`).

A change in what a worker reports, runners or executors, is an audit event
(`worker.facts`, old and new).

**The limit of this control.** The facts are the worker's own word. The
policy stops wrong routing and misconfiguration by an honest worker. It
doesn't constrain a compromised worker, which holds the repository anyway.

## Embeddings

With semantic code search on, the server sends each symbol's name,
signature, doc summary and attributes, and every semantic query, to the
embeddings endpoint. The policy covers that endpoint as the stage
`embeddings`.

- **A workspace whose policy doesn't allow the endpoint's provider isn't
  embedded.** Nothing is sent, its existing vectors are dropped, and
  semantic search falls back to text search. The index status says why.
- **A rule with no `stages` covers embeddings too.** "Anthropic and local
  only" keeps a workspace's symbols away from a cloud embeddings API.
- **The provider** is derived from `codeindex.embeddings.url`. For a model
  on another machine on your network, declare it:

  ```toml
  [codeindex.embeddings]
  provider = "local"
  ```

  Set this before the first policy. Undeclared, such an endpoint is
  `unknown:<host>`, and a policy would stop every workspace being embedded.
  The save refuses that without `--force` and names this setting.
- **A declaration the URL contradicts stops the server starting:** `local`
  on a URL that points at a cloud API, for example.

## What the policy doesn't cover

The policy governs agent runs and embeddings. Code and item text also leave
the server by:

- **the Slack relay:** item text, plans and review findings;
- **the log output** (`--log-output-url`): run logs, which hold code;
- **MCP servers** in a worker's `mcp_config`: whatever an agent sends them.

## REST

- `GET|PUT|DELETE /api/policy/fleet/{ceiling|default}`
- `GET|PUT|DELETE /api/groups/{id}/policy`
- `GET|PUT|DELETE /api/workspaces/{id}/policy`
- `GET /api/workspaces/{id}/policy/effective`

A run carries `runner`, `runner_picked`, `policy_rev`, `facts` (with the
`executor` the claim matched on), `executor` and `grants` (what the worker
reported) and `stranded`; `GET /api/workers` carries each worker's
`runner_facts` and `executors`.
