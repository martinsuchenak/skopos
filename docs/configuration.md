# Configuration

Skopos loads configuration from (in priority order): **CLI flags** > **environment variables** > **TOML config file** > **defaults**.

## Config file

```bash
cp skopos-config.example.toml skopos-config.toml
```

Optional — if absent, skopos runs on defaults. The file is gitignored (may contain secrets).

## All options

| Flag | Env var | Config path | Default | Description |
|------|---------|-------------|---------|-------------|
| `--config` | `CONFIG_FILE` | — | `skopos-config.toml` | Path to config file |
| `--log-level` | `LOG_LEVEL` | `log.level` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `--log-format` | `LOG_FORMAT` | `log.format` | `text` | `text` or `json` |
| `--dns-server` | `SKOPOS_DNS_SERVER` | `dns.server` | — | DNS server for `srv+` URLs, `host:port` (Consul: `127.0.0.1:8600`); default: the system resolver ([SRV URLs](#srv-urls)) |
| `--log-output-url` | `LOG_OUTPUT_URL` | `log.output.url` | — | Also send logs to this HTTP log intake ([Log output](#log-output)) |
| `--log-output-format` | `LOG_OUTPUT_FORMAT` | `log.output.format` | `ndjson` | `ndjson`, `loki` or `elasticsearch` |
| `--log-output-stream` | `LOG_OUTPUT_STREAM` | `log.output.stream` | `skopos` | Stream name: VictoriaLogs stream, Loki job, Elasticsearch index |
| `--log-output-header` | `LOG_OUTPUT_HEADER` | `log.output.header` | — | Header(s) for the intake, `Name: value` (several separated by `;`) |
| `--server-host` | `SERVER_HOST` | `server.host` | `127.0.0.1` | HTTP listen host (set `0.0.0.0` to listen on all interfaces) |
| `--server-port` | `SERVER_PORT` | `server.port` | `8080` | HTTP listen port (REST, MCP at `/mcp`, dashboard, SSE) |
| `--database-path` | `DATABASE_PATH` | `database.path` | `skopos.db` | SQLite database file path. Keep it on local disk, not NFS or SMB (skopos warns at startup); on a cluster, replicate it with Litestream ([Nomad guide](deployment/nomad.md)) |
| `--api-key` | `SKOPOS_API_KEY` | `auth.api_key` | (empty = auth disabled, loopback only) | API key; when set, required by every endpoint (REST, MCP, SSE) |
| `--insecure-no-api-key` | `SKOPOS_INSECURE_NO_API_KEY` | — | `false` | Allow running without an API key on a non-loopback bind (authentication stays disabled — know what you are exposing) |
| `--health-stuck-threshold` | `HEALTH_STUCK_THRESHOLD` | `health.stuck_threshold_minutes` | `15` | Minutes before an active agent is marked stuck (0 disables) |
| `--cleanup-retention-days` | `CLEANUP_RETENTION_DAYS` | `cleanup.retention_days` | `30` | Days to retain data (0 disables cleanup). The cleanup runs every 10 minutes; a run longer than a second is logged as a `slow cleanup` warning |
| `--public-url` | `SKOPOS_PUBLIC_URL` | `server.public_url` | — | The URL other systems reach the server at; webhook and feed payloads link to items from it ([Connecting another system](guides/external-systems.md)) |
| `--integrations-key` | `SKOPOS_INTEGRATIONS_KEY` | `integrations.secret_key` | — | The server's secret key: it seals the secrets skopos stores (webhook signing secrets, MCP server credentials). Without it webhooks are off and no secret can be stored (the event feed works). Keep a copy apart from the database, and change it only by rotating — see [The secret key](concepts/workspace-config.md#the-secret-key) |
| `--integrations-previous-key` | `SKOPOS_INTEGRATIONS_PREVIOUS_KEY` | `integrations.previous_secret_key` | — | The secret key the server had before. Set it while changing the key, run `skopos secret-key rotate`, then remove it |
| `--webhooks-allow-private` | `SKOPOS_WEBHOOKS_ALLOW_PRIVATE` | `integrations.allow_private` | — | CIDRs or addresses webhooks may reach although private, loopback or link-local (e.g. `10.20.0.0/16`) |
| `--webhooks-allow-http` | `SKOPOS_WEBHOOKS_ALLOW_HTTP` | `integrations.allow_http` | — | Hosts webhooks may reach over plain http (otherwise https only). Deliveries connect directly; proxy settings aren't used |
| `--index-dir` | `SKOPOS_INDEX_DIR` | `codeindex.dir` | `.skopos/indexes` | Directory for per-workspace code index databases |
| `--refresh-interval` | `SKOPOS_REFRESH_INTERVAL` | `codeindex.refresh_interval` | `0` (off) | Poll registered `git_url` workspaces for server-side refresh (e.g. `30m`, `1h`); CI can also POST the refresh endpoint on merge |
| `--embeddings-url` | `SKOPOS_EMBEDDINGS_URL` | `codeindex.embeddings.url` | (empty = disabled) | OpenAI-compatible `/v1/embeddings` endpoint for semantic code search (local Ollama works) |
| `--embeddings-model` | `SKOPOS_EMBEDDINGS_MODEL` | `codeindex.embeddings.model` | (empty) | Embedding model name (required with `--embeddings-url`) |
| `--embeddings-provider` | `SKOPOS_EMBEDDINGS_PROVIDER` | `codeindex.embeddings.provider` | (derived from the URL) | Who runs the embeddings endpoint, for [runner policies](concepts/runner-policy.md#embeddings). A loopback URL is `local`; declare `local` for a model on another machine on your network |
| `--embeddings-api-key` | `SKOPOS_EMBEDDINGS_API_KEY` | `codeindex.embeddings.api_key` | (empty) | API key for the embeddings endpoint (not needed locally) |
| `--vector-store` | `SKOPOS_VECTOR_STORE` | `codeindex.embeddings.vector_store` | `sqlite` | Vector backend: `sqlite` (embedded brute force) or `qdrant` (external, monorepo scale) |
| `--qdrant-url` | `SKOPOS_QDRANT_URL` | `codeindex.embeddings.qdrant_url` | (empty) | Qdrant REST address when `--vector-store=qdrant` (e.g. `http://localhost:6333`) |
| `--qdrant-api-key` | `SKOPOS_QDRANT_API_KEY` | `codeindex.embeddings.qdrant_api_key` | (empty) | Qdrant API key when required |
| `--run-stage` | — | — | (none) | **Deprecated**: imported once into the [pipeline settings](#pipeline-settings-agent-pipeline), removed next release. Agent pipeline routing, `<workspace>:<stage>=<runner>`, repeatable; plan/review/qa take a `[focus@]runner,…` list — see [Stage routing](#pipeline-settings-agent-pipeline) |
| `--stage-policy` | — | — | `any-blocks`, 60m | **Deprecated**: imported once into the [pipeline settings](#pipeline-settings-agent-pipeline), removed next release. How a multi-agent stage is decided, `<workspace>:<stage>=<policy>[,early-exit][,timeout=<duration>]` — see [Multi-agent review and QA](#multi-agent-review-and-qa) |
| `--agents-default` | — | — | `panel` | **Deprecated**: imported once into the [pipeline settings](#pipeline-settings-agent-pipeline), removed next release. What items without their own switch get on a multi-agent stage, `<workspace>=<panel\|solo>`, repeatable — see [Per-item switch](#per-item-switch-panel-or-solo) |

### Client (written by `skopos setup`)

The `[client]` section configures CLI commands (`index push`, queries) to
talk to a remote server without passing flags. `skopos setup` writes it
automatically; flags and `SKOPOS_SERVER_URL`/`SKOPOS_API_KEY` override it.

| Config path | Env var | Default | Description |
|-------------|---------|---------|-------------|
| `client.server_url` | `SKOPOS_SERVER_URL` | (empty = local index) | Remote skopos URL for index/query commands |
| `client.api_key` | `SKOPOS_API_KEY` | (empty) | API key for the remote server |

### Worker

The `[worker]` section configures the execution plane (`skopos worker`).
Scaffold it with `skopos worker init` (which also writes a `skopos-worker.env`
for secrets). See [deployment/worker.md](deployment/worker.md) for the full
walkthrough.

| Config path | Flag | Default | Description |
|-------------|------|---------|-------------|
| `worker.id` | `--worker-id` / `SKOPOS_WORKER_ID` | `worker-<hostname>` | Worker identity — must keep the `worker-` prefix |
| `worker.state_dir` | `--state-dir` / `SKOPOS_WORKER_STATE` | `~/.local/state/skopos-worker` | Worktrees, logs, session state |
| `worker.runners` | — | every runner the accounts use | Runners claims offer (only those with a usable account are offered at any moment) |
| `worker.workspaces` | `--workspace` | (from repos) | Workspace IDs this worker serves |
| `worker.poll_every` | — | `30s` | Idle claim interval |
| `worker.server_commands` | — | `false` | Take a workspace's `setup`, `checks` and `preflight` commands, its stage blocks (each stage's tools, env and settings) and the wrappers they name from the server once the server holds its config. Off, those stay this file's; the workspace's other shared fields still come from the server. `skopos worker init` sets it for a new worker — see [Workspace config on the server](concepts/workspace-config.md#commands-from-the-server) |
| `worker.vars.<NAME>` | — | — | This host's value for `${NAME}` in what the server supplies: a stage's env, a wrapper's argv, and the command and env of a `command` MCP server. Only these names are filled in — see [Stages and wrappers from the server](deployment/worker.md#stages-and-wrappers-from-the-server) |
| `worker.contained_by` | — | — | Names the container the worker's whole host is (a Knot space, a VM, a Docker container). Runs then record the executor `host:<name>` unless a wrapper or a sandbox goes around the run, and stage profiles may grant capabilities without a wrapper — see [A worker inside a container](deployment/worker.md#a-worker-inside-a-container) |
| `worker.auto_upgrade` | — | `true` | An idle worker behind the server's release downloads it, verifies its checksum, replaces its own binary and exits with code 75 for its service manager to restart it — see [Self-healing](deployment/worker.md#self-healing) |
| `worker.upgrade_url` | — | GitHub releases | Where releases come from: `<url>/v<version>/skopos_<os>_<arch>` and `checksums.txt` |
| `worker.repos."<workspace-id>"` | — | — | Per-workspace local clone: `path`, `default_branch`, `branch_prefix`, `branch_template` (default `{prefix}{tail}-{slug}`; placeholders `{prefix}`, `{tail}`, `{slug}`, `{ref}`; needs `{tail}` or `{ref}` — see [Branch names and existing branches](deployment/worker.md#branch-names-and-existing-branches)), `setup`, `setup_env`, `setup_timeout` (all setup commands together, default `10m`, at most `2h` — see [Setup](deployment/worker.md#setup)), `check_timeout` (all checks together, default `30m`, at most `2h`), `quiet_timeout` (stops one setup or check command that prints nothing for that long; off by default, at most `2h`), `preflight` / `preflight_interval` (commands that say whether this worker can take the repo's work now, run every `5m` by default; failing keeps the workspace out of its claims — see [Self-healing](deployment/worker.md#self-healing)), `checkout` (`worktree`, the default: a worktree per item; `branch`: the clone itself, switched to the item's branch for the run — see [Branch checkout](deployment/worker.md#branch-checkout)), `checks` (run after every implementation; a failure goes back to the implementer up to `check_fix_rounds` times, default 2, then blocks the item — see [Checks](guides/pipeline.md)), `check_fix_rounds`, `protected_branches`, `profile`, `stages.<stage>`, and `push.{kind,remote_url,token_env,pull_request,steps}` (`kind` is `github`, `gitea` or `none`; with no push block the worker never pushes, and a block with `remote_url` and no `kind` is refused at startup; a block with only `token_env` is for a workspace whose push settings the server holds, and keeps the workspace out of the worker's claims while the server does not; `pull_request` = `draft`, `ready` or `none` opens a pull request after the push — see [Pushing and pull requests](deployment/worker.md#pushing-and-pull-requests); `steps = false` stops pushing the implementer's commits as it makes them) |
| `worker.accounts` | — | — | Credential pool: `name`, `runner` (`claude`, `opencode`, `codex`, or a `runner_config` name such as an `antigravity` one), `priority`, `model`, `effort` (reasoning effort, checked against the runner's CLI: claude/antigravity `low`…`max`, codex `minimal`…`xhigh`, opencode a model variant, command `{effort}`), `env` (injected into agent runs only), `login_dir` (where the account's CLI keeps its login, for a stage wrapper's `{login}`; default: the directory its kind uses). Or, in place of `runner`, `model` and `effort`: `kind` (`claude`, `opencode`, `codex`, `antigravity`) makes the account a login that feeds every runner of that kind the server defines, and `models` (patterns such as `zai-coding-plan/*`) narrows which — see [Runner definitions](concepts/workspace-config.md#runner-definitions) |
| `worker.runner_config.<name>` | — | — | Agent CLI per runner name: `kind` (`claude` / `opencode` / `codex` / `antigravity` / `command`; codex takes read-only stages only; antigravity and command are unverified, wrapper-only), `bin`, `config` (opencode base config: providers/models), `command` (argv template for any other CLI: `{prompt}` `{dir}` `{model}` `{effort}` — unverified, wrapper-only) |
| `worker.claude_bin` | — | `claude` | Claude Code binary (or `runner_config.claude.bin`) — must support `--setting-sources`, `--restricted`, `--strict-mcp-config` (checked at start) |
| `worker.plan_timeout` / `worker.impl_timeout` | — | `10m` / `90m` | Agent run timeouts (review uses the plan timeout) |
| `worker.qa_timeout` | — | `30m` | How long a QA run's agent may take: it tests the change on a running system ([QA](#qa-testing-the-change)) |
| `worker.mcp_config` | — | — | JSON file with the MCP servers pipeline runs get (Claude Code `mcpServers` shape); required when `mcp_servers` is set |
| `worker.mcp_servers` | — | — | MCP tool prefixes allowed in plan/implement runs, e.g. `["mcp__skopos"]` |
| `worker.mcp_deny` | — | none | MCP tools denied in every run (added to the built-in git/network denials) |
| `worker.profiles.<name>.<stage>` | — | — | Operator-supplied stage executor (`plan`/`implement`/`review`/`qa`): `wrapper` (placeholders `{dir}` `{gitdir}` `{login}` `{rundir}` `{cli}` `{run}` `{stage}` `{workspace}`), `settings`, `tools`, `env` — see [deployment/worker.md](deployment/worker.md#containing-agent-runs). For a workspace the server manages (with `server_commands`), `tools`, `env` and the settings besides `sandbox` come from the server instead |
| — | `--env-file` / `SKOPOS_WORKER_ENV` | `<config-dir>/skopos-worker.env` when present | `KEY=VALUE` secrets loaded before the config (real environment wins) |

Manage runners and accounts with `skopos worker runner add | list | test`
instead of editing these by hand — see
[deployment/worker.md](deployment/worker.md#adding-runners-the-wizard).

### Workspace config on the server

A workspace's shared settings — branches, `setup`, `checks`, `preflight`,
timeouts, push settings, `mcp_deny`, `mcp_servers` — can be held on the
server instead of in each worker's file, as revisions:
`skopos workspace config show|set|clear|import`, `skopos group config …`,
or over REST. Runner definitions (`skopos runner list|set|delete`) give a
runner name one model, effort and provider on every worker. The whole run
configuration exports as one file and applies back:
`skopos config export`, `skopos config apply [--plan]`. Changing any of it
needs the admin permission. A workspace with a config on the server ignores
the same fields in a worker's file. See
[Workspace config on the server](concepts/workspace-config.md).

### Runner policy

Which runners may work on a workspace — by provider, model, agent CLI and
stage — is stored in the database as revisions and edited without a
restart: `skopos policy show|set|clear` with `--workspace`, `--group` or
`--fleet ceiling|default`, or over REST. Changing one needs the admin
permission at that level. See [Runner policy](concepts/runner-policy.md).

### Pipeline settings (agent pipeline)

Which worker runner takes each stage of a workspace — `plan`, `compare`,
`synthesize`, `implement`, `review`, `qa` — is stored in the database and
edited without a restart:

- **Dashboard:** Workspaces → **Pipeline** on a workspace, or on a group.
- **CLI:** `skopos workspace pipeline show|set|clear` and `skopos group
  pipeline show|set|clear`.
- **REST:** `GET`/`PUT /api/workspaces/{id}/pipeline` and
  `/api/groups/{id}/pipeline`. **MCP:** `workspace_pipeline` (read only).

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage plan --runners claude
skopos workspace pipeline set --workspace github.com/you/repo --stage review --runners opencode-glm-5-3
skopos workspace pipeline show --workspace github.com/you/repo
```

- **Plan and implement** always run. Without a setting, any runner's
  account may take them.
- **Review and QA** run only when set. Otherwise a finished implementation
  goes straight to `in_review`. The order is implement → review → QA: QA
  starts when the review has passed, or right after the implementation when
  no review is set. A stage that blocks stops the item there. The item
  preview shows QA as a step of its own, after Agent review, in workspaces
  that set it. **Compare and synthesize** are off unless set.
- **Pinned runs wait for their own runner.** A pinned run waits for an
  account of its runner; it never falls back to another runner. The dialog
  and `show` warn about a runner no worker serving the workspace offers yet.
- **Group defaults.** A workspace group (explicit members or patterns like
  `github.com/acme/*`) can carry the same settings. Per stage, a
  workspace's own setting wins, else its group's. Two groups may not set
  the same stage, or both set the agents default, for a shared workspace
  (the save is refused; if membership changes make it happen anyway, the
  first group by name wins and the dialog reports the conflict until you
  fix it). Unregistering a workspace drops its settings.
- **Who may change them.** A workspace's: approver keys whose scope covers
  it (and root). A group's: root. Any key in scope may read them (the
  dialog is read-only for other keys). Every change is audit-logged
  (`workspace_pipeline`) with what changed.
- **When a change applies.** At the next queued stage. Runs already queued
  or running keep their runner.
- **The old flags.** `--run-stage`, `--stage-policy` and `--agents-default`
  are imported once at startup (stages a workspace has not stored yet;
  stored settings win, a differing flag only logs) and will be removed in
  the next release.

The [pipeline tutorial](guides/pipeline.md) walks through a complete setup.

#### Multi-agent review and QA

Optional. A review or QA stage may run on several agents at once, each with
its own **focus**. One runner per stage (the line above) is unchanged.

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage review \
  --runners security@claude,tests@opencode-glm,conventions@opencode-qwen --policy any-blocks
```

In the dashboard: Pipeline → Review → add each runner with its focus.

- **Members.** Each list entry is `[focus@]runner`. Every member is its own
  run, pinned to its runner; together they form a *group* that is decided
  once. A single entry with a focus (`review=security@claude`) is one
  focused reviewer, not a group.
- **Focus briefs** are what a member with a focus is told. They come from
  one of two places, and a brief on the server wins over a file of the same
  name:
  - **the repository,** `.skopos/focus/<name>.md` (optional `description:`
    frontmatter, then the brief). Workers read it at the approved base
    commit, never from the branch under review;
  - **the server,** held with the pipeline settings: see
    [Briefs on the server](#briefs-on-the-server).

  A focus with no brief in either place fails that member.
- **Findings** carry a severity: `blocking` fails a reviewer, `nit` is
  reported but never blocks.
- **Policies** (Decision in the dialog, `--policy` / `--early-exit` /
  `--timeout` on the CLI):
  - `any-blocks` (default): any blocking finding fails the group; waits for
    every member, so you get full feedback per round;
  - `majority`: fails when more than half of the members that finished fail;
  - `all-pass`: like `any-blocks`, never exits early;
  - `early-exit`: decide as soon as the outcome is certain and cancel the
    rest (saves tokens, gives partial feedback);
  - `timeout=<duration>` (default `60m`): at the deadline the group is
    decided with what finished; unclaimed or paused members are cancelled
    and named.
- **The decision** writes one `Review summary — round N` (or `QA summary`)
  section into the item: each member's result, blocking findings merged
  across reviewers with who found them, nits separately. A failed group
  blocks the item, as a failed review always has; a passing one continues
  to QA or `in_review`. Slack's blocked and branch-ready messages quote the
  summary.
- **Parallelism** comes from workers: a worker runs one job at a time, so
  members claimed by one worker run one after another.

#### Briefs on the server

A brief can be held on the server instead of committed to the repository:
for an experiment, for a pipeline you run only for a while, or to give one
brief to every repository of a group.

- **Dashboard:** Workspaces → **Pipeline** → **Briefs**. The tab lists the
  briefs held for the workspace, the ones it gets from a group (**Override
  here** saves a copy for the workspace), and every focus the stages use
  that has none (**Write here**).
- **CLI:**

  ```bash
  skopos workspace pipeline brief set --workspace github.com/you/repo --name e2e --file e2e.md
  skopos workspace pipeline brief list --workspace github.com/you/repo
  skopos workspace pipeline brief show --workspace github.com/you/repo --name e2e [--history]
  skopos workspace pipeline brief delete --workspace github.com/you/repo --name e2e
  skopos group pipeline brief set --group you --name security --file security.md
  ```
- **REST:** `GET`/`PUT`/`DELETE /api/workspaces/{id}/pipeline/briefs/{name}`
  and the same under `/api/groups/{id}`; `GET …/pipeline/briefs` lists them.

How it works:

- **Which brief a run gets:** the workspace's own, else its group's, else
  the repository's file. The run's log says which.
- **From the next run.** A brief reaches a run when a worker claims it, so a
  change needs no commit and no restart.
- **Who may change one:** an approver whose scope covers the workspace;
  root for a group. Any key with the workspace in scope may read them.
- **Every save is a revision.** The history says who saved which version
  when, and the audit log records each change with its size and hash.
  Removing a brief keeps its history.
- **Limits:** 16 KB, the name a focus can have (lowercase letters, digits,
  `-` and `_`). Two groups cannot hold a brief of the same name for a
  workspace both contain.
- **Workers** need a release that takes briefs from the server. An older
  one is given no runs of a workspace that holds a brief (its own or a
  group's), and shows as unhealthy for it with that reason.
- **Not in `skopos config export`:** the fleet file does not carry briefs.

#### QA: testing the change

Review reads the code; QA runs it. A QA run gets its own prompt: test the
change as the people who use it would, against the item and the approved
plan, and report every failed scenario as a finding with the steps to
repeat it. Set it like any stage:

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage qa --runners e2e@claude
```

- **Your instructions** are the member's focus brief, here `e2e`: which
  system to test on, where its credentials are, how to sign in, what to do
  when it is not there. It is `.skopos/focus/e2e.md` in the repository, or a
  [brief held on the server](#briefs-on-the-server).
- **Where QA runs.** At the head of the pushed branch, after the
  repository's `setup` commands:
  - with `checkout = "branch"`, in the clone itself, switched to that commit
    for the run. A system that runs from the clone runs the change, and the
    files git ignores (local configuration, credentials) are there. The
    clone goes back to its default branch afterwards and what the run
    changed in tracked files is discarded;
  - otherwise in a worktree of its own, which has none of the ignored files;
    a `setup` command can copy one in (`cp {clone}/tests/e2e/.env.e2e
    tests/e2e/.env.e2e`).
- **What QA can use** is what its stage gives it: by default reading files
  and `git diff`. A browser, a test runner or a service that creates test
  environments comes from the QA stage's tools and MCP servers
  ([stages](concepts/workspace-config.md#stages),
  [MCP servers](concepts/workspace-config.md#mcp-servers)), which need a
  contained stage.
- **When QA cannot test** (no system, no credentials, a missing tool,
  nothing says where to test) it does not fall back to reading the code. It
  stops with questions, the item waits for you as *blocked*, and your answer
  (**Retry** with the answer, `skopos inbox retry`, or a Slack reply) starts
  QA again on the same commit. The implementation is not run again.
- **The result** is a verdict, findings and the list of scenarios with what
  each did, in the item's `qa — <runner>` section. A blocking finding sends
  the item back like a review's.
- **Timeout:** `qa_timeout`, 30 minutes by default.
- **Credentials in the log.** List the repository's credential files in the
  worker's repo block, `redact_files = ["tests/e2e/.env.e2e"]`: the values
  of their password, secret, token and key lines, and a password inside a
  URL value, are replaced in every run log and in what QA reports (its
  summary, scenarios, findings and questions). The files are read in the
  clone before the run and again in the tree QA tests, after setup.
  Credentials that reach a run another way (an MCP tool's answer) are not
  known to the worker and are not replaced.

#### Multi-agent planning

Optional, like the above. A `plan` list runs several planners, each writing
a **candidate**:

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage plan --runners claude,opencode-glm,opencode-qwen
```

- **Candidates don't touch the plan.** Each planner writes a candidate
  revision; the plan itself is created and linked when the first planner
  starts, and stays empty until you approve one.
- **Same input.** The first planner pins the base commit; the others plan
  at that commit too, so candidates differ in judgement only.
- **The gate opens** once every planner is done (or at the group's
  timeout) with at least one candidate. If none produced a plan, the item
  is blocked with their reasons and questions.
- **You approve one.** The dashboard shows a card per candidate (steps,
  skills) with **Approve** and **Revise**; Slack lists them numbered
  (`approve 2`, `approve glm`, `revise 2: <notes>`); the CLI shows them in
  `skopos inbox plan` and asks which on `approve`. The approved candidate
  becomes the plan exactly (same step ids, re-verified hash) and the
  approval names it; its siblings are superseded.
- **Changes.** *Revise* one candidate: one planner (its runner) reworks it
  with your notes. *Re-plan all*: every planner runs again as round 2. Either
  way the old round's candidates can no longer be approved.

**Optional merge and judge.** Between the candidates and the gate:

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage synthesize --runners claude         # the merge
skopos workspace pipeline set --workspace github.com/you/repo --stage compare --runners opencode-qwen     # the judge
```

- `synthesize` writes one more candidate, `synthesis`, combining the
  planners' candidates. The originals stay approvable — merging can average
  away the one idea that made a candidate better.
- `compare` scores every candidate, the synthesis among them (correctness,
  scope, step granularity, tests, fit with AGENTS.md), lists risks and
  recommends one with reasons. It's written into the item as `Comparison`,
  shown at the gate and in Slack, and **never pre-selects** anything.
  Prefer a runner that wrote no candidate and is not the merge's: a judge
  favours its own style.
- Order: candidates → synthesize → compare → gate, each only when
  configured and only when the round has two or more candidates. The
  recommendation therefore covers every plan you can approve. Both are
  advisory: if one fails, the gate opens with the candidates as they are —
  after a failed merge the judge compares the planners' candidates.

`implement`, `compare` and `synthesize` take one runner; a list there keeps
the first runner and logs a warning.

**Read the plans side by side.** "Read the plans side by side", at the
approval gate and on the Plan step of the item preview, opens every plan of
the planning round in full, one column each: the candidates with their
questions, the synthesis and the comparison.
- Hide and show columns with the chips above them, to put two plans next to
  each other.
- **Approve** on a candidate's column approves that candidate, with the
  notes and answers typed in the preview.
- An item planned more than once has a round switcher: a re-plan after
  "request changes" starts a new round.
- It works for a single planner's plan too, and after the gate.

**Export the plans as Markdown.** One document with the item's title and
brief, then every section of the round as the item has it: each plan, its
questions, the comparison, the synthesis and your notes.
- **Dashboard:** "Download .md" and "Copy as Markdown" in the side-by-side
  view.
- **CLI:** `skopos inbox plans --id <item> [--round N] [--out plans.md]`.
- **REST:** `GET /api/inbox/{id}/plans?format=md`; without `format`, the
  same round as JSON, each plan rendered.

#### Per-item switch: panel or solo

The lists above define the repository's **panel**. Each item chooses
whether its plan, review and QA stages use it:

| Item setting | Plan, review, QA |
|---|---|
| `panel` | the repository's list (a group) |
| `solo` | one run on the list's **first** runner, without its focus brief |
| (empty) | the workspace default ("Items without their own choice get" in the Pipeline dialog, or its group's), `panel` when not set |

```bash
# panel only where an item asks for it
skopos workspace pipeline set --workspace github.com/you/repo --agents-default solo
```

- **Every interface sets it.** Dashboard: the *Agents* field in the capture
  and edit forms (a badge shows it on the item). Slack: `#panel` or `#solo`
  in a `new:` capture, or reply `panel` / `solo` / `default` in the item's
  thread. CLI: `skopos inbox add|update --agents panel|solo|default`. REST
  and MCP: the `agents` field of create/update (`"default"` or `""` on
  update clears it).
- **When it applies.** It is read each time a stage is queued, so a change
  takes effect at the next plan, review or QA stage; a stage already running
  keeps its agents.
- **Who may change it.** Anyone who may edit the item, until it enters the
  agent workflow; from then on it is approver-only (it decides how many
  agents check the work) and audit-logged like any workflow edit.
- **Without a list** for a stage, `panel` has nothing to fan out to and the
  stage runs on its one runner.

#### Second opinions (on demand)

On an item at `in_review`, or blocked by its review, you can ask for a
**second opinion**: one extra review of the exact head waiting for your
approval, by a runner you pick. Ideally that's a different model family
from the reviewer.

- **Advisory.** The result is appended to the item as `Second opinion —
  <runner> @ <sha>`: the verdict, the summary, and blocking findings and nits.
  The item's status never changes; you decide whether to mark done, request
  changes or reject.
- **The exact head.** It reviews the newest successful implementation's head
  (the commit mark-done approves), not the branch tip. If you've requested
  changes since, it says it reviewed an older head.
- **Where you ask for one:**
  - **Dashboard:** the item preview has a **Second opinion** field and button.
    The runner field suggests your workers' runners and is prefilled with the
    default.
  - **Slack:** reply `second`, `second codex` or `second: codex` in the
    item's thread. The result is posted back to the thread.
  - **CLI:** `skopos inbox second-opinion --id <item> [--runner codex] [--force]`.
  - **REST:** `POST /api/inbox/{id}/second-opinion`.
  - **MCP:** agents can read the result (it's part of the item), but can't
    ask for one.
- **Where you read it.** The item preview shows a **Second opinion** step
  after Agent review, in every status once one was asked for:
  - one line per opinion, newest first: the runner, the commit and the
    verdict (`pass`, `would block: …`, or why it didn't finish);
  - **Watch live** while one runs and **Log** afterwards, for each;
  - every finding and summary under "Findings and summaries";
  - an opinion on a commit older than the one waiting for sign-off is
    marked "(older commit)".

  The step says whether anything from a second opinion is outstanding:
  - **amber** when a runner's newest opinion on the current commit would
    block or didn't finish (the sign-off card then says so too), or when a
    blocking finding of any second opinion is neither addressed nor waived;
  - **green** when one finished and nothing of it is left open, also for
    an opinion on an older commit whose findings were dealt with since;
  - a line sums up what became of its findings.
- **Default runner.** Set "Second opinions use" in a workspace's or group's
  Pipeline dialog, or `skopos workspace|group pipeline set --second-opinion
  <runner>` (`none` clears it). It resolves like the agents default: the
  workspace wins over its group. Without one, every request must name a
  runner.
- **Limits.**
  - Approver keys only.
  - One second opinion at a time per item.
  - Repeating a runner on a head it already reviewed needs `force`.
  - Every request is audit-logged (`inbox.second_opinion`).
- **The runner** needs a worker that offers it, like any pinned run. It runs
  with the repo's `review` profile, so a contained stage keeps its tools.

#### Review findings

Every finding of a review, a QA run, a review-only run or a second opinion
is a record with an id. What happens to it afterwards is recorded with it,
so a later round says which findings are new, which are confirmed fixed and
which are still open.

| State | Meaning |
|---|---|
| `open` | to be fixed; nobody says it is |
| `fixed` | the implementer says it is fixed; no review has confirmed it |
| `addressed` | a review checked it and it is fixed |
| `still_open` | a review checked it and it is not fixed |
| `waived` | you accepted it as it is |
| `noted` | recorded, not followed up |

- **What is followed up.** A blocking finding of a review that gates the
  item (review, QA, a review-only run) is followed from the start. A nit, a
  second opinion's finding and a finding from before this record are
  `noted` until you send them back.
- **The implementer** is given the followed-up findings that are `open` or
  `still_open`, and answers each: fixed, or not fixed and why. That is a
  claim, not a result.
- **The next review** is given the findings something is still owed on,
  with the implementer's answer, and says of each whether it is addressed
  or still open. Its own list holds only what nobody reported before. A
  blocking finding that is still open fails the review, like a new one.
- **Who checks what.** Each finding is checked by one reviewer.
  - A reviewer checks its own earlier findings: the same stage and, in a
    panel, the same focus. When two members review alike, the first of
    them checks it.
  - A finding no reviewer of the round answers for goes to the first
    reviewer: a second opinion's finding you sent back, or a focus that is
    no longer in the panel.
  - The other members see those findings as someone else's and don't
    report them again; an answer from them isn't recorded.
  - QA checks QA's findings, the review stage the rest.
  - What another member found in the same round isn't shown to the others,
    so each member's verdict is its own.
  - Another second opinion by the same runner answers that runner's
    earlier blocking findings, sent back or not, until one is addressed.
- **A review that passes** settles the findings that were its to check and
  that it didn't answer by name; the record says "not raised again". A
  review that fails settles only what it answers.
- **Two checks of one commit that disagree** leave the finding open: a
  second opinion calling it addressed doesn't undo a review that found it
  not fixed on the same commit.
- **Send findings back.** Request changes at sign-off, or retry a blocked
  item, with the ids of the findings to fix: from a review or a second
  opinion, whatever their state. Each is followed from then on.
  - **CLI:** `skopos inbox request-changes --id <item> --finding <id>`
    (repeatable; `--notes` is then optional), `skopos inbox retry --id
    <item> --finding <id>`.
  - **REST:** `findings: [ids]` in the body of
    `POST /api/inbox/{id}/request-changes` and `…/retry`.
  - An id the item doesn't have is refused before anything is queued.
- **Waive a finding** to accept it as it is. The implementer isn't asked to
  fix it, no review checks it or blocks on it, and reviewers are told you
  accepted it, with your note. Sending it back later undoes the waive.
  - **CLI:** `skopos inbox waive --id <item> --finding <id> [--note …]`.
  - **REST:** `POST /api/inbox/{id}/findings/{finding}/waive`.
  - Approver keys only; audited (`inbox.finding_waive`).
  - **A blocking finding needs a reason** (`--note`, `note`); a nit can be
    waived without one.
- **At sign-off** the audit entry of Mark done records the findings'
  state: how many are in each state, and by id the ones no review settled
  (to fix, still open, fixed but not confirmed) and the ones you waived.
- **Read the record.** `skopos inbox findings --id <item> [--state open]`,
  or `GET /api/inbox/{id}/findings`: every finding with its state and what
  happened to it. Any key with the item in scope can read it.
- **In the dashboard.** The item preview's action card lists the findings
  in groups: New (raised by the newest review), To fix, Still open, Fixed
  but not confirmed, Not followed up, Addressed and Waived.
  - Each shows who raised it, on which commit, and what happened to it.
  - **Tick** a finding to send it back with Request changes or Retry.
    Request changes then needs no notes. Findings to fix and still open
    go back without a tick.
  - **Waive** asks for a note and accepts the finding; a blocking finding
    needs the note.
  - The Agent review and Second opinion steps show each run's findings
    with their state, and the earlier findings that run checked again.
- **In Slack.** The "Branch ready" and "Blocked" messages list the findings
  something is still owed on, and the ones nobody follows up, numbered.
  - `findings` posts the list again.
  - `changes 2,3: <notes>` (at sign-off) and `retry 2: <answer>` (blocked)
    send findings back by number; `waive 2: <why>` accepts one.
  - The numbers are those of the list the relay showed last. After a
    server restart it asks you to reply `findings` first.
  - A reply by number that arrives mid-run isn't kept for later.
- **A finding nobody checks again** stays `fixed` (claimed, not confirmed):
  a second opinion's finding you sent back in a workspace that has QA but
  no review stage, say. You see it as such and decide.
- **Workers.** A worker older than this record keeps working: its reviews'
  findings are recorded, a passing review of its settles them, and nothing
  it runs answers one by name.

## Slack relay (server)

Enabled when all three `SKOPOS_SLACK_*` variables are set on `skopos serve`;
absent = disabled. Socket Mode (outbound websocket — no public URL needed).
The relay posts one thread per item (the `inbox_items.slack_thread_ts` column
persists the binding) on every gate (plan ready / blocked / branch ready /
done) in the bot's DM with the configured user, and applies that user's DM
replies onto the approver-gated workflow actions, audit-logged `via: slack`.
Only direct messages from `SKOPOS_SLACK_USER_ID` are read — other users and
channels the app can see are ignored and cannot change where messages go.
The Slack app needs the `chat:write`, `reactions:write` and `im:history` bot
scopes and the `message.im` event subscription.

| Env var | Meaning |
|---------|---------|
| `SKOPOS_SLACK_BOT_TOKEN` | Bot token (`xoxb-…`) — posting, reactions |
| `SKOPOS_SLACK_APP_TOKEN` | App-level token (`xapp-…`) — Socket Mode |
| `SKOPOS_SLACK_USER_ID` | The only user whose replies are honored |
| `SKOPOS_SLACK_DEFAULT_WORKSPACE` | Workspace for `new: <what>` capture (unset disables capture) |

Commands (replies in an item's thread): `approve` [`: <notes>`], `revise:
<notes>`, `changes: <notes>`, `done`, `retry: <answer>`, `reject`,
`second` / `second <runner>` (an advisory [second opinion](#second-opinions-on-demand),
`force` to repeat a runner on the same head),
`findings`, `waive <n>: <why>`, `changes <n,…>: <notes>` and `retry <n,…>:
<answer>` (the [review findings](#review-findings) by their number in the
list, applied at once for `findings` and `waive`), and
`panel` / `solo` / `default` (the item's [agents switch](#per-item-switch-panel-or-solo),
applied at once, even mid-run). A command is
the bare word or `word: <text>` on its own — "done with lunch" is not `done`
(a reply that leads with a command word gets a syntax hint instead).
Top-level: `new: <what to do>` or `new <workspace>: <what>` (exact id or a
unique `/suffix`), `help`; `#panel` or `#solo` in a capture sets the item's
agents switch (other hashtags become tags). Non-approving replies (`revise`, `changes`,
`retry`, `reject`) that arrive mid-run are buffered and applied when the run
finishes (in-memory; a restart drops unapplied ones); `approve` and `done`
are refused mid-run — they answer the gate message they approve.

## Agent identity (all surfaces)

These environment variables affect how agents identify themselves — to the
server (MCP/REST) and to CLI commands:

| Env var | Default | Description |
|---------|---------|-------------|
| `SKOPOS_SESSION_ID` | auto-derived (per workspace per day, or `.skopos-session` file in the workspace root) | Share one session across multiple agents working on the same task |
| `SKOPOS_AGENT_ID` | `<command>-<hostname>` | Stable agent id for `skopos report` and hook scripts |

## Log levels

```bash
skopos serve --log-level debug      # see every HTTP request
skopos serve --log-level trace      # maximum verbosity
LOG_LEVEL=debug skopos serve        # via env var
```

With `debug`, every HTTP request is logged (method, path, status, duration) through the single paularlott logger. All log output is consistent (one format, one writer).

At any level, a request that takes over a second is logged as a `slow http request` warning, with the database connection pool's state:

- `db_in_use` / `db_open` / `db_max_open`: connections busy when the request finished, open, and the cap (the number of CPUs, at least 4).
- `db_pool_waits` / `db_pool_wait`: how often, and for how long, requests waited for a free connection while this one ran (pool-wide, so concurrent requests count).

A slow request with pool waits means the pool was exhausted. A slow request without them points at SQLite's write lock (each write waits up to 5s for it) or at work outside the database. A `slow cleanup` warning at the same time names the retention cleanup as what held the lock. Streams (SSE, MCP streaming) are never logged as slow.

## Log output

Besides stdout, `serve` and `worker` can send their logs to an HTTP log
intake, in batches (every 2 seconds, or 100 lines). It works the way knot
ships its logs: the same flags, the same formats.

| Format | Takes it |
|---|---|
| `ndjson` (default) | VictoriaLogs (`http://host:9428/insert/jsonline`; skopos adds the field-mapping parameters), Vector's `http_server` source, OpenObserve |
| `loki` | Loki and Grafana Alloy (`/loki/api/v1/push`), VictoriaLogs (`/insert/loki/api/v1/push`), a knot space's agent (`http://localhost:12201/loki/api/v1/push`) |
| `elasticsearch` | Elasticsearch, OpenSearch, Quickwit (`/_bulk`) |

```bash
skopos serve --log-output-url http://victorialogs:9428/insert/jsonline --log-output-stream skopos-server
```

- **With an output set, stdout is JSON too.** The records are slog's
  (`time`, `level`, `msg` and attributes).
- **Workers send their run logs too.** Every line a run's agent, setup and
  checks print goes out with `run_id`, `item_id`, `workspace`, `stage` and
  `src`; filter on `run_id` to follow one run
  ([Run logs](deployment/worker.md#run-logs)).
- **A sink that is down never blocks.** Lines stay buffered (at most
  10,000; the oldest go first) and are retried. A `4xx` other than `429`
  drops the batch. Failures are reported on stderr at most once a minute.
- **Credentials:** a sink behind a proxy takes `LOG_OUTPUT_HEADER`, e.g.
  `Authorization: Basic …`. On a worker, put it in `skopos-worker.env`; agent
  runs never see `LOG_OUTPUT_URL` or `LOG_OUTPUT_HEADER`.

## SRV URLs

Any URL skopos works with can name a DNS SRV record instead of a host and
port: write `srv+http://name/path` or `srv+https://name/path` (knot's
convention). `name` is looked up as an SRV record, such as Consul's
`skopos.service.consul` or `_http._tcp.example.com`, and the records give
the address and port.

```bash
skopos worker --server-url srv+http://skopos.service.consul
LOG_OUTPUT_URL=srv+http://victorialogs.service.consul/insert/jsonline
```

- **Where it works:** the server URL (`--server-url`, `SKOPOS_SERVER_URL`,
  `[client].server_url`) for the worker, the CLI and the hooks; the log
  output; the embeddings and Qdrant URLs; and a repo's `push.remote_url`
  (pushes and pull requests).
- **Followed live:** each new connection looks the record up again, so a
  service rescheduled onto another port is found without a restart.
  Targets are tried in RFC 2782 order (priority, then weight), so a dead
  one is skipped.
- **TLS:** with `srv+https`, the request keeps `name` as its host, so the
  certificate must be for `name`.
- **DNS server:** where `.consul` isn't in the system resolver, point
  skopos at Consul's DNS with `--dns-server 127.0.0.1:8600` (or
  `SKOPOS_DNS_SERVER`, in a worker's `skopos-worker.env` too).
- **Programs that can't look SRV up get a resolved URL:**
  - the agents' MCP config: the worker writes it with the resolved
    address and rewrites it before each run;
  - `git push`: resolved for each push;
  - `skopos install`: agent configs get the URL resolved at install time,
    so re-run it if the service moves; skopos's own client config keeps
    `srv+`.

  A resolved `http` URL uses the target's address; a resolved `https` URL
  keeps `name` with the record's port, for TLS.

## Authentication

When `api_key` is set, every endpoint (reads, writes, MCP, SSE) requires the key via:

```
Authorization: Bearer mysecret
```

When empty (default), authentication is disabled — all endpoints are open. A startup warning is logged.

## Database

SQLite with WAL journal mode, foreign keys enforced per-connection (via DSN pragma), and a bounded connection pool. Migrations run automatically on every `serve` (idempotent `CREATE TABLE IF NOT EXISTS`). The schema lives in `internal/db/schema.sql`.

## API keys

Besides the root `api_key`, the server supports scoped API keys stored in
the database — see [concepts/api-keys.md](concepts/api-keys.md). The root
key stays in configuration and is the only key that can mint, edit, revoke,
or delete others; generate a strong one with `skopos key generate-root`.
`last_used_at` is updated at most once per 5 minutes per key.

## MCP lean tools

`[mcp] lean_tools = true` (or `--mcp-lean-tools` / `SKOPOS_MCP_LEAN_TOOLS`)
hides non-core MCP tools from `tools/list`, keeping only the workhorses
natively visible (code_search, code_find, code_symbol, code_callers,
code_impact, skopos_context, blackboard_read/write, report_status). Hidden
tools remain fully callable via the `tool_search` / `execute_tool`
meta-tools the server exposes automatically. This roughly halves the
per-step schema weight agents re-send every turn — the measured step-level
token cost in the index-effectiveness benchmarks. Default: off (all tools
listed).
