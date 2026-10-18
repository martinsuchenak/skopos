# Tutorial: the agent pipeline, end to end

This guide sets up skopos to take a work item from a rough idea to a reviewed,
pushed branch: one server, one or more workers, agents doing the work, and
you approving at two gates. It takes about an hour the first time.

```
capture ──► queued ──► planning ──► awaiting_approval ──► approved ──► implementing
  (you)                (agent)        YOU approve the plan              (agent)
                                                                           │
   done ◄── you mark done ◄── in_review ◄── [review] ◄── [qa] ◄────────────┘
            (approves the head)              (optional agent stages)
```

- **The server** holds the inbox, plans, approvals, run queue and audit log,
  and serves the dashboard. It never runs agents.
- **Workers** claim runs (one stage of one item), run an agent CLI in a git
  worktree, push the branch, and report back.
- **You** approve the plan before any code is written, and approve the exact
  pushed head before an item is done. Approvals bind to what you saw: the
  plan revision plus its base commit, or the head commit.

## 1. Run the server

Pick a host the workers can reach over HTTPS (Docker and Nomad recipes:
[deployment](../deployment/)). Then:

```bash
skopos setup                  # choose "3) Run server" — writes an annotated skopos-config.toml
```

Set `auth.api_key` in `skopos-config.toml`. This is the **root** key: only for
administration, never for agents or workers.

Start the server:

```bash
skopos serve --server-host 0.0.0.0
```

Open the dashboard, click **Set key**, and paste the root key for now.

## 2. Register the workspaces and mint keys

A workspace is a repository, identified like `github.com/you/repo`. Register
each repository the pipeline should work on, using the root key:

```bash
export SKOPOS_SERVER_URL=https://skopos.example.com SKOPOS_API_KEY=$ROOT_KEY

skopos workspace add --id github.com/you/repo --name repo   # inside a checkout, --id defaults to it
skopos workspace list
```

Mint scoped keys:

```bash
# You: approves plans and reviews. Use it in the dashboard instead of the root key.
skopos key create --name me --workspace github.com/you/repo --approver

# One per worker machine: never --approver (workers must not approve their own work)
skopos key create --name worker-vm1 --workspace github.com/you/repo
```

Each secret is printed once. Many repositories? Use a workspace group
(`skopos group create …`, then `key create --group …`); see
[API keys](../concepts/api-keys.md).

## 3. Prepare each repository

Everything an agent reads about the repo comes from the repo itself, at the
approved base commit.

- **`AGENTS.md`** (or `CLAUDE.md`) at the root holds the conventions and the
  verification rules, e.g. "run `php -l` on every changed file" or "every
  new function gets a table-driven test". Agents can only run what their
  stage allows (step 5), so write rules that the tools you grant can satisfy.
- **Skills** are optional playbooks in `.skopos/skills/<name>/SKILL.md`, with
  a `description:` in the frontmatter. The planner picks the ones that apply.
  Tagging an item `#<name>` forces a skill in. You approve the selection
  together with the plan.
- **A push token** for the worker: a Gitea or GitHub token that can push
  branches. Agents never see it; only the worker pushes, under
  `branch_prefix` (default `agent/`). Protected branches like `main` are
  refused.

## 4. Decide which agent does what

Each stage can run on a different runner, meaning a different CLI or model.
Set it per workspace in the dashboard (Workspaces → **Pipeline**) or with the
CLI, no restart needed:

```bash
skopos workspace pipeline set --workspace github.com/you/repo --stage plan --runners claude
skopos workspace pipeline set --workspace github.com/you/repo --stage implement --runners claude
skopos workspace pipeline set --workspace github.com/you/repo --stage review --runners opencode-glm-5-3
```

Many repositories with the same setup? Put them in a workspace group (for
example the pattern `github.com/you/*`) and set the pipeline once on the
group; a repository overrides only the stages it needs.

- **Plan and implement always run.** They use any available account unless
  pinned.
- **Review and QA only run when you give them a preference.** Without one, a
  finished branch comes straight to you.
- **Use a different model for review.** A second opinion catches more than
  the implementer reviewing itself.
- **Names must match.** Runner names here must match the worker's runners
  (step 5); the dialog suggests the runners your workers offer and warns
  about one no worker offers yet. A run pinned to a runner waits for that
  runner's accounts; it never silently switches model.
- **Optional: several planners.** Add two or more runners to Plan
  (`--runners claude,opencode-glm`): each writes a candidate plan; you
  approve one at the gate. See
  [Multi-agent planning](../configuration.md#multi-agent-planning).
- **Optional: several reviewers.** List members to run review or QA on
  several agents, each with a focus brief from `.skopos/focus/<name>.md`:
  `--runners security@claude,tests@opencode-glm` on the review stage.
  They are decided together (by default, any blocking finding sends the item
  back to you) and leave one merged summary. See
  [Multi-agent review and QA](../configuration.md#multi-agent-review-and-qa).
- **Panel or solo, per item.** With lists configured, each item chooses: the
  dashboard's *Agents* field, `#panel` / `#solo` in a Slack capture (or a
  `panel` / `solo` reply in its thread), `--agents` on the CLI. Items that
  don't choose get the workspace default ("Items without their own choice
  get" in the Pipeline dialog; `panel` when unset). See
  [Per-item switch](../configuration.md#per-item-switch-panel-or-solo).

## 5. Set up a worker

On each worker machine (a Linux VM is the most robust), with skopos and the
agent CLIs installed (`claude` and/or `opencode`):

```bash
# Clone each repository the worker serves
git clone https://git.example.com/you/repo ~/clones/repo

# Scaffold config + env file; in a terminal it offers the runner wizard
skopos worker init --server-url https://skopos.example.com --workspace github.com/you/repo
```

**`skopos-worker.env`** holds the secrets (mode 0600):

```bash
SKOPOS_API_KEY=sk_...            # the worker key from step 2
GITEA_PUSH_TOKEN=...             # the push token from step 3
```

**`skopos-config.toml`**: fill in the repo block:

```toml
[worker.repos."github.com/you/repo"]
path = "~/clones/repo"
checks = ["task test"]            # the worker runs these after every implementation

[worker.repos."github.com/you/repo".push]
kind = "gitea"                    # gitea | github | none
remote_url = "https://git.example.com/you/repo.git"
token_env = "GITEA_PUSH_TOKEN"
```

**Runners and accounts.** The wizard detects the CLIs, lists the models, and
writes the config:

```bash
skopos worker runner add          # once per runner/model; names should match step 4
skopos worker runner test         # one real prompt per account, through the locked-down runner
skopos worker runner list         # the overview
```

**Toolchains: optional, but needed to run tests inside the agent.** By
default an implementing agent can only edit files and commit. To let it run
`php`, `go test` and the like, contain the stage and grant the tools in a
profile:

```toml
[worker.profiles.php.implement]
wrapper = ["docker", "run", "--rm", "-i", "-v", "{dir}:{dir}", "-w", "{dir}",
           "-e", "ANTHROPIC_API_KEY", "ghcr.io/you/agent-php@sha256:..."]
tools   = ["Bash(php:*)", "Bash(vendor/bin/phpstan:*)", "Bash(vendor/bin/phpunit:*)"]

[worker.repos."github.com/you/repo"]
profile = "php"
```

skopos refuses extra tools on an uncontained stage. Containment is yours to
provide: a container, a VM, or Claude Code's sandbox (see
[Containing agent runs](../deployment/worker.md#containing-agent-runs)).
Without a profile, the worker's `checks` still verify every branch.

**Check and install:**

```bash
skopos worker check               # everything the service will see: keys, clones, CLIs, profiles
skopos worker install             # launchd (macOS) or systemd (Linux); starts only if check passes
```

## 6. Run your first item

**Capture** an idea, in any of these ways:
- **Dashboard:** Inbox → Capture item.
- **CLI:** `skopos inbox add --workspace github.com/you/repo --title "…" --content "…" --tag go-tests`
- **Slack** (step 7): DM the bot `new repo: …`. Slack captures are queued
  immediately.

**Panel or solo?** If you gave plan, review or QA a list of agents (step 4),
choose per item whether it gets that panel or a single agent:
- **Dashboard:** the *Agents* field when capturing or editing (Repository
  default / Agent panel / One agent); the item then shows an `agent panel`
  or `one agent` badge;
- **CLI:** `skopos inbox add … --agents panel` (or `solo`), and later
  `skopos inbox update --id <item> --agents solo|panel|default`;
- **Slack:** add `#panel` or `#solo` to the `new:` message.

Leave it unset to get the workspace default (set in its Pipeline dialog,
`panel` unless you changed it). You can switch an item later; it applies from its
next plan, review or QA stage.

**Queue it.** Open the item in the dashboard and click **Queue**, or run
`skopos inbox queue --id <item>`. A worker claims the plan run within a poll
interval.

Every action below also has a CLI form, run with your approver key:
`skopos inbox plan|approve|request-changes|retry|mark-done|reject --id <item>`.
In a terminal, `approve` and `mark-done` show the revision or head and ask you
to confirm. Scripts must name it with `--revision` or `--head`.

**Approve the plan.** The item reaches `awaiting_approval` with an
**Enrichment** section: the steps, the size, the branch, the base commit and
the selected skills. On a card, **Review plan** opens it; plans are never
approved from a card. The preview shows which revision you're approving.
Then either:
- click **Approve**, with optional notes; they're appended for the
  implementer and override the plan where they conflict;
- or click **Request changes**, with notes, to get a re-plan.

The approval binds to that revision. If the planner wrote a newer one
meanwhile, the server refuses and the preview reloads with the current plan.

**Several planners?** With a `plan` member list (step 4), the gate shows one
card per **candidate**. Approve the one you want (or `approve 2` in Slack);
it becomes the plan. *Revise* sends one candidate back to its planner with
your notes; *Re-plan all* runs every planner again.

**Implementation runs.** The worker branches from the approved base, the agent
commits each step, the worker runs `checks`, pushes, and the configured
review stage runs.

**Review and finish.** At `in_review`, look at the branch, the review verdict
on the item, and the run list (runner, executor, head commit). Then:
- **Mark done** with the prefilled head commit: this approves exactly that
  commit;
- or **Request changes**, with notes, sends it back to implementation.

**Blocked?** If an agent needs an answer, the item is `blocked` with its
question. Click **Retry** with your answer; it's appended to the item and the
next run reads it.

**Reject** (the preview's button, with a confirmation; `skopos inbox reject`;
or `reject` in Slack) takes an item out of the pipeline.

Every transition, approval and edit is in the item's timeline
(`GET /api/inbox/{id}/timeline`), labelled with who acted and from where.

## 7. Optional: drive it from Slack

Set these on `skopos serve` (a Socket Mode app, so no public URL is needed):

```bash
SKOPOS_SLACK_BOT_TOKEN=xoxb-...   SKOPOS_SLACK_APP_TOKEN=xapp-...
SKOPOS_SLACK_USER_ID=U...         SKOPOS_SLACK_DEFAULT_WORKSPACE=github.com/you/repo
```

The bot DMs you a thread per item at every gate. A plan-ready message lists
the revision's steps, and `approve` approves exactly that revision. If the
plan changed, or the relay restarted and can't know what you read, it posts
the current plan and asks again. You reply with:
- `approve` or `approve: notes` (with several candidates: `approve 2`,
  `approve glm`, `approve 2: notes`);
- `revise 2: notes` to rework one candidate;
- `revise: notes`;
- `changes: notes`;
- `done`;
- `retry: answer`;
- `reject`;
- `panel`, `solo` or `default` to switch the item's agents (applied at once,
  even mid-run; it takes effect from the next stage).

Start an item with `new: …`, or `new <workspace>: …` for another repo; add
`#panel` or `#solo` to choose its agents. See
[Slack relay](../configuration.md#slack-relay-server).

## 8. Scale out

- **More workers.** Repeat step 5 on another machine with its own key. Runs
  stick to the worker that started them, because the agent session lives
  there.
- **More accounts.** Add accounts to a runner (`runner add` again, same name).
  The worker rotates to the next account when one hits its quota, and a
  quota pause no longer stalls other work.
- **More models.** Give each model its own runner name and pin stages to it
  (step 4).
- **Other CLIs.** Use `runner add`, kind `command`. These only run inside a
  wrapper, because skopos can't verify their lockdown.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Runs stay `queued` | No worker serves the workspace or offers the pinned runner: check `skopos worker runner list` (claims) and the worker's logs |
| `no account feeds runner "X"` | A stage preference names a runner without an account — `skopos worker runner add` with that name |
| `runner "X" has no lockdown skopos has verified` | A `command` runner on a stage without a `wrapper` — add a profile with one |
| `grants tools/env but is not contained` | A profile grants tools without a wrapper/sandbox — add one, or remove the tools |
| `does not support --setting-sources, --restricted …` | Claude Code is too old for the lockdown — upgrade it |
| `mcp_servers is set but mcp_config is not` | Runs no longer read `~/.claude.json`; point `worker.mcp_config` at a JSON file with the servers |
| `runner test` fails with `env X resolved empty` | The account's `${VAR}` isn't set in `skopos-worker.env` |
| Item `blocked` with a question | Answer it with **Retry** (dashboard) or `retry: …` (Slack) |
| `approve` refused as stale | The plan changed after you looked — re-open the item and approve the current revision |

Next: the [worker reference](../deployment/worker.md) for every option, and
[configuration](../configuration.md) for all keys and flags.
