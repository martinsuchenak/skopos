# Deploying a worker

A worker is the execution plane: it polls skopos for run stages
(plan / implement / review / qa) and executes them with an agent CLI against a
local clone. It is the same binary as the server (`skopos worker`) and needs:

- skopos over HTTP (URL + a worker-scoped API key)
- a local clone per served workspace (with a push token if it should push)
- one or more agent CLIs: **Claude Code**, **opencode** and **Codex**
  (read-only stages) are built in; any other CLI can run inside a wrapper you
  provide

Run one worker per VM, one key per worker. For the whole setup — server, keys,
repos, workers, first item — follow the
[pipeline tutorial](../guides/pipeline.md); this page is the worker reference.

## Quick start

One command, after minting the worker key (step 2 below):

```bash
curl -fsSL https://github.com/martinsuchenak/skopos/releases/latest/download/install.sh | sh -s -- \
  --worker --server-url https://skopos.example.com --workspace github.com/you/repo
```

It installs the binary into `~/.local/bin`, then runs `skopos worker init`
with your terminal as input: it asks for the worker key (typed hidden), the
clone (offering to clone it), whether to push to GitHub with draft pull
requests (and the token), then the runner wizard. Then `worker check`, and
when that passes, `worker install` starts the service. Add `--no-service` to
stop before the service, `--repo PATH`, `--clone`, `--push
github|gitea|none` to answer up front.

**Unattended** (cloud-init, Ansible, no terminal): the same command takes
the secrets from the environment, never from arguments: `SKOPOS_API_KEY`
(the worker key), `GH_TOKEN` (the push token; `--push-token-env` on `worker
init` names another), and `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`
for the claude account (without either, it uses the machine's own Claude
login). They go into `skopos-worker.env` (0600); the config is
`~/.config/skopos/skopos-config.toml`. Run it again later and it keeps an
existing `[worker]` section: it upgrades the binary and re-checks.

Step by step instead:

```bash
# 0. Install the binary into ~/.local/bin (run again to upgrade; the worker
#    also upgrades itself to the server's release from there)
curl -fsSL https://github.com/martinsuchenak/skopos/releases/latest/download/install.sh | sh

# 1. Scaffold the config (annotated [worker] section + skopos-worker.env).
#    In a terminal it asks for the key, the clone and the push, then offers
#    the runner wizard. --repo PATH, --clone and --push github|gitea|none
#    answer up front; SKOPOS_API_KEY, GH_TOKEN and a Claude credential are
#    taken from the environment.
skopos worker init --server-url https://skopos.example.com \
                   --workspace github.com/you/repo

# 2. Mint a worker key (root key, against the server) and put it in
#    skopos-worker.env as SKOPOS_API_KEY=...
skopos key create --name worker-vm1 --workspace github.com/you/repo \
                  --server-url https://skopos.example.com --api-key <root-key>

# 3. Fill in the repos blocks in skopos-config.toml (clone path, checks, push)

# 4. Agent runners and their accounts — wizard, then a real smoke test
skopos worker runner add
skopos worker runner test

# 5. Validate everything the service will see
skopos worker check

# 6. Install and start the service (launchd on macOS, systemd on Linux)
skopos worker install
```

## Agent runners

An account's `runner` picks the agent CLI; the account also carries the
model, the effort and the credentials.

| Runner kind | Lockdown | How it runs |
|---|---|---|
| `claude` (default) | verified | Claude Code `-p`; structured results via `--json-schema` |
| `opencode` | verified | `opencode run --format json`; results extracted from the reply |
| `codex` | verified, **read-only stages** | `codex exec --json`; structured results via `--output-schema`. Plan, review, compare and second opinions; it refuses implement |
| `antigravity` | **unverified** | Antigravity CLI (`agy --print`, Gemini); structured results via `--json-schema` — runs **only inside a wrapper** (see [Antigravity](#antigravity-agy)) |
| `command` | **unverified** | any CLI from an argv template — runs **only inside a wrapper** |

**Verified** means skopos has checked the lockdown live (the opt-in tests
`SKOPOS_LIVE_CLAUDE=1` / `SKOPOS_LIVE_OPENCODE=1` / `SKOPOS_LIVE_CODEX=1`):

- instruction files are not loaded;
- files outside the worktree are unreadable;
- the stage's tool allowlist is enforced, and commits still work (Codex:
  nothing is writable, which is why it only takes read-only stages).

skopos can't vouch for an arbitrary CLI, so a `command` (or `antigravity`)
runner on a stage without a wrapper refuses the run with a clear reason. `worker check` lists
the stages that have no wrapper.

### Adding runners: the wizard

```sh
skopos worker runner add      # wizard
skopos worker runner list     # runners, CLI versions, lockdown status, accounts, what claims offer
skopos worker runner test     # one real prompt per account, through the locked-down runner
```

The wizard handles the whole setup:

- **Detects CLIs.** It finds the agent CLIs on the machine (claude, opencode,
  codex, gemini, agy, aider) and shows the models opencode knows.
- **Names the runner.** It suggests a name, for example `opencode-glm-5-3-flash`.
- **Sets up credentials.**
  - **Claude:** this machine's login, an API key kept in the env file, a
    separate login directory for a second subscription account, or a
    subscription token kept in the env file (`CLAUDE_CODE_OAUTH_TOKEN`,
    created once with `claude setup-token`; no login directory needed).
  - **Codex:** a login directory of its own (the default; see
    [Codex](#codex)), an API key kept in the env file, or this machine's
    `~/.codex`.
  - **Custom opencode providers** (an OpenAI-compatible router, say): it
    writes a providers-only config. After you confirm, it moves the API key
    into the env file; the providers file only references it as `{env:VAR}`.
- **Edits safely.** It previews exactly what it adds and validates the edited
  config before saving. A failed edit changes nothing.
- **Smoke-tests.** It finishes with an optional live test.

For scripts:

```sh
skopos worker runner add --kind opencode --name opencode-glm \
  --model zai-coding-plan/glm-5.3 --account glm --probe
```

`runner test` also fails an account whose `${VAR}` env reference resolved to
empty. Otherwise the CLI would quietly fall back to its own login and pass on
the wrong credentials.

### What the config looks like

```toml
[worker.runner_config.opencode-qwen]
kind   = "opencode"
config = "~/.config/skopos/opencode-providers.json"   # providers/models only; ~/.config/opencode is never read

[[worker.accounts]]
name   = "qwen"
runner = "opencode-qwen"
model  = "llmrouter/qwen/qwen3.8-27b"
env    = { LLMROUTER_API_KEY = "${LLMROUTER_API_KEY}" }  # the providers file says {env:LLMROUTER_API_KEY}

[worker.runner_config.aider]                          # any other CLI: wrapper-only
kind    = "command"
command = ["aider", "--yes", "--message", "{prompt}"] # {prompt} {dir} {model} {effort}; no {prompt} = stdin
```

`worker.runners` (what claims offer) defaults to every runner your accounts
use. Built-in names (`claude`, `opencode`, `codex`) need no `runner_config`
block.

### Codex

Codex takes the read-only stages: plan, review, compare and second
opinions (a different model family is the point of a second opinion). It
refuses implement, because its sandbox keeps `.git` read-only and an
implementer couldn't commit. An implement run with no runner preference
never falls back to a Codex account: when every Claude account is at its
limit, it pauses until the first one resets.

Give each Codex account a login directory of its own:

```bash
CODEX_HOME=~/skopos-workers/accounts/codex-1 codex login   # ChatGPT subscription
```

```toml
[[worker.accounts]]
name   = "codex-1"
runner = "codex"
env    = { CODEX_HOME = "~/skopos-workers/accounts/codex-1" }   # or CODEX_API_KEY = "${CODEX_KEY}"
```

- **No AGENTS.md in the login directory.** Codex always loads
  `$CODEX_HOME/AGENTS.md` and has no switch for it, so a worker refuses an
  account whose login directory holds one (or `AGENTS.override.md`), and
  `worker check` reports it. A personal `~/.codex` often has one; that's why
  the wizard suggests a separate directory.
- **Sessions** are kept in the login directory, so an interrupted run
  resumes on its own worker and account.
- **Usage limits** pause the run and mark the account until the reset Codex
  reports.
- **Verified on macOS.** The lockdown rests on Codex's sandbox (Seatbelt on
  macOS, its Linux sandbox elsewhere). Run
  `SKOPOS_LIVE_CODEX=1 go test ./internal/worker -run 'Codex.*Lockdown'` on a
  Linux worker before relying on it there; `TestCodexLockdownProbe` spends
  no tokens.

### Antigravity (agy)

The Gemini CLI is decommissioned, so Gemini models come through the
Antigravity CLI. The runner is **wrapper-only**: it runs on stages a
wrapper contains (the Knot space, a container), never bare. A spike
against agy 1.1.25 found why:

- **Instruction files load.** agy always loads the working directory's
  `AGENTS.md` and `GEMINI.md`, with no switch to skip them. (Review
  worktrees reset them to the base version, as for every runner.)
- **The allowlist lives in a settings file.** In headless mode agy denies
  every tool that would need a permission prompt: reads outside the
  workspace, writes, and shell commands without an allow rule. Allow rules
  can only come from `$HOME/.gemini/antigravity-cli/settings.json`.
- **A denied tool ends the run.** The first denial stops the run with no
  output, so a reviewer that reaches for a command the stage doesn't allow
  fails instead of carrying on.

What the runner does:

- **HOME is the account's login directory.** agy keeps its sign-in,
  settings and conversations under `~/.gemini`, and nothing moves them. The
  account must set `HOME`; skopos never writes the worker user's own
  `~/.gemini`.
- **Each run writes its allow rules** into that directory's settings: the
  stage's `Bash(...)` patterns become `command(...)` rules, the hard denies
  stay denied, and reads inside the worktree need no rule.
- **Flags:** `--sandbox`, `--disable-slash-commands` (no skill expansion),
  `--json-schema` for the result, `--conversation` to resume, and
  `--print-timeout` set to the stage timeout (agy's default is 5 minutes).
  Never `--dangerously-skip-permissions`.

```bash
HOME=~/skopos-workers/accounts/agy-1 agy        # sign in once
```

```toml
[worker.runner_config.agy]
kind = "antigravity"

[[worker.accounts]]
name   = "agy-1"
runner = "agy"
model  = "gemini-3.8-flash-high"                  # agy models
env    = { HOME = "~/skopos-workers/accounts/agy-1" }
```

- **One login directory per worker.** Runs rewrite its settings, so two
  workers must not share one.
- **No `GEMINI.md` or `AGENTS.md` in its `.gemini`.** A worker refuses an
  account whose login directory has them, and `worker check` reports it.
- **Live test:** `SKOPOS_LIVE_ANTIGRAVITY=1 SKOPOS_LIVE_ANTIGRAVITY_HOME=<login dir>
  go test ./internal/worker -run TestLiveAntigravityLockdown -v` checks what
  holds and logs the instruction-file gap.

### Effort

An account's `effort` is the reasoning effort its CLI runs at (empty = the
CLI's default). Each runner kind passes it its own way, and the worker
refuses a value the CLI doesn't take when it loads the config:

| Runner kind | Passed as | Values |
|---|---|---|
| `claude` | `--effort` | `low`, `medium`, `high`, `xhigh`, `max` |
| `antigravity` | `--effort` | `low`, `medium`, `high`, `xhigh`, `max` |
| `codex` | `-c model_reasoning_effort=…` | `minimal`, `low`, `medium`, `high`, `xhigh` |
| `opencode` | `--variant` | the model's variants (provider-specific, e.g. `high`, `max`, `minimal`) |
| `command` | `{effort}` in the template | any word; refused when the template has no `{effort}`, and a template with `{effort}` needs it on every account |

```toml
[worker.runner_config.claude-opus-high]
kind = "claude"

[[worker.accounts]]
name   = "opus-high"
runner = "claude-opus-high"
model  = "opus"
effort = "high"
```

`skopos worker runner add --kind claude --name claude-opus-high --model opus --effort high`
writes the same; the wizard asks for the effort after the model. `runner
list`, `runner test` and the run log show it next to the model.

- **The CLI must know the flag.** When an account sets effort, `worker
  check` and worker startup also check the CLI's help for `--effort`
  (claude, agy) or `--variant` (opencode). An older CLI is refused there
  rather than failing every run.
- **Keep one setting per runner.** Stages pin runners, and the pool rotates
  between a runner's accounts on quota. Accounts of one runner that differ
  in model or effort get a warning (`runner list`, the worker's log at
  startup), since a quota switch would change how the stage runs.

### One model per stage

The model and the effort belong to the account, and a runner name is any
label. To run one CLI with different models or efforts, give each its own
runner name. The wizard does this when you pick a model or an effort. Then
pin stages to runners in the workspace's pipeline settings (dashboard:
Workspaces → Pipeline):

```sh
skopos workspace pipeline set --workspace github.com/you/repo --stage plan --runners opencode-glm-5-3
skopos workspace pipeline set --workspace github.com/you/repo --stage implement --runners claude
skopos workspace pipeline set --workspace github.com/you/repo --stage review --runners opencode-qwen3-8-27b
```

- **A pinned run uses only its own runner.** When that runner's accounts are
  quota-exhausted, the run pauses until the earliest reset. It never borrows
  another runner's account.
- **Claims follow availability.** A worker only offers runners that have a
  usable account right now. A quota pause doesn't stall the worker while
  other accounts can still work.
- **Misconfiguration fails clearly.** A preference naming a runner no account
  feeds fails the run and says why.
- **Unpinned runs are flexible.** Runs without a stage preference may use any
  available account.
- **Review and QA are opt-in.** They only run when the workspace has a
  preference for them. Otherwise a finished implementation goes straight to
  `in_review` for you.

## What every agent run gets

These hold for every runner, regardless of configuration.

- **No user or project instructions or settings.**
  - **Claude:** gets `--setting-sources "" --restricted --strict-mcp-config`
    and a `--tools` list derived from the stage allowlist. So it loads no
    worktree `CLAUDE.md`, no `~/.claude/CLAUDE.md`, none of the worker user's
    allow rules, hooks or MCP servers, and its file tools stay inside the
    working directory.
  - **opencode:** runs with an empty config dir and its `OPENCODE_DISABLE_*`
    switches. Its permissions come from an ordered `OPENCODE_PERMISSION`
    translated from the same allowlist: deny-all, then the stage's allows,
    then the hard denies, including `external_directory`.
  - **Codex:** `codex exec --ignore-user-config --ignore-rules
    --strict-config`, every setting passed with `-c`: no AGENTS.md
    (`project_doc_max_bytes=0`), no skills listing, plugins, apps or hooks,
    approvals `never`. Its permission profile lets the shell read only the
    worktree, the clone's git directory and what a shell needs (not the
    temp directories); nothing is writable and the network is off. The
    stage allowlist becomes that read-only sandbox, since Codex reads files
    through its shell.
  - **Instructions:** reach the agent only through the stage prompt; see
    [Repo knowledge](#repo-knowledge-agentsmd-skills-verification).
- **Stage allowlists.** Plan is read-only; implement can edit and run a short
  list of git and read commands; review can read and diff. The prompts name
  the exact shell commands the stage allows and the checks the worker runs
  afterwards, and tell the agent not to chain commands. A profile can grant
  more tools (see [Containing agent runs](#containing-agent-runs)).
- **MCP servers only from `mcp_config`** (plus skopos itself, with an agent
  key: see [Skopos tools for agent runs](#skopos-tools-for-agent-runs)). Put
  the servers runs may use in a JSON file (`{"mcpServers": {...}}`, Claude
  Code's shape; opencode gets it translated), point `worker.mcp_config` at
  it, and allowlist their tool prefixes in `mcp_servers`.
- **Scrubbed environment.** Agent runs, checks and `setup` commands get no
  push tokens, no skopos key, no SSH agent and nothing from the env file:
  - an account's `env` (with `${VAR}` references) passes credentials to the
    agent;
  - `setup_env` passes what setup needs.
- **The right working directory.** Every agent command gets `PWD` set to its
  run directory, and opencode also gets `--dir`, because opencode trusts
  `$PWD`.
- **Review worktrees reset instruction files.** Before a review or QA run,
  `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `.skopos/`, `.claude/`, `.opencode/`,
  `.codex/`, `.agents/`, `opencode.json` and similar files are reset to the base version, so no CLI
  picks up instructions the implementer wrote. Those changes still show in
  the reviewed diff.
- **Only the worker pushes.** It validates the refspec, keeps the token in its
  own environment, and never force-pushes.

The daemon refuses to start, and `worker check` reports it, when a runner's
CLI can't do its lockdown (for example, an older Claude Code without
`--restricted`).

## Skopos tools for agent runs

With an **agent key**, claude and opencode runs reach skopos over MCP: the
code index, the blackboard, and their plan's steps.

1. Mint an MCP-only key for the worker's workspaces (root key):

   ```sh
   skopos key create --name knot-1-agent --workspace github.com/you/repo --preset agent
   ```

2. Put it in `skopos-worker.env` next to the worker key:

   ```bash
   SKOPOS_AGENT_KEY=sk_...
   ```

At startup the worker checks the key with `whoami` and refuses to start
when it could give an agent more than MCP: full access, the `rest`
surface, the approver permission, or no `code` read. It writes the run MCP
config to `<state_dir>/agent-mcp.json` (mode 0600): your `mcp_config`
servers plus a `skopos` server with the agent key. Don't define a server
named `skopos` in `mcp_config` yourself; the worker refuses that too.

What a run gets:

- **Tools:** every stage may call `skopos_context`, `blackboard_read`,
  `blackboard_write`, `plan_read` and the `code_*` tools. Implement may also
  call `plan_update_item`.
- **Prompt:** plan, implement and review prompts get a Skopos section. It
  tells the agent to read the blackboard for the workspace and branch first,
  and to record decisions and bugs as `<runner>@<run id>`. Implement also
  gets its steps' plan item ids, to mark each one done.
- **Code index guidance**, only when the workspace's code index switch is on
  and the server holds a usable index. The worker asks the server which
  branches are indexed, at which commit, and names one branch:
  - plan: the default branch, which may lag its newest commits;
  - implement: the default branch, noting that the agent's own changes are
    not in it;
  - review and QA: the item branch when its index is at the exact commit
    under review, otherwise the default branch, and to use the diff for
    what changed.
- **Server instructions:** a capped key's MCP `initialize` answer describes
  only the tools it may call, not the full skopos instructions. A capped key
  sends one request at a time (no JSON-RPC batches).
- **Codex** runs get no skopos tools (their network is off).
- The agent key never reaches the agent's environment: the scrub drops
  every `*_KEY` variable.

Without an agent key the worker logs a warning at startup, and runs get only
your `mcp_config` servers.

## Code indexing while idle

A workspace's pipeline settings have a switch, **Keep the code index
fresh** (`code_index`; dashboard or `PUT /api/workspaces/{id}/pipeline`).
With it on, idle workers keep that workspace's index current, so agents and
reviewers query the code as it is:

- **Who can:** every claim reports the workspaces this worker can index: the
  repo is configured and present, its preflight passes, and its last index
  fetch did not fail in the past 15 minutes. The switch can be turned on
  only while some worker can.
- **Who does:** the server gives each workspace's default branch to one able,
  idle worker, the one with the smallest id, so three workers on one repo
  don't all parse it. A worker busy with a run can't index, so for the length
  of the run the duty moves to an idle one. Each worker also indexes the item
  branches it implemented there.
- **When:** only while idle. A claimed run stops indexing at once, and what
  was parsed so far stays in the parse cache (`<state_dir>/index-cache`), so
  the next idle moment carries on from there. The worker checks an assigned
  default branch every 5 minutes and pushes only when origin moved. It drops
  an item branch's index when the sweep retires the branch.
- **Item branches** are indexed at the end of their implement run, after the
  push and before it reports, so the review that follows queries the code it
  reviews. A branch blocked by its checks is never reviewed, so it isn't
  indexed. This is
  the one index build that doesn't wait for an idle moment. It's bounded at
  5 minutes (a cold parse cache on a large repo), and a failure or timeout
  leaves the branch for the next idle moment; it never fails the run.
- **Where:** in a detached worktree of its own per repo
  (`<state_dir>/index/<repo>-<hash>`), never in the clone a run uses. Fetch
  and checkout take the clone's lock only when it is free; a clone another
  process holds just waits for the next check.
- **Stale branches:** every 6 hours the default-branch worker compares the
  server's indexed branches with origin's (`git ls-remote`). It drops the
  index of each branch that is gone from origin and was last built more than
  7 days ago, whoever pushed it: a worker, or someone running `skopos index
  push`. The 7 days protect a branch someone indexed before pushing it. The
  default branch is never dropped, and an unreachable origin drops nothing.
  The server's daily GC then frees what no branch references.
- **Cost:** after the first build only changed files are parsed. A failed
  fetch or build backs off for 15 minutes.

The worker pushes with its own key, which needs `code:write`.

## Pushing and pull requests

Each repo's `push` block decides what happens to a finished implementation:

```toml
[worker.repos."github.com/you/repo".push]
kind         = "github"                          # github | gitea | none (required)
remote_url   = "https://github.com/you/repo.git"
token_env    = "REPO_PUSH_TOKEN"                 # the value lives in skopos-worker.env
pull_request = "draft"                           # draft | ready | none (default none)
```

- **No push block:** the branch stays on the worker. The dashboard says so
  and shows no link, and reviews and second opinions can't fetch it.
- **Push:** the worker pushes the branch after the checks run.
- **`pull_request`:** after the push, the worker opens a pull request into
  the default branch, or reuses the open one for that branch, since
  change-request rounds push to the same branch. Its link appears in the
  item preview. On GitHub, `draft` opens a draft; Gitea has no draft flag, so
  the title starts with `WIP:`. If the pull request can't be opened, the run
  still succeeds and its summary says why. The token then also needs
  pull-request write access: for a fine-grained GitHub token, **Contents** and
  **Pull requests**, both read and write.
- **The worker never merges.** Mark done records your sign-off; merging the
  pull request (or the branch) is yours.

**Cleanup.** When idle, every 5 minutes (at most 2 minutes per pass), the
worker looks up the items it implemented:

- **Done:** it removes the item's worktree, and the local branch too if the
  last round pushed it. An unpushed branch is the only copy of the work, so
  it stays. The pull request stays for you to merge.
- **Rejected:** a run still in progress stops at its next heartbeat (every
  30s): skopos answers 410, and the worker ends the run's context, which
  kills the agent's process group. Nothing more is pushed or reported, and a
  branch-checkout clone goes back to its default branch (uncommitted work
  stashed under the branch). After a day, because Reject can be undone, it closes the pull
  request without merging and cleans up as for done. A pull request someone
  else opened, on a branch the item took over, is never closed.
- **Not found:** skopos answers 404 both for a deleted item and for one
  outside the worker key's scope, so the worker never closes that item's pull
  request or deletes its branch. After a week, it removes the worktree.

A new round for an item whose worktree is gone, such as a rejected item that
was restored, continues the item's branch. It uses the local branch, or the
one on the remote, so the push stays fast-forward. A local branch behind
the remote's (another worker pushed a later round) is fast-forwarded first.
When both have moved on — an interrupted round left commits on this worker
while another worker pushed its own — the worker pulls, as a developer
would: it merges the remote's branch into the local one and carries on from
there. If they don't merge cleanly the run fails, naming the conflicting
files; keep one of the two (push the one you want and delete the other, or
delete the worker's local branch to continue from the remote's) and retry.
A pull request skopos opened and someone closed without merging is reopened
rather than duplicated.

## Branch names and existing branches

An item works on the branch named in its **Branch** field, or else on the
name the repo's template gives it:

```toml
[worker.repos."github.com/you/repo"]
branch_prefix   = "WR"              # the worker only pushes branches starting with this
branch_template = "{ref}-{slug}"    # default "{prefix}{tail}-{slug}"
```

- **Placeholders:** `{prefix}` (`branch_prefix`), `{tail}` (the item id's
  tail), `{slug}` (the title, lowercase with dashes, at most 30 characters)
  and `{ref}` (the item's **Reference** field, e.g. `WR203345`). A template
  needs `{tail}` or `{ref}`, so two items never share a branch.
- **A template with `{ref}`** stops an item without a reference at planning
  with "set the item's reference".
- **The name must start with `branch_prefix`** and match no protected
  pattern, whether it comes from the template or the Branch field: the plan
  stage checks this before any work.

**Existing branches.** When the planner starts, the worker looks on origin:

- **The branch exists** (the template's name, or the one in the Branch
  field): the item takes it over, as a *handover*. The plan is made against
  the branch head, which becomes the approved base, and the planner gets the
  branch's commits and diff since it left the default branch. The implementer
  continues on the branch. If the branch is rewritten after the approval
  (force-pushed), the implement run stops instead of continuing from
  something nobody approved. The review covers the whole branch and is told
  which commits were already there.
- **Other branches start with the template's text before `{slug}`**
  (`WR203345-api`, `WR203345-ui`) and none matches exactly: the item stops
  with a question listing them. Set its Branch field to the one to continue,
  or to a new name, and retry.
- **Nothing matches:** a new branch, as before.

Instructions, skills and focus briefs are always read from the default
branch's history (the commit the branch grew from), never from a branch
being taken over.

**Review only.** The dashboard's **Review branch** (on an open item with a
Branch or Reference) queues one `branch_review` run instead of a plan:

1. It finds the branch as above. A branch missing from origin blocks the
   item.
2. It runs setup and the repo's checks at the branch head, read-only. With
   `checkout = "branch"` the clone is detached at the head, and setup is
   skipped when the clone is already set up for that branch at that head.
   Afterwards, whatever the checks changed is thrown away and the clone goes
   back to its default branch. There are no fix rounds, and nothing is
   committed or pushed.
3. The branch is reviewed the way the workspace reviews an implementation
   (its review stage under Workspaces → Pipeline): one runner, one focused
   reviewer, or the review panel, each member with its focus brief and the
   panel decided by its policy into one summary. An item set to *one agent*
   gets the panel's first member without its focus. Reviewers review against
   the item's text, with failed checks as blocking findings. Panel members
   share the checks: a member that finds results already reported for the
   head reuses them instead of running setup again.

The item then comes to you whatever the verdict (a panel blocks the item
only when no member managed to review). **Mark done** accepts it.
**Request changes** turns it into a handover: it is planned from the review
and your requests, then implemented on the same branch. **Retry** on a
blocked review runs the review again.

Records for this live in `<state_dir>/branches/`. Worktrees left from before
this release have no record, so remove them by hand (`git worktree list` in
the clone).

## Containing agent runs

skopos provides no isolation; it delegates. How a stage is contained is your
choice: a container, a VM, a sandbox tool, or Claude Code's own sandbox. You
declare it per stage in a profile:

```toml
[worker.profiles.php.implement]
wrapper = ["docker", "run", "--rm", "-i", "-v", "{dir}:{dir}", "-w", "{dir}",
           "-e", "ANTHROPIC_API_KEY", "ghcr.io/you/agent-php@sha256:..."]
tools   = ["Bash(php:*)", "Bash(composer:*)", "Bash(vendor/bin/phpstan:*)", "Bash(vendor/bin/phpunit:*)"]

[worker.profiles.php.review]
settings = '{"sandbox":{"enabled":true}}'   # Claude Code's sandbox (claude runner only)
tools    = ["Bash(vendor/bin/phpstan:*)", "Bash(vendor/bin/phpunit:*)"]

[worker.repos."github.com/you/repo"]
path    = "~/clones/repo"
profile = "php"
# [worker.repos."github.com/you/repo".stages.qa] overrides one stage
```

- **`wrapper`** is an argv prefix; the agent command is appended to it.
  - Placeholders: `{dir}` (the run's working directory), `{run}`, `{stage}`,
    `{workspace}`.
  - The wrapper process receives the scrubbed environment plus the stage's
    `env`, so forward what the agent needs (`-e NAME`).
  - The image must contain the agent CLI.
  - Session resumes need a persistent config dir (`CLAUDE_CONFIG_DIR`)
    mounted into the container.
  - The agent needs network access to its model API.
- **`settings`** is merged into a claude run's settings (skopos's deny list
  always stays). `sandbox.enabled = true` counts as containment **for the
  claude runner only**; other runners need a wrapper.
- **`tools`** are extra allowed tools for the stage, in Claude-style names
  (`Bash(cmd:*)`). They are translated for opencode.
- **`env`** is extra agent environment. `${VAR}` expands from the env file.

**Fail closed.** Only a contained stage may be granted `tools`, `env` or
settings `allow` rules. An uncontained grant is a config error: the daemon
won't start and `worker check` lists it. A tool like `Bash(php:*)` runs
arbitrary code, so it only makes sense inside something that contains it.

Every run records its `executor` (`local`, `wrapper:<hash>`, `sandbox:<hash>`),
visible in the API and the dashboard's run list.

## Repo knowledge (AGENTS.md, skills, verification)

**Instructions.** Every stage prompt includes the repo's `AGENTS.md` (falling
back to `CLAUDE.md`), read from the clone **at the base commit**. That's the
planner's base, or the approved base for implement and review. It is never
read from the agent's branch.

**Skills** are playbooks in `.skopos/skills/<name>/SKILL.md`. A skill can
start with frontmatter carrying a `description`; the body is the playbook.

- **Planning.** The planner sees an index of the skills at its base and names
  the ones that apply.
- **Tag hints.** Item tags that name a skill are always included. A Slack
  capture's `#hashtags` become tags.
- **Approval.** The selection is stored on the plan revision and covered by
  its hash, so approving the plan approves the skills. The Enrichment section
  lists them.
- **Implement and review.** These stages receive the bodies of the approved
  skills, read at the approved base.

**Verification.** Put repo-specific rules ("run `php -l` on changed files",
"phpstan must pass") in the repo's `AGENTS.md`, and grant the tools they need
through a profile. Then:

- the planner writes verification steps using only commands the implementer
  actually has;
- the implementer runs what it can;
- the worker runs the repo's `checks` after implementation, whatever the
  agent did, and records the results on the run. A failure goes back to the
  implementer, with the output, to fix (`check_fix_rounds`, default 2); if the
  checks still fail, the item blocks instead of going to review. Reviewers get
  the results for the commit they review.

### Setup

Every item is implemented in its own worktree, a fresh checkout with nothing
git doesn't track (or in the clone itself: see [Branch checkout](#branch-checkout)). A repo's `setup` commands prepare it before the agent
starts: dependencies, generated code, local config copied from the clone
(`{clone}` expands to the clone's path), database updates, an asset build.

```toml
[worker.repos."github.com/you/app"]
setup = [
  "cp {clone}/config/local.php config/local.php",
  "composer install --no-interaction",
  "npm ci && npm run build",
]
setup_timeout = "30m"
```

- **When it runs:** once, in a fresh worktree. Later rounds of the same item
  reuse the worktree and skip it; a setup that was interrupted runs again.
- **The code is already current.** The plan stage fetches the default branch
  and pins its newest commit; the worktree branches from that commit. Setup
  must not fetch, pull or check out: it would move the branch off the
  approved base, and it has no git credentials anyway.
- **Environment:** scrubbed like every agent run; `setup_env` passes what it
  needs.
- **Time:** `setup_timeout` bounds all the commands together (default `10m`,
  at most `2h`). When it runs out, the command is stopped with everything it
  started (its whole process group), so a script whose child hangs can't
  keep the run busy forever. `check_timeout` does the same for the checks
  (all of them together, default `30m`, at most `2h`): the check running
  when it runs out fails, and the rest fail without running.
- **Silence:** with `quiet_timeout` set (off by default), a command that
  prints nothing for that long is stopped the same way, long before the
  timeout. Set it above the longest silent stretch your setup and checks
  have (a migration, a test runner without progress output). A check
  stopped by either timeout is a hang, not a failure for the implementer:
  no fix round, the run is retried (see [Self-healing](#self-healing)).
- **Background processes:** a command that exits but leaves something
  running in the background (a dev server) still counts as finished: the
  worker stops waiting for its output 10 seconds after it exits.
- **Failure:** the first failing command stops setup and the worktree is
  removed. A setup failure is the environment's problem, so the run is
  retried (see [Self-healing](#self-healing)); the item blocks only once the
  retries are spent. The preview shows each command's output.

## Self-healing

A worker is meant to finish work without anyone fixing it. Four things keep
the worker's own problems from blocking items:

- **Retries.** A failure that is the worker's problem goes back to the
  queue instead of blocking the item: setup failed or hung, a check hung,
  git (fetch, checkout, worktree) failed, the push was rejected, or the
  provider kept failing. The retry waits 1 minute, then 5; from the second
  retry on, the worker that failed it can't take it for 30 minutes more, so
  another worker serving the repo gets the first go. After 3 attempts the
  item blocks with the last reason. Failures that need a person block at
  once, as before: checks still failing after the fix rounds, the agent
  saying it is blocked, a merge conflict, a branch rewritten since the plan
  was approved.
- **Preflight.** `preflight` lists commands that say whether this worker can
  take the repo's work right now (its database, queue, services):

  ```toml
  [worker.repos."github.com/you/app"]
  preflight = ["redis-cli -h $REDIS_HOST ping", "php cli_tools/healthcheck.php"]
  preflight_interval = "5m"
  setup_env = { REDIS_HOST = "${REDIS_HOST}" }
  ```

  The worker runs them in the clone before claiming, at most once per
  interval (each run bounded to 2 minutes). They get the scrubbed setup
  environment, so pass what they need through `setup_env`, as above.
  While one fails, the worker takes none of that repo's work — not even its
  own interrupted runs, which pass to other workers after the takeover
  grace — and the Workers view shows why. Slack hears once when a worker
  stops taking a repo's work, and once when it is back.
- **Step pushes.** While the implementer works, its commits are pushed as
  it makes them (checked every 30 s, through the same push path: prefix,
  protected branches, never force). A round cut off by a restart or a kill
  leaves its work on origin, and the next round continues from there. Set
  `push.steps = false` to push only at the end.
- **Self-upgrade.** The server names its release on every claim. An idle
  worker on an older release downloads `skopos_<os>_<arch>` from the
  release (`upgrade_url`, the GitHub releases by default), checks it
  against the release's `checksums.txt`, makes sure it runs as that
  version, renames it over its own binary and exits with code 75. systemd
  (`Restart=on-failure`), launchd (`KeepAlive`) and a supervisor loop start
  the new one. It never downgrades, never touches a dev build or a
  pre-release, and a failed upgrade waits an hour before the next try. The
  binary's directory must be writable by the worker; `auto_upgrade = false`
  turns it off.

  **Installed with Homebrew** (the binary is in a `Caskroom` or `Cellar`):
  the worker runs that prefix's `brew update`, then `brew upgrade --cask
  skopos`, instead of replacing the file (brew refreshes its taps itself
  only once a day), so Homebrew keeps track of the version and runs its
  install steps (on macOS, lifting the quarantine). It then checks that
  `<prefix>/bin/skopos` runs as the server's version before it exits to
  restart. Until the tap has the release (it usually lags by a few
  minutes), that check fails and it tries again an hour later. For this to
  work:

  - the service runs the linked path, `<prefix>/bin/skopos`
    (`/opt/homebrew/bin/skopos`, `/home/linuxbrew/.linuxbrew/bin/skopos`),
    not the versioned one, which `brew upgrade` removes. `skopos worker
    install` records the linked path; an older service file may name the
    versioned one, so check it once;
  - the worker runs as the user that owns the Homebrew prefix (brew refuses
    to run as anyone else).

  brew gets the worker's environment without its tokens and env-file values.

### Branch checkout

Some repos can't be duplicated into a worktree per item. Their checkout is
tied to a local database the setup migrates, and to configuration nobody
wants copied around. Set `checkout = "branch"` and the implementation runs in
the clone itself:

```toml
[worker.repos."github.com/you/app"]
checkout = "branch"
setup = ["php cli_tools/prepare_checkout.php"]
setup_timeout = "45m"
```

1. **Hold:** every run on the repo locks the clone, whatever its stage (a
   file lock in its `.git`). Another worker process on the same host waits.
   Plan runs read the clone's working tree, and every stage fetches into it.
2. **Branch:** an implement run switches the clone to the item's branch:
   - the branch it already has locally (a later round);
   - else the one on origin (another worker's earlier round);
   - else a new branch at the approved base, fetched first when the clone
     doesn't have that commit yet (another worker planned the item).
3. **Setup:** setup runs unless the clone is still set up for this branch at
   this head, for example when a paused run resumes and no other item has
   been set up in the clone since. Make it idempotent: it runs again whenever
   another item's branch was set up in between.
4. **Work:** the agent implements, the checks run, and the branch is pushed,
   as in a worktree.
5. **Back:** when the run ends, the clone goes back to the default branch,
   whether it pushed, failed or paused. Anything left uncommitted (a quota
   pause mid-turn) is stashed as `skopos:<branch>`. The item's next run
   applies the stash again, and the cleanup sweep drops it once the item is
   finished.

A worker killed mid-run leaves the clone on the item's branch. The next run
on the clone stashes the leftover changes under the branch and puts the clone
back on the default branch. Uncommitted or untracked files on the default
branch are different: they aren't skopos's, so every run on the repo stops
with an error naming them until you commit, remove or `.gitignore` them.

If the repo used worktrees before, an item's old worktree is removed when its
next implement run starts in the clone. If that worktree has uncommitted
changes, the run stops instead.

Each worker host needs its own clone (and its own database, if setup migrates
one). Several workers can serve the same repo, each with its own clone. The
approved base is a commit, so it doesn't matter which worker planned the
item. Branch checkout needs a unix host.

## Run logs

While a run executes, the worker streams what its agent, setup and checks
print:

- **To skopos,** always. The dashboard's item view shows it: **Watch live**
  on the running step, **Log** on finished steps and in Run history. Lines
  appear within a second (the panel long-polls); skopos keeps each run's log (8 MB at most)
  for the cleanup retention (`cleanup.retention_days`, 30 days by default).
- **To the log output,** when one is configured (`LOG_OUTPUT_URL` in
  `skopos-worker.env`; see [Log output](../configuration.md#log-output)).
  Each line carries `run_id`, `item_id`, `workspace`, `stage` and `src`.

What a run log holds:

- **Readable lines, not raw JSON:** the agent's text, each tool call (`▸ Bash
  go test ./...`) and the first lines of each tool result. Claude runs use
  `--output-format stream-json` for this; opencode and codex already print
  events.
- **Setup and checks:** each command (`$ …`), its output and whether it
  passed.
- **The worker's own notes:** the run starting, each agent turn and what it
  cost, pauses, the result. Also every step before the agent that can take
  a while: waiting for a branch-checkout clone (another worker process or
  the code indexing holds it), putting back a branch a killed run left,
  fetching and checking out the item's branch, and "setup skipped" when the
  clone is still set up for it. A step that takes 5 seconds or more gets a
  second line with how long it took.
- **No secrets:** the worker replaces its keys, push tokens, account
  credentials and env-file values with `[redacted]` before a line leaves it.

The raw agent output still lands in `<state_dir>/runs/<run>/<stage>.log`.
Reading a log takes `runs:read`, so agent keys can't.

## Secrets

No secrets live in the service definition or the TOML. The daemon loads
`skopos-worker.env` (next to the config) before parsing the config:

- **Format:** `KEY=VALUE` lines, `#` comments, plus the `KEY="v"` and
  `export KEY=` forms.
- **Precedence everywhere:** flags > real environment > env file > `[client]`
  in the TOML.
- **What goes there:** `SKOPOS_API_KEY`, `SKOPOS_AGENT_KEY`, each repo's
  `push.token_env` value, any `${VAR}` your accounts reference, and the log
  output settings (`LOG_OUTPUT_URL`, `LOG_OUTPUT_HEADER`).
- **SRV records:** `SKOPOS_SERVER_URL`, `LOG_OUTPUT_URL` and
  `push.remote_url` may be `srv+http(s)://name/…`; add
  `SKOPOS_DNS_SERVER=127.0.0.1:8600` when the host's resolver doesn't know
  Consul ([SRV URLs](../configuration.md#srv-urls)).
- **What agents see:** env-file values never reach an agent directly. Only an
  account's `env = { NAME = "${VAR}" }` passes a credential to the agent run.

## Presence

The dashboard's Workers view (and `GET /api/workers`) shows each worker as
**busy** (holds a claimed or running run), **idle** or **offline**:

- **From the first poll.** Every claim poll reports the worker's runners,
  workspaces, skopos version and `poll_every`, so a new worker is visible
  within seconds of starting, before it has worked on anything.
- **While busy.** Run heartbeats keep it fresh.
- **Offline.** A clean stop (SIGTERM, `systemctl stop`) says goodbye and the
  worker shows offline at once. After a crash it shows offline once it has
  been silent for three poll intervals (at least two minutes; 15 minutes
  while it holds a run).
- **Version.** Workers still on an older release after an upgrade are easy
  to spot.

The pipeline settings dialog suggests the runners these workers offer and
warns about a runner no worker serving the workspace offers yet.

## Service management

`skopos worker install` writes the platform service and starts it:

- **macOS:** a LaunchAgent at `~/Library/LaunchAgents/skopos-worker.plist`
  (`KeepAlive`, logs in `<state-dir>/service.log`). Restart with
  `launchctl kickstart -k gui/$(id -u)/skopos-worker`.
- **Linux:** a systemd **user** unit at
  `~/.config/systemd/user/skopos-worker.service` (no root), with linger
  enabled so it survives logout. `--system` installs a system unit under
  `/etc/systemd/system` instead (requires root, `multi-user.target`).
  Restart with `systemctl --user restart skopos-worker`; logs via
  `journalctl --user -u skopos-worker`.

**Flags:**
- `--label <name>`: the service name.
- `--dry-run`: print what it would do without changing anything.
- `--no-start`: write the definition disabled (launchd `Disabled`, systemd
  `disable`), so nothing starts it at login or boot. A failed check does the
  same.
- `--env-file <path>`: a non-default env file. It's checked, then passed to
  the service as `--env-file`.
- Linux only: `--system`, `--user <name>`, `--allow-root`, `--path <PATH>`.

Re-running `install` overwrites the service definition (it's derived from the
config) and restarts the service on both platforms, so it's safe.
`skopos worker uninstall` stops and removes the service; the config, env file
and state dir are kept.

`skopos worker check` (and install's pre-start check) evaluates the config in
the environment the service gets: its PATH, HOME and the env file, not your
shell's. A `SKOPOS_API_KEY` exported in your shell doesn't count.

## macOS note: Local Network permission

macOS gates processes that connect to LAN-private addresses, and
launchd-spawned processes cannot show the consent prompt. A worker against a
public HTTPS endpoint is unaffected. If your skopos server resolves to a LAN
address (a split-horizon domain counts), know the hard-won lessons from the
agent-trial predecessor:

- **Every upgrade resets it.** The permission attaches to the exact binary,
  and each Homebrew Cask version is a new file. After `brew upgrade`, the
  service silently fails to connect until you re-grant it in System Settings
  → Privacy & Security → Local Network.
- **A bare CLI binary sometimes never gets the grant.** agent-trial ended up
  with a small, stable **launcher** binary (embedded Info.plist, ad-hoc
  codesigned, never rebuilt) that holds the permission and execs the real
  binary. That pattern applies here too if you hit it.

The robust setups, in order:
1. Run workers on Linux VMs against the public endpoint.
2. Front the server with a public endpoint.
3. Use the launcher pattern for a Mac worker against a LAN server.

## Unattended operation

Before leaving a worker unattended:

1. `skopos worker check` passes and `skopos worker runner test` is green for
   every account.
2. Run one item supervised end to end, and confirm:
   - the run rows (`GET /api/runs`, or the dashboard's run list) show
     `succeeded` and the expected `runner` and `executor`;
   - quota pauses recover via `resume_after`: the run flips back to claimed on
     the owner's next poll after the reset;
   - pushes land on the intended remote with the configured `branch_prefix`.

Worker ids must start with `worker-`; the config refuses anything else. The
prefix marks worker-created plans so the server's orphan cleanup (empty,
unlinked, a day old) never deletes a plan a human made.

Give every worker process its own id. A worker runs one stage at a time
and only polls when idle, so when it polls, skopos hands back any run still
claimed or running under that id and key. That's how it recovers at once
from a claim whose response it never received (a timeout) or from its own
restart mid-run, instead of waiting out the 10-minute lease. Two processes
sharing an id would keep taking each other's run.
