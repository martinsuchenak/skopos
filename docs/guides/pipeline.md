# Tutorial: the agent pipeline, end to end

This guide sets up skopos to take a work item from a rough idea to a reviewed,
pushed branch: one server, one or more workers, agents doing the work, and
you approving at two gates. It takes about an hour the first time.

```
capture ──► queued ──► planning ──► awaiting_approval ──► approved ──► implementing
  (you)                (agent)        YOU approve the plan              (agent)
                                                                           │
   done ◄── you mark done ◄── in_review ◄── [qa] ◄── [review] ◄────────────┘
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

Each stage can run on a different runner, meaning a different CLI, model or effort.
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
  finished branch comes straight to you. Review runs first; QA runs once the
  review has passed, or straight after the implementation when only QA is
  set.
- **Briefs without a commit.** What a focused agent is told can be held on
  the server (Pipeline → Briefs) instead of in `.skopos/focus/`:
  [Briefs on the server](../configuration.md#briefs-on-the-server).
- **Review reads the code, QA tests it.** QA runs at the pushed commit after
  the repository's setup, with the instructions you give it in a focus
  brief, and asks you when it has nothing to test on. See
  [QA: testing the change](../configuration.md#qa-testing-the-change).
- **Use a different model for review.** A second opinion catches more than
  the implementer reviewing itself.
- **Limit which models may see a repository.** A runner policy allows
  runners by provider, model and stage, per workspace, group or fleet:
  `skopos policy set --group acme --allow provider=anthropic`. See
  [Runner policy](../concepts/runner-policy.md).
- **Keep a repository's checks in one place.** With several workers, hold
  its setup and check commands, branches and push settings on the server
  instead of in each worker's file, and define each runner's model once:
  `skopos workspace config set`, `skopos runner set`. See
  [Workspace config on the server](../concepts/workspace-config.md).
- **Give a stage the repository's own tools once.** The extra commands a
  stage's agent may run (`Bash(npm run:*)`), its env, and a wrapper for
  some runners go in the workspace's or group's config as a stage block,
  instead of in each worker's profiles. See
  [Stages](../concepts/workspace-config.md#stages).
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
agent CLIs installed (`claude`, `opencode` and/or `codex` — codex for read-only stages):

```bash
# Install skopos and set up the worker in one go: it asks for the worker key,
# the clone (and clones it), the push token, then the agent runners, checks
# the config and starts the service
curl -fsSL https://github.com/martinsuchenak/skopos/releases/latest/download/install.sh | sh -s -- \
  --worker --server-url https://skopos.example.com --workspace github.com/you/repo
```

Or by hand: clone each repository, then `skopos worker init --server-url
https://skopos.example.com --workspace github.com/you/repo`. Either way,
you can check or adjust the two files it writes:

**`skopos-worker.env`** holds the secrets (mode 0600):

```bash
SKOPOS_API_KEY=sk_...            # the worker key from step 2
# SKOPOS_AGENT_KEY=sk_...        # only for a server before v0.33: runs now get their own token from the claim
GITEA_PUSH_TOKEN=...             # the push token from step 3
```

**`skopos-config.toml`**: fill in the repo block:

```toml
[worker.repos."github.com/you/repo"]
path = "~/clones/repo"
checks = ["task test"]            # the worker runs these after every implementation

[worker.repos."github.com/you/repo".push]
kind = "gitea"                    # gitea | github | none (github: a personal access token with Contents read/write)
remote_url = "https://git.example.com/you/repo.git"
token_env = "GITEA_PUSH_TOKEN"
```

**Runners and accounts.** The wizard detects the CLIs, lists the models, asks
for the effort (empty = the CLI's default), and writes the config:

```bash
skopos worker runner add          # once per runner/model/effort; names should match step 4
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

**Starting from an existing branch.** Fill in the item's **Branch** (or
its **Reference**, when the repository's branch names use it, e.g.
`JOB203345`):
- **Queue** it, and the agents continue the branch: the planner sees what is
  already on it and plans the rest (a *handover*).
- **Review branch** instead, and nothing is planned or implemented: a worker
  runs the checks on the branch and an agent reviews it. You then mark it
  done, or request changes, which turns it into a handover.

See [Branch names and existing branches](../deployment/worker.md#branch-names-and-existing-branches).

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
**Enrichment** section: the steps, the size, the branch, the base commit, the
selected skills and the risks. On a card, **Review plan** opens it; plans are
never approved from a card. The preview shows which revision you're approving.

**Answer the planner's questions.** The planner's open questions follow the
plan in a **Questions — planner** section, numbered, each choice lettered:

```markdown
## Questions — planner

1. Keep the old endpoint?
   - (a) yes, deprecated
   - (b) no, remove it
2. Which cache backend?
```

A step that depends on an answer names the question by its number. The
preview lists the questions above the notes field: pick an option, write an
answer, or both, for each one you answer. The answers go with **Approve**,
**Request changes** or **Retry** as their own **Answers** section, ahead of
your notes. Every later plan, and the implementer, reads them with the
questions. When the planner can't plan without answers, the item is blocked
with the same list, and **Retry** sends your answers. The implementer asks
the same way: a proposed amendment or a block comes with a **Questions —
implementer** section, listed for answering at the gate or with **Retry**.

**The record.** Every question and answer is kept as a record, not only as
text in the item: who asked (planner, implementer or worker, the run, the
candidate) and about what (the plan revision and base, or the commit), and
for each answer who gave it, when, through which surface (dashboard, Slack,
CLI, MCP or the API) and with which action and approval.
`GET /api/inbox/{id}/questions` returns it, each question with its state:
open, answered, answered in free text, skipped (a gate passed it unanswered)
or superseded (a later round asked again). Approving or sending the item back
records the open questions you leave unanswered as skipped. The item's
sections are written from the record, and the audit trail has a
`question.asked`, `question.answered` or `question.skipped` event for each.

**Answering in free text.** Answers can be a list by question id or free
text. In free text, lines like `1) a`, `1. b — and drop the docs` or
`2) Redis` answer question 1 or 2 of the open questions, by option letter
and/or in words. With several planners asking at once, `<label> 1) a` or a
`To <label>:` line picks whose question. The rest of the text is an answer
to the round's questions as a whole. The action's response says which lines
answered which question. Notes work the same for the lines that answer
(`approve: 1) a, 2) Redis` in Slack, `--notes` in the CLI); the rest of the
notes stays notes.

**Answering without approving.** While the item waits for you (awaiting
approval, blocked or failed), **Save answers** in the preview records your
answers and leaves the item where it is; so do `answer: 1) a, 2) …` in
Slack, `skopos inbox answer --id <item> --answer 1=a --answer "2=Redis"`,
and `POST /api/inbox/{id}/answers`. The next run reads them.

**Correcting an answer.** Answering an answered question again is a
correction: it names the answer it replaces (`replaces` in the API,
`skopos inbox answer --question <id> --replaces <answer id> --option b`).
The record keeps both, and a correction made after a run already used the
earlier answer is flagged. `skopos inbox questions --id <item>` shows the
record with the ids.

**What the agents read.** Each run's prompt gets the decisions from the
record: every answered question with its current answer, the questions you
passed unanswered (marked as such, so the agent decides or asks again), and
your free-text answers, including the answers you gave when no question was
open (a Retry answer to a failed check, say). The record notes the first run
that used each answer.

**The decision log and reports.** `skopos questions` lists every question
across items with its state and current answer (who, via what, with which
action and approval, how long it waited), filtered by `--workspace`,
`--item`, `--state`, `--asker`, `--answered-by`, `--from` and `--to` (local
dates);
`--format csv` exports it. `skopos questions summary` counts the same rows:
answered, in free text, skipped, open, superseded, corrections (and those
after a run used the answer) and the median wait, in total, per workspace
and per person. The API is `GET /api/inbox/questions` (`format=csv`) and
`GET /api/inbox/questions/summary`; agents read the same through the MCP
tool `inbox_questions`. A run's questions publish a `questions.waiting`
event on `/api/events/stream`.

Then either:
- click **Approve**, with optional notes; they're appended for the
  implementer and override the plan where they conflict;
- or click **Request changes**, with notes or answers, to get a re-plan.

To read a plan in full, or several planners' plans next to each other,
click **Read the plans side by side**. The same view exports them as
Markdown, as does `skopos inbox plans --id <item>`.

The approval binds to that revision. If the planner wrote a newer one
meanwhile, the server refuses and the preview reloads with the current plan.

**What the agents read.** The item keeps its whole history: every plan, your
notes, the answers and the reviews, in the order they happened. The agents
get your own text, the planner's questions with your answers, and only the
sections that are current for them:
- **A re-plan** gets the plan your newest notes are about, and those notes.
- **The implementer** gets the approved plan, your approval notes and, after a
  review sent the item back, the latest review with your change requests.

Older plans and notes that were already dealt with stay out of the prompts.
Your text is everything before the first section the pipeline added, so
don't give your own text a heading the pipeline uses (`## Review notes`,
`## Answers` and so on).

**Several planners?** With a `plan` member list (step 4), the gate shows one
card per **candidate**. Approve the one you want (or `approve 2` in Slack);
it becomes the plan. *Revise* sends one candidate back to its planner with
your notes; *Re-plan all* runs every planner again.

**Implementation runs.** The worker branches from the approved base, the agent
commits each step, the worker runs `checks` and pushes. If the workspace has a
review (or QA) stage, it starts on its own as soon as the push lands; you
don't start it. Without one, the item goes straight to `in_review` and the
preview says no agent reviewed the commit.

**Checks are the CI.** The repo's `checks` run on the worker after every
implementation, outside the agent's lockdown (the toolchain must be installed
on the worker host):
- **A failing check goes back to the implementer** with its output, in the same
  session, to fix and commit. The checks then run again. That happens up to
  `check_fix_rounds` times (default 2; 0 blocks at once).
- **Uncommitted changes to tracked files fail too** (`committed`, checked
  after the other checks), so what passed is what gets pushed. That includes a
  check that rewrites a tracked file (`go mod tidy`, a formatter with `-w`): the
  implementer is asked to commit the result. A `setup` command that rewrites a
  tracked file (a regenerated lockfile, say) trips this too; make setup leave
  tracked files alone.
- **If checks still fail, the item blocks** at Implement with "checks failed:
  …". So does a fix round where the implementer reports it can't fix them. The
  branch is pushed anyway, so you can look at it, and its pull request opens
  as a draft even with `pull_request = "ready"`. Retry with an answer to send
  it back.
- **A quota pause during a fix round** resumes at the checks, with the rounds
  it has already used, not by re-running the implementation.
- **Reviewers get the results** for the commit they review. They can't run
  tools, so they review by reading, and a failed check counts as blocking.
- **The preview's Checks step** lists each check, with its output under
  **Check output**. It shows as skipped when the repo has no checks, and red
  when the checks block the item. While the checks run, or the implementer
  fixes the ones that failed, the step shows as running.

**Follow it in the preview.** The item preview opens with the seven pipeline
steps: Queued, Plan, Plan approval, Implement, Checks, Agent review and Your
sign-off.
- **Finished steps** are checked, with who did them and what came out: the
  plan revision, the pushed commit with a **View changes** link (the base to
  head comparison on GitHub or Gitea), the setup commands that prepared the
  worktree (output under **Setup output**), the checks, and each reviewer's
  verdict. The
  review findings and summaries unfold under the step. A repository with no
  `push` block in the worker config keeps the branch on the worker: the
  preview says so, shows no link, and won't offer a second opinion, because
  reviewers fetch the branch from the remote.
- **The step waiting for you** holds the buttons, under a short list of what
  you're approving. At plan approval the plan's steps are listed.
- **A running step** warns when its worker has gone offline or stopped
  sending heartbeats, instead of spinning forever.
- **Run logs:** **Watch live** on a running step opens what its agent, setup
  and checks print as it happens (the agent's text, each tool call and the
  start of its result, each command and whether it passed), and the worker's
  steps before that (waiting for the clone, fetching, checking out). Finished steps
  and every Run history row have a **Log** button for the saved copy. See
  [Run logs](../deployment/worker.md#run-logs).
  - **The live view moves with the pipeline.** With **Move to the next run**
    ticked (it is when you open a running step), the panel shows the item's
    next run as soon as it starts: plan, then implement, each reviewer, QA.
    It stays where it is while you have scrolled up to read; **Follow**
    takes you on. A log opened on a finished step stays on that run.
- **Below the steps:** the item description, the run history and a short
  history of who queued, approved, asked for changes or signed off. The
  description shows your own text, then what the pipeline added, in rounds:
  a plan with its questions and your notes on it, a review with your change
  requests. The newest round is open; older ones are folded, numbered when a
  kind repeats ("Plan · round 2 of 3"). The header shows the item's ID (the
  copy button copies it) and what the agent runs cost, when the runners
  report it. On a phone the preview fills the screen.

**What's waiting for you.** Beside the Inbox entry in the sidebar, a count
shows how many items in the selected workspace (all of them under "All
workspaces") wait for you: plan approval, sign-off, or blocked. The browser
tab title carries the same number. Click the count, or the **Needs you**
chip in the list, to see just those. Board cards show the seven steps as dots.

**Review and finish.** At `in_review`, look at the branch (or its pull
request), the review verdicts and the run history. Then:
- **Mark done (approve `<commit>`)**: this approves exactly the commit the
  reviewers checked. A push after it needs a new sign-off;
- or **Request changes**, with notes, sends it back to implementation.

**Findings are followed across rounds.** Each review finding is a record.
When work goes back, the implementer answers every blocking finding (fixed,
or not and why), and the next review says of each whether it is addressed
or still open, apart from what it finds new.
- The preview's action card lists them in groups (New, To fix, Still open,
  Fixed but not confirmed, Not followed up, Addressed, Waived). Tick one to
  send it back with Request changes, or waive it with a note.
- `skopos inbox findings --id <item>` lists them with their states and ids.
- `skopos inbox request-changes --id <item> --finding <id>` sends one back
  to be fixed: a nit, or a second opinion's finding, which nobody follows
  up otherwise.
- `skopos inbox waive --id <item> --finding <id> --note "…"` accepts one as
  it is.

See [Review findings](../configuration.md#review-findings).

Mark done doesn't merge. Merge the pull request or the branch yourself: skopos
never merges. With `pull_request = "draft"` in the repo's push config, the
worker opens the pull request when it pushes. Rejecting an item closes its
pull request. See [Pushing and pull requests](../deployment/worker.md#pushing-and-pull-requests).

**Want another pair of eyes first?** Ask for a **second opinion** in the
preview (or `second codex` in Slack).
- **What runs:** one extra review of that exact head, by the runner you
  pick, ideally a different model family from the reviewer.
- **Advisory:** the result is appended to the item, and the status doesn't
  change.
- **Where it shows:** as a Second opinion step after Agent review, with its
  log and findings. If it would block, the sign-off card says so.
- **Also when blocked:** it works when the item is blocked by its review, to
  get a counter-opinion on a disputed finding.
- **Default runner:** set one in the workspace's Pipeline dialog. See
  [Second opinions](../configuration.md#second-opinions-on-demand).

**Blocked?** If an agent needs an answer, the item is `blocked` with its
question. Click **Retry** with your answer; it's appended to the item and the
next run reads it.

**Reject** (the preview's button, with a confirmation; `skopos inbox reject`;
or `reject` in Slack) takes an item out of the pipeline. A run in progress
(planning, implementing, reviewing) stops within about 30 seconds: the
worker learns at its next heartbeat, stops the agent and everything it
started, and pushes or reports nothing more. Its live log ends with
"stopped: item rejected". Mark done stops a run the same way.

Every transition, approval and edit is in the item's timeline
(`GET /api/inbox/{id}/timeline`), labelled with who acted and from where.

## 7. Optional: drive it from Slack

Set these on `skopos serve` (a Socket Mode app, so no public URL is needed):

```bash
SKOPOS_SLACK_BOT_TOKEN=xoxb-...   SKOPOS_SLACK_APP_TOKEN=xapp-...
SKOPOS_SLACK_USER_ID=U...         SKOPOS_SLACK_DEFAULT_WORKSPACE=github.com/you/repo
```

The bot DMs you a thread per item at every gate. A plan-ready message lists
the revision's steps and the planner's questions, one per line with their
lettered options, and `approve` approves exactly that revision. Answer the
questions in the notes (`approve: 1) a, 2) Redis`). A planner that needs
answers before it can plan posts its questions instead of a Blocked notice;
reply `retry: 1) a, 2) …`. If the
plan changed, or the relay restarted and can't know what you read, it posts
the current plan and asks again. You reply with:
- `approve` or `approve: notes` (with several candidates: `approve 2`,
  `approve glm`, `approve 2: notes`);
- `revise 2: notes` to rework one candidate;
- `revise: notes`;
- `changes: notes`;
- `done`;
- `retry: answer`;
- `answer: 1) a, 2) …` (answers the open questions; the item stays where it is);
- `reject`;
- `second` or `second <runner>` for an advisory second opinion on the
  finished branch;
- `findings` for the numbered list of review findings, `changes 2,3: notes`
  or `retry 2: answer` to send findings back by number, `waive 2: why` to
  accept one;
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
- **More models or efforts.** Give each model or effort its own runner name
  and pin stages to it (step 4), e.g. `claude-opus-high` for implement and
  `claude-sonnet-low` for plan. The effort values per CLI are in the
  [worker reference](../deployment/worker.md#effort).
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
| `implement failed: no result after 1h30m0s` | The implementer hit `worker.impl_timeout` (default `90m`). Its finished steps are committed: raise the timeout for a large plan and **Retry**, and the next run continues from the first step not committed |
| `approve` refused as stale | The plan changed after you looked — re-open the item and approve the current revision |
| Watch live / Log shows "No log for this run" | The run ran on a worker older than the server, or before run logs existed. Its raw output is in the worker's `<state_dir>/runs/<run>/` |

Next: the [worker reference](../deployment/worker.md) for every option, and
[configuration](../configuration.md) for all keys and flags.
