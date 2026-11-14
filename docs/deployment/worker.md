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
skopos key create --name worker-vm1 --workspace github.com/you/repo --worker \
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
model  = "router/qwen/qwen3.8-27b"
env    = { ROUTER_API_KEY = "${ROUTER_API_KEY}" }  # the providers file says {env:ROUTER_API_KEY}

[worker.runner_config.aider]                          # any other CLI: wrapper-only
kind    = "command"
command = ["aider", "--yes", "--message", "{prompt}"] # {prompt} {dir} {model} {effort}; no {prompt} = stdin
```

`worker.runners` (what claims offer) defaults to every runner your accounts
use. Built-in names (`claude`, `opencode`, `codex`) need no `runner_config`
block.

### Runners the server defines

When the server holds [runner definitions](../concepts/workspace-config.md#runner-definitions),
an account can name the CLI it is a login for and leave the runner, model
and effort to the server:

```toml
[[worker.accounts]]
name = "claude-1"
kind = "claude"                       # feeds every claude runner the server defines
env  = { CLAUDE_CONFIG_DIR = "~/.claude-1" }

[[worker.accounts]]
name   = "opencode-zai"
kind   = "opencode"
models = ["zai-coding-plan/*"]        # only the runners whose model matches
env    = { ZAI_API_KEY = "${ZAI_API_KEY}" }
```

- **The worker fetches the definitions before its first claim** and again,
  between runs, whenever a claim answer names another revision.
- **`runner_config.<kind>`** (an opencode `config`, a `bin`, a declared
  `endpoint`) serves every defined runner of that kind, unless the runner
  has a block under its own name.
- **An account that names a `runner`** keeps working. If the server defines
  that name, the definition's model and effort replace the account's.
- **A runner is not offered** when the file gives its name another kind,
  when the endpoint here contradicts the provider the definition declares,
  or when its CLI fails the lockdown check. The log and
  `skopos worker check` say which and why.
- **One login, one quota:** a limit hit under one runner pauses every
  runner the same account feeds.
- **A `command` runner** stays in this file: its argv runs on this host.

### What the worker reports about its runners

With every claim the worker tells the server, for each runner: the CLI, the
model, the endpoint the CLI sends its requests to, and the provider behind
that endpoint. A workspace's [runner policy](../concepts/runner-policy.md)
is matched against these, so they decide which runs the worker is given. It
logs them at startup; `skopos worker runner list` shows each runner's
provider ("sends to") and says when one needs declaring; the dashboard's
Workers view shows them too.

- **Derived where it can be:** from `ANTHROPIC_BASE_URL`,
  `CLAUDE_CODE_USE_BEDROCK` and `CLAUDE_CODE_USE_VERTEX` for claude, from
  `OPENAI_BASE_URL` for codex, and from the model's provider and the
  runner's opencode config for opencode. The values are read from the
  account's `env`, else the service's own environment.
- **Declared where it can't:** a model on another machine on your network,
  a router that forwards to several companies, and any `command` runner.

  ```toml
  [worker.runner_config.opencode-local]
  kind = "opencode"
  provider = "local"          # a model on another machine on this network

  [worker.runner_config.aider]
  kind = "command"
  command = ["aider", "--yes", "--message", "{prompt}"]
  endpoint = "https://api.openai.com"   # provider derived: openai
  ```

  Undeclared, such a runner reports `unknown:<host>`, which a policy
  allows only by naming it.
- **Only what the agent really gets counts.** Values in the worker's env
  file never reach an agent (the scrub drops them), so an
  `ANTHROPIC_BASE_URL` there changes nothing an agent does and nothing the
  worker reports. Set such a variable in the account's `env`.
- **Derived once, at startup.** Restart the worker after changing an
  account, a runner or an opencode config.
- **Refused by `skopos worker check` and `skopos worker install`.** A
  running worker with one of these reports no facts, logs the problem as
  an error and takes runs only for workspaces without a runner policy:
  - an account's `env` sets `HTTPS_PROXY`, `HTTP_PROXY` or `ALL_PROXY`
    (a proxy for the whole host belongs in the service's environment);
  - a stage profile's `env` sets a proxy or an endpoint variable
    (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `CLAUDE_CODE_USE_*`,
    `OPENCODE_CONFIG`);
  - two accounts of one runner differ in model, provider or endpoint.
    Before, this was a warning. Give each its own runner name. Accounts
    that differ only in `effort` still get the warning;
  - a declared provider contradicts the endpoint.
- **The server names the runner of every run.** For a stage with no
  preference it picks the first runner in the worker's order that the
  policy allows and that can run the stage: `claude` first, then the order
  of `worker.runners`. A worker doesn't run a run that names no runner, or
  one it doesn't have.

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

With an access token instead, log in from a file or variable (codex reads
the token on stdin):

```bash
printenv CODEX_ACCESS_TOKEN | CODEX_HOME=~/skopos-workers/accounts/codex-1 codex login --with-access-token
```

```toml
[[worker.accounts]]
name   = "codex-1"
runner = "codex"
env    = { CODEX_HOME = "~/skopos-workers/accounts/codex-1" }   # or CODEX_API_KEY = "${CODEX_KEY}"
```

- **Credentials in `env`** (`CODEX_ACCESS_TOKEN`, `CODEX_API_KEY`) reach
  the codex process but never the agent's shell: every account env key
  except `PATH` and `HOME`, and any variable named like a credential, is
  left out of it. Set only one of them; codex refuses to start with two.
  A login directory keeps the credential out of the environment
  altogether.

- **No AGENTS.md in the login directory.** Codex always loads
  `$CODEX_HOME/AGENTS.md` and has no switch for it, so a worker refuses an
  account whose login directory holds one (or `AGENTS.override.md`), and
  `worker check` reports it. A personal `~/.codex` often has one; that's why
  the wizard suggests a separate directory.
- **Sessions** are kept in the login directory, so an interrupted run
  resumes on its own worker and account.
- **Usage limits** pause the run and mark the account until the reset Codex
  reports.
- **MCP servers** reach a Codex run as settings on its command line, since
  its lockdown ignores the login directory's own config:
  - It gets the skopos tools and the run's **http** servers. A server that
    is a command is left out of a Codex run, and the run's log says so:
    Codex would start it outside its sandbox.
  - A header's value is never on the command line. It is handed over in a
    variable of the Codex process that the agent's shell does not get.
  - A server's address is on the command line, so a server whose address
    carries user info or one of the run's secrets is left out. Keep
    credentials in headers.
  - Codex is told which of a server's tools exist for the run: the ones
    the stage allows, less `mcp_deny`. A deny pattern (`mcp__router__write_*`)
    works when the stage allows that server's tools by name; with the whole
    server allowed (`mcp__router`) the server is left out, because Codex
    takes names, not patterns.
  - The agent's shell still has no network: it reaches a server only
    through that server's tools.
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
Workspaces → Settings):

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
    then the hard denies, including `external_directory`. opencode applies
    `external_directory` to its file tools but only to a few shell commands,
    so the shell's allowed commands are also refused when their text
    reaches out of the worktree or runs something else: `$` and backtick
    expansion, `~`, absolute paths, `..`, output redirects (`>`),
    `find -exec` and similar, `rg --pre` and `git diff --no-index`. The edit
    tool can't write `.git` (a `.git/config` line can make git run a
    command). This matches the command text; it is not a sandbox. A symlink
    in the repository still leads out, so give opencode's implement and
    review stages a wrapper profile where that matters.
  - **Codex:** `codex exec --ignore-user-config --ignore-rules
    --strict-config`, every setting passed with `-c`: no AGENTS.md
    (`project_doc_max_bytes=0`), no skills listing, plugins, apps or hooks,
    approvals `never`. Its permission profile lets the shell read only the
    worktree, the clone's git directory and what a shell needs (not the
    temp directories); nothing is writable and the network is off. The
    shell's environment leaves out the account's env and anything named
    like a credential. Its MCP servers are only the run's own, passed as
    settings (see [Codex](#codex)). The stage allowlist becomes that read-only sandbox, since Codex reads files
    through its shell.
  - **Instructions:** reach the agent only through the stage prompt; see
    [Repo knowledge](#repo-knowledge-agentsmd-skills-verification).
- **Stage allowlists.** Plan is read-only; implement can edit and run a short
  list of git and read commands; review can read and diff. Commits are
  allowed only as `git commit -m …` (git then refuses `-F`, so a commit
  can't take its message from a file). Claude and opencode runs also refuse
  options of allowed commands that reach past the allowlist: `git --output`
  (writes a file), `--pathspec-from-file` (echoes one), `rg --pre` and
  `rg --hostname-bin` (run a command). Codex runs need no such list, since
  nothing is writable in their sandbox and it covers the files they read;
  agy (wrapper-only) relies on its wrapper. The prompts name
  the exact shell commands the stage allows and the checks the worker runs
  afterwards, and tell the agent not to chain commands. A profile can grant
  more tools (see [Containing agent runs](#containing-agent-runs)).
- **MCP servers only from `mcp_config`** (plus skopos itself, with an agent
  key: see [Skopos tools for agent runs](#skopos-tools-for-agent-runs)). Put
  the servers runs may use in a JSON file (`{"mcpServers": {...}}`, Claude
  Code's shape; opencode and Codex get it translated), point `worker.mcp_config` at
  it, and allowlist their tool prefixes in `mcp_servers`. A workspace whose
  config on the server names its MCP servers gets those instead, with each
  run: see [MCP servers from the server](#mcp-servers-from-the-server).
- **Scrubbed environment.** Agent runs, checks and `setup` commands get no
  push tokens, no skopos key, no SSH agent and nothing from the env file:
  - an account's `env` (with `${VAR}` references) passes credentials to the
    agent;
  - `setup_env` passes what setup needs.

  Setup and checks run the branch's code, which the agent wrote, as the
  worker user. The agent's lockdown doesn't cover them; only the scrubbed
  environment and the timeouts apply. Where that matters, run the worker
  itself in a container or as a user that can reach only what it needs.
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

Claude, opencode and Codex runs reach skopos over MCP: the code index, the
blackboard, and their plan's steps. There is nothing to set up.

**Each run gets its own token.** The server mints it when the worker claims
the run, and the worker writes it into that run's MCP config:
`<state_dir>/runs/<run>/mcp.json` (the directory 0700, the file 0600),
holding your `mcp_config` servers plus a `skopos` server with the token.
The file is removed when the run ends.

- **One workspace:** the token reaches only the run's own workspace. A
  worker that serves several repositories no longer gives every agent a
  key for all of them.
- **One attempt:** it works only while the run is claimed or running. A
  report, a pause, a retry or a cancel ends it at once.
- **Recorded as the run:** blackboard entries and audit records name
  `run <id> (<runner> on <worker>)`.
- **Redacted:** the token never appears in the run's log.
- **A `skopos` server in your `mcp_config`** is replaced by the run's own
  for these runs.
- **A wrapper that mounts files into a container** has to mount the run's
  directory, `<state_dir>/runs/<run>/` (`{rundir}` in the wrapper). Before,
  the one file to mount was `<state_dir>/agent-mcp.json`.
- **If the run's config can't be written** (a broken `mcp_config`, a full
  disk), the run's agents get no skopos tools and the worker log says so.
  The shared agent key is never used in a token's place.

See [Run tokens](../concepts/api-keys.md#run-tokens) for exactly what a
token may do.

**A server from before run tokens** (before v0.33) sends none. For that
case only, a worker can still hold an **agent key**: an MCP-only key shared
by all its runs.

```sh
skopos key create --name knot-1-agent --workspace github.com/you/repo --preset agent
```

```bash
SKOPOS_AGENT_KEY=sk_...        # in skopos-worker.env
```

The worker checks an agent key with `whoami` at startup and refuses to
start when it could give an agent more than MCP: full access, the `rest`
surface, the approver or admin permission, or no `code` read. It writes
`<state_dir>/agent-mcp.json` (mode 0600) and refuses an `mcp_config` that
defines its own `skopos` server. Once the server issues run tokens, the
worker says so in its log and the key is no longer used; remove it.

**The worker's own key** is checked at startup, and by `skopos worker
check`. A key that holds the admin permission is refused, because a worker
must not be able to change the policy and configuration it runs under. The
root key and an approver key still work, each with a warning at startup.

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

  The guidance lists `code_search`, `code_symbol` and `code_callers` /
  `code_impact`; when that branch's symbols are embedded, it also tells the
  agent to start with `code_find` to locate code by what it does.
- **Server instructions:** a capped key's MCP `initialize` answer describes
  only the tools it may call, not the full skopos instructions. A capped key
  sends one request at a time (no JSON-RPC batches).
- **Codex** runs get the same tools and the same token, as settings on
  their command line instead of a file (see [Codex](#codex)). The token is
  in a variable of the Codex process, not in an argument.
- Neither the run token nor an agent key reaches the agent's environment:
  the token lives only in the run's MCP config, and the scrub drops every
  `*_KEY` variable.

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
branch_prefix   = "JOB"             # the worker only pushes branches starting with this
branch_template = "{ref}-{slug}"    # default "{prefix}{tail}-{slug}"
```

- **Placeholders:** `{prefix}` (`branch_prefix`), `{tail}` (the item id's
  tail), `{slug}` (the title, lowercase with dashes, at most 30 characters)
  and `{ref}` (the item's **Reference** field, e.g. `JOB100234`). A template
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
  (`JOB100234-api`, `JOB100234-ui`) and none matches exactly: the item stops
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
   (its review stage under Workspaces → Settings): one runner, one focused
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
  - Placeholders:

    | Placeholder | Expands to |
    |---|---|
    | `{dir}` | the run's working directory (its worktree, or the clone for a branch checkout) |
    | `{gitdir}` | the clone's git directory. A worktree's `.git` file points into it, and commits write there |
    | `{login}` | the account's login directory: `login_dir` on the account, else `CLAUDE_CONFIG_DIR` (default `~/.claude`), `CODEX_HOME` (default `~/.codex`), opencode's data directory (`$XDG_DATA_HOME/opencode`, default `~/.local/share/opencode`), `~/.gemini` for Antigravity |
    | `{rundir}` | the run's own directory, `<state_dir>/runs/<run>/`: its MCP file, its agent log, and the temporary config directories the runners create |
    | `{cli}` | the directory the agent CLI's binary is in, symlinks resolved. A wrapper that uses it starts the agent by that resolved path, so the directory on `PATH` that only holds a link to it need not be inside |
    | `{run}`, `{stage}`, `{workspace}` | the run id, the stage, the workspace id |

  - A placeholder the wrapper uses and the worker has no value for fails
    the run before the agent starts. A `command` runner has no login
    directory unless its account sets `login_dir`. A `{name}` that is not
    in this table is passed on as written, with a warning at startup; in
    a wrapper defined on the server it is refused when the wrapper is saved.
  - **Setup and checks run behind the wrapper too.** The worker's `setup`
    and `checks` commands for an implement stage, and for the review of an
    existing branch, execute the branch's code, so they run through the
    stage's wrapper like the agent. The wrapper has to hold what they
    need: the toolchain, caches, and anything `setup` reads from the
    clone.
  - The wrapper process receives the scrubbed environment plus the stage's
    `env`, so forward what the agent needs (`-e NAME`).
  - A container image must contain the agent CLI.
  - Session resumes need the login directory (`{login}`) inside the wrapper.
  - The agent needs network access to its model API.
- **`settings`** is merged into a claude run's settings (skopos's deny list
  always stays). `sandbox.enabled = true` lets a stage grant capabilities
  **on the claude runner only**; other runners need a wrapper. For a
  runner policy it is its own executor, `sandbox:<hash>` (see
  [Executors](#executors)).
- **`tools`** are extra allowed tools for the stage, in Claude-style names
  (`Bash(cmd:*)`). They are translated for opencode.
  - `Agent` (`Task` on older Claude Code) lets a claude stage start
    subagents, for example to hand smaller jobs to a smaller model; opencode
    runs ignore it. A subagent stays inside the stage's lockdown: files only
    in the worktree, the stage's Bash limits, no project instructions loaded
    (checked by the live lockdown tests).
- **`env`** is extra agent environment. `${VAR}` expands from the env file.

**Fail closed.** Only a contained stage may be granted `tools`, `env` or
settings `allow` rules. An uncontained grant is a config error: the daemon
won't start and `worker check` lists it. A tool like `Bash(php:*)` runs
arbitrary code, so it only makes sense inside something that contains it.

**A workspace the server manages** takes each stage's `tools`, `env` and
`settings` from the server, and this file keeps what contains the stage:
see [Stages and wrappers from the server](#stages-and-wrappers-from-the-server).

### A worker inside a container

When the worker's whole host is a container (a Knot space, a VM, a Docker
container), say so:

```toml
[worker]
contained_by = "knot"
```

- Stage profiles may then grant `tools`, `env` and settings `allow` rules
  without a wrapper, and unverified runners (`antigravity`, `command`) run
  without one.
- It says the host is confined. It does not keep runs apart: inside the
  container a run still sees every clone and login on the worker. A
  [policy rule](../concepts/runner-policy.md#containment) that asks for
  `isolated` is not met by it; one that asks for `contained` is.
- A wrapper that only sets variables (`wrapper = ["env", "NAME=value"]`)
  counts the same way: nothing goes around the run, so it is the host's
  containment (`host:<name>`, the name from `SKOPOS_CONTAINED_BY=<name>`).
  `skopos worker runner list` and the worker's startup log point it out;
  `contained_by` replaces it. An `env` wrapper that runs a command of its
  own (`["env", "A=b", "bwrap", …]`) is that command's wrapper.

### Executors

Every run records its **executor**: what contained the agent.

| Executor | What it is |
|---|---|
| `wrapper:<name>:<hash>` | the stage's wrapper, around the agent and the worker's setup and check commands. `<name>` is the profile's name (`repo` for a repo's own `stages` block), or the name of a wrapper the server defines; the hash covers the wrapper's argv |
| `runner:<kind>` | a runner whose own lockdown is a sandbox: `runner:codex` on macOS, where that lockdown is verified (read-only, confined to the worktree, no network from the shell; Codex itself reaches the run's MCP servers). Not for the review of an existing branch, whose setup and checks the worker runs outside it |
| `host:<name>` | the worker's host is a container (`contained_by`) |
| `sandbox:<hash>` | Claude Code's sandbox, turned on by the stage's `settings`; the hash covers the sandbox settings |
| `local` | nothing but the agent CLI's own lockdown |

When several apply, the first in that order is the run's executor.

Two more are not executors a run records: they say the worker cannot run a
stage with a runner, and the server hands it no such run.

| Reported | When |
|---|---|
| `none:wrapper` | the workspace's config on the server wraps that runner's runs of the stage in a wrapper this worker cannot use: its check fails here, it needs a `${NAME}` the worker does not declare, or it is no longer defined |
| `none:uncontained` | the stage is given tools, env or settings — by the workspace's config on the server, or by a profile in this file — and nothing contains a run of it with that runner here. Sandbox settings contain only a runner that honours them (claude) |
| `none:config` | the workspace's stage blocks on the server cannot be used on this worker (a `${NAME}` it does not declare, or something it refuses): the worker takes none of the workspace's runs |

- The worker reports the executor a run would get, per workspace, stage and
  runner, with every claim. A workspace's
  [runner policy](../concepts/runner-policy.md#containment) can require one:
  `isolated` is met by a wrapper or a runner sandbox, `contained` also by a
  host container, and a sandbox by its name (`sandbox:*`).
  `skopos worker runner list` shows them (`runs in:`), and so does the
  dashboard's Workers view.
- A run comes with the executor the server matched its policy on. A worker
  that would run it in something else starts nothing and gives the run
  back; its next claim reports what it would run it in.
- What a stage profile gives a run (`tools`, `env` names, and every setting
  besides the sandbox block) is recorded apart, as the run's `grants`
  hash. Changing it doesn't change the executor.
- The runners create their temporary config directories under the run's own
  directory, not in the shared `/tmp`, and they are removed with the run.
- The worker never writes through a link in a run's directory: a file it
  writes there replaces whatever a run left under that name.

### A bwrap profile

[bubblewrap](https://github.com/containers/bubblewrap) runs the agent in
its own mount namespace on Linux, with only the paths you bind. This
profile gives a run its worktree, the clone's git directory, its account's
login and its own run directory, and nothing else of the worker: no other
clone, no state directory, no env file, no other account's login, no home
directory.

<!-- bwrap-profile:begin -->
```toml
[worker.profiles.bwrap.implement]
wrapper = ["bwrap", "--unshare-all", "--share-net", "--die-with-parent",
  "--ro-bind", "/usr", "/usr",
  "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
  "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
  "--ro-bind-try", "/etc/ssl", "/etc/ssl",
  "--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
  "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
  "--ro-bind-try", "/etc/hosts", "/etc/hosts",
  "--ro-bind-try", "/etc/passwd", "/etc/passwd",
  "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
  "--ro-bind", "{cli}", "{cli}",
  "--bind", "{dir}", "{dir}",
  "--bind", "{gitdir}", "{gitdir}",
  "--ro-bind-try", "{gitdir}/config", "{gitdir}/config",
  "--ro-bind-try", "{gitdir}/hooks", "{gitdir}/hooks",
  "--bind", "{login}", "{login}",
  "--bind", "{rundir}", "{rundir}",
  "--chdir", "{dir}"]
```
<!-- bwrap-profile:end -->

- **`/etc` is not bound whole.** A worker env file may live there.
- **The network stays open:** the agent has to reach its model API, and
  bwrap can't limit it by host. Services listening on the host's loopback
  are reachable too.
- **The worker's own git does not trust the worktree.** A run can rewrite
  the two files that tell git where a worktree's repository is (`.git` in
  the worktree, `commondir` in its git directory). For every git command
  the worker itself runs in a run's worktree, and for the value of
  `{gitdir}`, it names the directories from the clone's side
  (`GIT_DIR`, `GIT_COMMON_DIR`, `GIT_WORK_TREE`) and turns the file system
  monitor off. This does not cover `checkout = "branch"`, where the agent
  works in the clone itself.
- **`{gitdir}` is the whole git directory of the clone,** so a run can read
  and write every branch of its own repository. Its `config` and `hooks`
  are read-only: the worker runs git in that clone afterwards. It can't
  reach another repository.
- **Commits need an identity in the clone's own config**
  (`git -C <clone> config user.name …`): a `~/.gitconfig` is not inside.
- **The login directory.** With Claude Code's default `~/.claude`, the file
  `~/.claude.json` sits beside it, outside `{login}`. Give the account its
  own `CLAUDE_CONFIG_DIR`, which holds both.
- **The agent CLI.** `{cli}` holds a self-contained binary. A CLI that is
  a script (an npm install) also needs its runtime and its package
  directory bound.
- **Tools the stage needs** (a language toolchain outside `/usr`, a package
  cache) need their own binds. The worker's setup and checks run behind
  the same wrapper.
- **The host needs unprivileged user namespaces.** Inside a container that
  usually means the container allows them.
- **What is checked.** `TestBwrapProfileFromDocs` runs this profile, read
  from this page, around a stand-in agent on Linux
  (`go test ./internal/worker -run TestBwrapProfileFromDocs`; it skips
  where bwrap can't create namespaces, and it is not part of CI). The run
  can read and write its worktree, commit, use its login and its run
  directory. It can't see another clone, the clone's own working tree, the
  state directory, the env file, another account's login, another run's
  directory or the home directory, and it can't change the clone's git
  config or hooks. A check command run through the wrapper is confined the
  same way. It is not a test of a particular agent CLI under bwrap: try a
  run on a test workspace before a policy rule depends on the profile.

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

## Config from the server

A workspace can have its shared settings on the server
([Workspace config on the server](../concepts/workspace-config.md)). For
such a workspace the worker takes branches, timeouts, push settings and the
MCP lists from there, and with `server_commands = true` its `setup`,
`checks` and `preflight` commands too.

```toml
[worker]
id = "worker-knot-claude-1"
server_commands = true

[worker.repos."github.com/acme/shop"]
path = "/srv/clones/shop"
push.token_env = "REPO_PUSH_TOKEN"
```

- **What stays in this file:** the clone's `path`, `setup_env`,
  `push.token_env`, the accounts, `mcp_config` (for workspaces whose config
  names no MCP servers), and what contains a stage
  (`contained_by`, a profile's `wrapper` and sandbox settings).
- **A push block with only `token_env`** is enough here: the kind, the URL
  and the pull-request setting come from the server. While the server
  holds no config for the workspace, such a block keeps the workspace out
  of this worker's claims (unhealthy, with the reason) instead of running
  work it would never push.
- **The same fields in this file are ignored** for a managed workspace. The
  worker logs once which ones; a field the server leaves out has its
  default, not this file's value.
- **Without `server_commands`** the command fields — `setup`, `checks`,
  `preflight` and the stage blocks — stay this file's and the rest still
  comes from the server. The worker logs that the server sets commands it
  does not take.
- **Each run names its config.** The claim carries the revision; the worker
  fetches exactly that one before the stage starts and writes it at the
  top of the run log. A run whose revision cannot be fetched goes back to
  the queue and is retried.
- **A command change is logged** when the worker applies it, with the
  commands.
- **A config this host cannot use** — it pushes, and the repo has no
  `push.token_env` here — keeps the workspace out of this worker's claims,
  shown as unhealthy with the reason, like a failing preflight.
- **Plan and implement timeouts and `mcp_servers`** are the workspace's
  for a managed workspace; the `[worker]` values serve the others. A
  `mcp_deny` this file sets stays in force everywhere: for a managed
  workspace it is added to the workspace's list.
- **`skopos worker check`** fetches the definitions and each workspace's
  config and checks the merged result.
- **A server that cannot be reached at start:** the worker waits and
  retries before it takes work.
- **A worker older than this feature** is given no runs of a managed
  workspace, and shows as unhealthy for it until it upgrades.

### MCP servers from the server

A workspace's config on the server can name the
[MCP servers](../concepts/workspace-config.md#mcp-servers) its runs get.
For such a workspace:

- **Each run gets its servers with its claim,** credentials included, and
  the worker writes them into the run's own MCP file,
  `<state_dir>/runs/<run>/mcp.json`, next to skopos itself. The file is
  removed when the run ends.
- **`mcp_config` in this file is not used for the workspace,** also when a
  server could not be delivered or the list is empty. It still serves the
  workspaces whose config names no MCP servers.
- **A run that cannot get its file is given back.** When the run's MCP
  file cannot be written, or its claim carried no servers although the
  workspace's config names them, the worker starts nothing and the run is
  retried.
- **Files a killed worker left behind are removed at the next start:** a
  run's MCP file holds that workspace's credentials.
- **An opencode runner's own `config` file should not define MCP
  servers:** opencode merges it with the run's, and only the tool allowlist
  then stands between such a server and a run.
- **The credentials are kept out of the run's log.**
- **A `command` server runs on this host,** so it is only taken with
  `server_commands = true`, and its binary has to be installed here.
  `${NAME}` in its command and env comes from `[worker.vars]`.
- **What the run did not get is in its log:** the server and why (a secret
  that is not set, `server_commands` off, a `${NAME}` this worker does not
  declare).
- **`skopos worker check`** names the workspaces that take their MCP
  servers from the server.

### Stages and wrappers from the server

A managed workspace's [stage blocks](../concepts/workspace-config.md#stages)
say what each stage's agent is given. On a worker with
`server_commands = true` they replace what this file's profiles give the
workspace:

```toml
[worker]
id = "worker-knot-claude-1"
state_dir = "/var/lib/skopos"
server_commands = true
contained_by = "knot"

[worker.vars]
CACHE_DIR = "/var/cache/skopos"

[worker.repos."github.com/acme/shop"]
path = "/srv/clones/shop"
push.token_env = "REPO_PUSH_TOKEN"

[[worker.accounts]]
name = "claude-1"
kind = "claude"
env = { CLAUDE_CONFIG_DIR = "/var/lib/skopos/accounts/claude-1" }
```

- **`tools`, `env` and `settings` come from the server,** per stage. A
  stage the server gives nothing gets nothing: what a profile in this file
  gives the workspace is not used. `skopos worker check` and the worker's
  log name the stages concerned.
- **What contains a stage is still this file's:** `contained_by`, a
  profile's `wrapper`, and the `sandbox` block and `permissions.deny` rules
  of a profile's `settings`. A profile that only does that can stay; one
  that only gave tools and env can go.
- **An account's own variables win** over a stage's env from the server.
- **`[worker.vars]`** holds this host's values for `${NAME}` in a stage's
  env and in a wrapper's argv. Nothing else is filled in, the worker's
  environment included. A name a stage's env uses and this table lacks
  makes the workspace unhealthy on this worker, with the name; this file's
  profiles do not apply instead.
- **The worker checks what it is handed.** A stage block or a wrapper
  definition the worker itself would refuse (an env variable a stage may
  not set, an argv that only sets variables, an unknown placeholder) is not
  used, whatever the server saved.
- **Nothing contains the stage here:** the worker takes no run of that
  stage and reports `none:uncontained` for it. `contained_by`, a wrapper or
  sandbox settings for the stage fixes it.
- **A wrapper the server attaches** ([Wrappers](../concepts/workspace-config.md#wrappers))
  goes around the agent and the stage's setup and check commands, like a
  profile's. Its placeholders are filled in the same way. The worker runs
  the wrapper's `check` when it first needs it, when the definition changes
  and every five minutes; while the check fails the worker reports
  `none:wrapper` and takes no run the wrapper is for.
- **A wrapper this file sets for the stage wins.** The server's rule is
  then not used.
- **bwrap inside a container** needs unprivileged user namespaces. Docker's
  default seccomp profile blocks them, and bwrap fails with "No permissions
  to create new namespace". A wrapper's `check` shows that at the claim,
  before any run.

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
- **Continuing a cut-off implementation.** When an implement run starts on a
  branch that already has the implementer's commits (`agent: step N -
  <title>`, matched by title, so an amendment's renumbering doesn't matter),
  those steps are marked done in the plan and the prompt marks them
  ALREADY COMMITTED: the agent continues with the next one. Uncommitted
  changes the earlier run left (a worktree as it was, or the
  `skopos:<branch>` stash of a branch checkout) are listed in the prompt for
  the agent to finish or undo, and an `index.lock` a killed git command left
  is removed. This covers a run that hit `impl_timeout`, a crash or a
  restart, and Retry after any of them.
- **Questions and decisions.** The planner's and the implementer's questions
  are posted to their run (`POST /api/runs/{id}/questions`) and recorded by
  skopos, which writes the item's section; a server without the endpoint
  gets the section appended as before. Each stage's prompt reads the
  decisions from the record (`POST /api/runs/{id}/questions/read`), falling
  back to the item's sections on an older server.
- **Back to the same worker.** An implement or branch-review run queued
  again for an item (**Retry**, **Request changes**) waits up to 15 minutes
  for the worker that ran that stage before, with the same runner: its
  clone has the branch, the setup and anything the last run left
  uncommitted. The tracker shows "Waiting for <worker> … then any worker".
  Once that worker is offline, no longer offers the repo or runner, or
  fails the repo's preflight, or after the 15 minutes, any worker takes the
  run and continues from what was pushed.
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

Before every run the clone goes back on its default branch:

- **On another branch with nothing uncommitted:** it is switched back, whoever
  checked that branch out.
- **A worker killed mid-run** leaves the clone on the item's branch. The next
  run on the clone stashes the leftover changes under the branch and puts the
  clone back on the default branch. The worker knows the branch was its own
  from a marker it writes in the clone's `.git` before the checkout.
- **Uncommitted or untracked files no run left** — on the default branch, or
  on a branch you checked out by hand — aren't skopos's: every run on the repo
  stops with an error naming them until you commit, stash or remove them (or
  `.gitignore` them). Nothing is stashed or switched under you. To work in the
  clone yourself, [pause the worker](#pausing-a-worker) first.

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
  appear within a second (the panel long-polls), and a live view moves to
  the item's next run when it starts ("Move to the next run"); skopos keeps each run's log (8 MB at most)
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
Reading a log takes `runs:read`, so run tokens and agent keys can't.

## Secrets

No secrets live in the service definition or the TOML. The daemon loads
`skopos-worker.env` (next to the config) before parsing the config:

- **Format:** `KEY=VALUE` lines, `#` comments, plus the `KEY="v"` and
  `export KEY=` forms.
- **Precedence everywhere:** flags > real environment > env file > `[client]`
  in the TOML.
- **What goes there:** `SKOPOS_API_KEY`, each repo's
  `push.token_env` value, any `${VAR}` your accounts reference, and the log
  output settings (`LOG_OUTPUT_URL`, `LOG_OUTPUT_HEADER`).
- **SRV records:** `SKOPOS_SERVER_URL`, `LOG_OUTPUT_URL` and
  `push.remote_url` may be `srv+http(s)://name/…`; add
  `SKOPOS_DNS_SERVER=127.0.0.1:8600` when the host's resolver doesn't know
  Consul ([SRV URLs](../configuration.md#srv-urls)).
- **What agents see:** env-file values never reach an agent directly. Only an
  account's `env = { NAME = "${VAR}" }` passes a credential to the agent run.
- **Credentials a repository keeps for testing:** a QA run may read them
  from the clone to sign in to a test system
  ([QA](../configuration.md#qa-testing-the-change)). Name those files in the
  repo block and their secrets stay out of every run log:

  ```toml
  [worker.repos."github.com/you/app"]
  redact_files = ["tests/e2e/.env.e2e"]   # relative to the clone, KEY=VALUE lines
  ```

  The values of lines whose key reads as a credential (`PASSWORD`, `SECRET`,
  `TOKEN`, `…_KEY`) are replaced; values under 8 characters are not, like
  every redacted value.

## Presence

The dashboard's Workers view (and `GET /api/workers`) shows each worker as
**busy** (holds a claimed or running run), **idle**, **paused** (see
[Pausing a worker](#pausing-a-worker)) or **offline**:

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

## Pausing a worker

To work on a worker's machine yourself (an editor connected over SSH, your
own Claude session), pause the worker instead of stopping the service:

```sh
skopos worker pause --for 2h --reason "working on the stock API by hand"
skopos worker resume
```

- **What stops:** a paused worker claims nothing new, not even its own
  interrupted runs (they wait for it), and does no idle work: no code
  indexing, no cleanup sweep. Other workers take the queue, and a retry no
  longer waits for a paused worker.
- **What doesn't:** a run it already holds finishes. Stopping the service
  instead cuts that run off.
- **How long:** `--for` takes a duration (`90m`, `2h`); without it the pause
  lasts until `skopos worker resume`.
- **Which worker:** run on the worker's host, the commands find it the way
  the daemon does: the config (`--config`, else `skopos-config.toml` in the
  current directory or `~/.config/skopos`), the `skopos-worker.env` beside it
  and the worker's key. `--worker-id` names another one.
- **Who may:** the key the worker runs with, the root key, or an approver
  whose scope covers every workspace the worker serves.

The dashboard's Workers view has the same Pause and Resume buttons, and
shows who paused a worker, until when and why. Pauses and resumes are in the
audit log (`worker.pause`, `worker.resume`). The API is
`POST /api/workers/{id}/pause` (`{"for": "2h", "reason": "…"}`, or `until`)
and `POST /api/workers/{id}/resume`.

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
address (a split-horizon domain counts), know this:

- **Every upgrade resets it.** The permission attaches to the exact binary,
  and each Homebrew Cask version is a new file. After `brew upgrade`, the
  service silently fails to connect until you re-grant it in System Settings
  → Privacy & Security → Local Network.
- **A bare CLI binary sometimes never gets the grant.** A small, stable
  **launcher** binary (embedded Info.plist, ad-hoc codesigned, never
  rebuilt) that holds the permission and execs the real binary gets
  around it.

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
