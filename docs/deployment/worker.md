# Deploying a worker

A worker is the execution plane: it polls skopos for run stages
(plan / implement / review / qa) and executes them with an agent CLI against a
local clone. It is the same binary as the server (`skopos worker`) and needs:

- skopos over HTTP (URL + a worker-scoped API key)
- a local clone per served workspace (with a push token if it should push)
- one or more agent CLIs: **Claude Code** and **opencode** are built in; any
  other CLI can run inside a wrapper you provide

Run one worker per VM, one key per worker. For the whole setup — server, keys,
repos, workers, first item — follow the
[pipeline tutorial](../guides/pipeline.md); this page is the worker reference.

## Quick start

```bash
# 1. Scaffold the config (annotated [worker] section + skopos-worker.env).
#    In a terminal it offers the runner wizard right away.
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
model and the credentials.

| Runner kind | Lockdown | How it runs |
|---|---|---|
| `claude` (default) | verified | Claude Code `-p`; structured results via `--json-schema` |
| `opencode` | verified | `opencode run --format json`; results extracted from the reply |
| `command` | **unverified** | any CLI from an argv template — runs **only inside a wrapper** |

**Verified** means skopos has checked the lockdown live (the opt-in tests
`SKOPOS_LIVE_CLAUDE=1` / `SKOPOS_LIVE_OPENCODE=1`):

- instruction files are not loaded;
- files outside the worktree are unreadable;
- the stage's tool allowlist is enforced, and commits still work.

skopos can't vouch for an arbitrary CLI, so a `command` runner on a stage
without a wrapper refuses the run with a clear reason. `worker check` lists
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
  - **Claude:** this machine's login, an API key kept in the env file, or a
    separate login directory for a second subscription account.
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
command = ["aider", "--yes", "--message", "{prompt}"] # {prompt} {dir} {model}; no {prompt} = stdin
```

`worker.runners` (what claims offer) defaults to every runner your accounts
use. Built-in names (`claude`, `opencode`) need no `runner_config` block.

### One model per stage

The model belongs to the account, and a runner name is any label. To run one
CLI with different models, give each model its own runner name. The wizard
does this when you pick a model. Then pin stages to runners in the
workspace's pipeline settings (dashboard: Workspaces → Pipeline):

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
  - **Instructions:** reach the agent only through the stage prompt; see
    [Repo knowledge](#repo-knowledge-agentsmd-skills-verification).
- **Stage allowlists.** Plan is read-only; implement can edit and run a short
  list of git and read commands; review can read and diff. The prompts name
  the exact shell commands the stage allows and the checks the worker runs
  afterwards, and tell the agent not to chain commands. A profile can grant
  more tools (see [Containing agent runs](#containing-agent-runs)).
- **MCP servers only from `mcp_config`.** Put the servers runs may use in a
  JSON file (`{"mcpServers": {...}}`, Claude Code's shape; opencode gets it
  translated), point `worker.mcp_config` at it, and allowlist their tool
  prefixes in `mcp_servers`.
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
  `opencode.json` and similar files are reset to the base version, so no CLI
  picks up instructions the implementer wrote. Those changes still show in
  the reviewed diff.
- **Only the worker pushes.** It validates the refspec, keeps the token in its
  own environment, and never force-pushes.

The daemon refuses to start, and `worker check` reports it, when a runner's
CLI can't do its lockdown (for example, an older Claude Code without
`--restricted`).

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
  agent did, and records the results on the run.

## Secrets

No secrets live in the service definition or the TOML. The daemon loads
`skopos-worker.env` (next to the config) before parsing the config:

- **Format:** `KEY=VALUE` lines, `#` comments, plus the `KEY="v"` and
  `export KEY=` forms.
- **Precedence everywhere:** flags > real environment > env file > `[client]`
  in the TOML.
- **What goes there:** `SKOPOS_API_KEY`, each repo's `push.token_env` value,
  and any `${VAR}` your accounts reference.
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
