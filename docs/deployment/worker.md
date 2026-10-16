# Deploying a worker

A worker is the execution plane: it polls skopos for run stages
(plan/implement/review/qa) and executes them with an agent CLI against a local
clone. It is the same binary as the server (`skopos worker`) and needs only:

- skopos over HTTP (URL + a worker-scoped API key)
- a local clone per served workspace
- the agent CLI(s) on the host (`claude`, ...)

Run one worker per VM, one key per worker. On the skopos side, route stages
per workspace with `--run-stage <workspace>:<stage>=<runner>`; the claim only
hands a worker the runs whose preferred runner it offers.

## Quick start

```bash
# 1. Scaffold the config (annotated [worker] section + skopos-worker.env)
skopos worker init --server-url https://skopos.example.com \
                   --workspace github.com/you/repo

# 2. Mint a worker key (root key, against the server)
skopos key create --name worker-vm1 --workspace github.com/you/repo \
                  --server-url https://skopos.example.com --api-key <root-key>

# 3. Fill in the config (repos, accounts) and the secrets file
#    - skopos-config.toml: clone paths, push remote, account env
#    - skopos-worker.env: SKOPOS_API_KEY and the push token_env values

# 4. Validate the config (what install checks before starting)
skopos worker check

# 5. Install and start the service (launchd on macOS, systemd on Linux)
#    A config that fails the checks gets the unit written DISABLED (not
#    started now, nor at the next login/boot) — fix it and re-run install.
skopos worker install

# 6. Watch the first item: queue one and follow
#    queued -> planning -> awaiting_approval (you approve) -> implementing -> in_review
```

## Service management

`skopos worker install` writes the platform service and starts it:

- **macOS**: a LaunchAgent at `~/Library/LaunchAgents/skopos-worker.plist`
  (`KeepAlive`, logs in `<state-dir>/service.log`). Restart with
  `launchctl kickstart -k gui/$(id -u)/skopos-worker`.
- **Linux**: a systemd **user** unit at
  `~/.config/systemd/user/skopos-worker.service` (no root), with linger
  enabled so it survives logout. `--system` installs a system unit under
  `/etc/systemd/system` instead (requires root, `multi-user.target`).
  Restart with `systemctl --user restart skopos-worker`; logs via
  `journalctl --user -u skopos-worker`.

Flags: `--label <name>` (service name), `--dry-run`, `--no-start`,
`--env-file <path>` (a non-default env file — checked, then passed to the
service as `--env-file`), and on Linux `--system`, `--user <name>`,
`--allow-root`, `--path <PATH>`. `--no-start` (or a failed check) writes the
definition disabled: launchd `Disabled`, systemd `disable` — nothing starts
it at login or boot. `skopos worker uninstall` stops and removes the service;
the config, env file and state dir are kept.

Re-running `install` overwrites the service definition (it is derived from the
config) and restarts it on both platforms — safe.

`skopos worker check` (and install's pre-start check) evaluates the config in
the environment the service gets — its PATH, HOME and the env file — not your
shell's: a `SKOPOS_API_KEY` exported in your shell does not count.

## What every agent run gets

Agent runs are locked down regardless of configuration:

- **No user or project settings** (`--setting-sources ""`). The agent does
  not load the worktree's `CLAUDE.md`, the worker user's `~/.claude/CLAUDE.md`,
  the allow rules in `~/.claude/settings.json`, hooks, or the MCP servers in
  `~/.claude.json`. Instructions reach the agent only through the stage
  prompt (see *Repo knowledge* below).
- **File tools confined to the working directory** (`--restricted`). The
  env file, the state dir and other clones are out of reach of
  Read/Edit/Write. Claude Code already path-checks Bash read commands.
- **MCP servers only from `mcp_config`** (`--strict-mcp-config`). Put the
  servers runs may use in a JSON file (`{"mcpServers": {...}}`), point
  `worker.mcp_config` at it, and allowlist their tool prefixes in
  `mcp_servers`.
- **Scrubbed environment.** Agent runs, checks and `setup` commands get no
  push tokens, no skopos key, no SSH agent and nothing from the env file;
  `setup_env` passes what setup legitimately needs.

The agent CLI must support these flags. The daemon refuses to start, and
`skopos worker check` reports it, when it does not.

## Containing agent runs

skopos provides no isolation; it delegates. How a stage is contained is
your choice: a container, a VM, a sandbox tool, or Claude Code's own
sandbox. You declare it per stage in a profile:

```toml
[worker.profiles.php.implement]
wrapper = ["docker", "run", "--rm", "-i", "-v", "{dir}:{dir}", "-w", "{dir}",
           "-e", "ANTHROPIC_API_KEY", "ghcr.io/you/agent-php@sha256:..."]
tools   = ["Bash(php:*)", "Bash(composer:*)", "Bash(vendor/bin/phpstan:*)", "Bash(vendor/bin/phpunit:*)"]

[worker.profiles.php.review]
settings = '{"sandbox":{"enabled":true}}'
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
- **`settings`** is merged into the run's settings (skopos's deny list always
  stays). `sandbox.enabled = true` counts as containment.
- **`tools`** are extra allowed tools for the stage.
- **`env`** is extra agent environment. `${VAR}` expands from the env file.

**Fail closed.** Only a contained stage (one with a `wrapper`, or
`settings` with `sandbox.enabled = true`) may be granted `tools`, `env` or
settings `allow` rules. An uncontained grant is a config error: the daemon
won't start and `worker check` lists it. A tool like `Bash(php:*)` runs
arbitrary code, so it only makes sense inside something that contains it.

Every run records its `executor` (`local`, `wrapper:<hash>`, `sandbox:<hash>`)
on the run, so you can see which envelope executed it.

## Repo knowledge (AGENTS.md and skills)

Every stage prompt includes the repo's `AGENTS.md` (falling back to
`CLAUDE.md`). It is read from the clone **at the base commit**: the
planner's base, or the approved base for implement and review. It is never
read from the agent's branch, which could otherwise brief its own reviewer.

Skills are playbooks in `.skopos/skills/<name>/SKILL.md`. A skill can start
with frontmatter carrying a `description`; the body is the playbook.

- **Planning.** The planner sees an index of the skills at its base and
  names the ones that apply.
- **Tag hints.** Item tags that name a skill are always included. A Slack
  capture's `#hashtags` become tags.
- **Approval.** The selection is stored on the plan revision and covered by
  its hash, so approving the plan approves the skills. The Enrichment
  section lists them.
- **Implement and review.** These stages receive the bodies of the approved
  skills, read at the approved base.

## Secrets

No secrets live in the service definition. The daemon loads
`skopos-worker.env` (next to the config) before parsing the config:

- `KEY=VALUE` lines, `#` comments, `KEY="v"` and `export KEY=` forms
- precedence everywhere: flags > real environment > env file > `[client]` in the TOML
- the real environment always wins over the file
- put `SKOPOS_API_KEY` and each repo's `push.token_env` value there

The TOML holds no token values by design; `token_env` names the variable.

## macOS note: Local Network permission

macOS gates processes that connect to LAN-private addresses, and
launchd-spawned processes cannot show the consent prompt. A worker against a
public HTTPS endpoint is unaffected. If your skopos server resolves to a LAN
address (a split-horizon domain counts), know the hard-won lessons from the
agent-trial predecessor:

- the permission attaches to the exact binary, so **every upgrade resets it**
  (each Homebrew Cask version is a new file) — the service will silently fail
  to connect after `brew upgrade` until the permission is re-granted in
  System Settings → Privacy & Security → Local Network;
- a bare CLI binary sometimes never gets the grant at all — agent-trial ended
  up with a small stable **launcher** binary (embedded Info.plist, ad-hoc
  codesigned, never rebuilt) that holds the permission and execs the real
  binary; that pattern applies here too if you hit it.

The robust setups, in order: run workers on Linux VMs against the public
endpoint; front the server with a public endpoint; or use the launcher
pattern for a Mac worker against a LAN server.

## Unattended operation

Before leaving a worker unattended, run one item supervised end to end and
check:

- the run rows (`GET /api/runs`) show `succeeded`, not repeated `interrupted`
- quota pauses recover via `resume_after` (the run flips back to claimed on
  the owner's next poll after the reset)
- pushes land on the intended remote with the configured `branch_prefix`

Worker ids must start with `worker-` — the config refuses anything else. The
prefix marks worker-created plans so the server's orphan cleanup (empty,
unlinked, a day old) never deletes a plan a human made.
