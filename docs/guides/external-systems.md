# Connecting another system

Another system (a ticketing or job system, say) can hand work to the agent
pipeline and follow it over REST:

1. create the item from its job and start it, in one call;
2. hear what happens to it: webhooks, or the event feed;
3. read the item's state when it needs the details, or show it as a card;
4. post people's decisions back: approve the plan, answer questions,
   request changes, sign off.

This guide uses a job system with jobs numbered like `JOB100235`.
The endpoints and event types are in [openapi.yaml](../../openapi.yaml)
(`webhooks:` lists the events).

## The key

Create one key per system, scoped to the workspaces it files work into:

```bash
skopos key create --name jobs --workspace github.com/acme/app \
  --preset integration --approver --delegate
```

- `--preset integration` limits the key to REST: the inbox (create, start,
  state, decisions) and reads of plans and runs.
- `--approver` lets it start items and record decisions. Without it, the key
  can create items and read their state, and `actions` in the state is
  empty.
- `--delegate` lets it name the person behind each decision (below).
- The preset includes `events:read`, for the event feed.

## Create and start

```http
POST /api/inbox
Authorization: Bearer sk_…
Content-Type: application/json
Idempotency-Key: job-100235-create

{
  "workspace_id": "github.com/acme/app",
  "title": "JOB100235 AI configurations",
  "content": "What the job asks for…",
  "author_agent_id": "jobs",
  "ref": "JOB100235",
  "external_url": "https://jobs.example.com/job/100235",
  "start": "queue"
}
```

- `start`: `queue` runs the pipeline (plan, approval, implement, checks,
  review); `review` reviews an existing branch, named by `branch` or by
  `ref` through the repository's branch template. Without `start` the item
  stays open.
- `ref` is the job's number. The worker's branch template can use it
  (`{ref}`), and `GET /api/inbox?ref=JOB100235` finds the job's items.
- `external_url` links the item back to the job; the dashboard shows it on
  the item.
- `needs` (optional, e.g. `["stock"]`) names the
  [labels](../deployment/worker.md#worker-labels) the item needs of its
  worker: its runs go only to a worker with all of them.
- `auto_approve_plan` (optional, `true`) approves the plan automatically
  when the planner asks no question; the branch still waits for sign-off.
  The key needs the approver permission. See
  [Approve the plan automatically](pipeline.md#approve-the-plan-automatically).
- The answer is `201` with the item. If it was created but couldn't start,
  it's open and `start_error` says why; `POST /api/inbox/{id}/queue` (or
  `/review`) starts it later.

### Retrying safely

Every POST under `/api/inbox` takes an `Idempotency-Key` header. Within 24
hours, the same key with the same request returns the first response
(`Idempotent-Replayed: true`) instead of acting again, so a create retried
after a timeout doesn't make a second item.

- The same key with a different request answers `422`.
- The same key while the first request is still running answers `409`.
- Keys are per API key, and only a successful response is kept: a
  refused request (`4xx`) or a server error (`5xx`) changed nothing, and its
  retry runs.
- The person named by `X-Skopos-On-Behalf-Of` is part of the request.

Derive the key from what the request does, e.g. `job-100235-create` or
`job-100235-approve-<revision id>`.

## Hear what happens: webhooks

skopos POSTs each event to the system's endpoint, signed per
[Standard Webhooks](https://www.standardwebhooks.com).

The server needs an integrations key, which encrypts the webhook secrets,
and its public URL for the links in payloads:

```toml
[server]
public_url = "https://skopos.example.com"

[integrations]
secret_key = "…a long random string…"   # or SKOPOS_INTEGRATIONS_KEY
allow_private = ["10.20.0.0/16"]         # only for an endpoint on a private network
```

Create the subscription in the dashboard's **Webhooks** view (root, or an
approver key for its own workspaces), or with the CLI:

```bash
skopos webhook create --name jobs --url https://jobs.example/hooks/skopos \
  --workspace github.com/acme/app
```

The signing secret (`whsec_…`) is printed once. `--type` limits the event
types; by default the subscription gets every type except `run.started` and
`run.finished`, which it gets only by naming them.

Every delivery is a POST with:

```http
webhook-id: msg_01a1…
webhook-timestamp: 1762678800
webhook-signature: v1,K5oZfzN95Z9UVu1EsfQmfVNQhnkZ2pj9o9NDN/H/pI4=
content-type: application/json

{"id": "msg_01a1…", "type": "item.needs_you", "timestamp": "2026-11-09T09:00:00.000Z",
 "data": {"seq": 812, "item_id": "01a1…", "workspace_id": "github.com/acme/app",
          "ref": "JOB100235", "external_url": "https://jobs.example.com/job/100235",
          "status": "awaiting_approval", "gate": "plan_approval",
          "links": {"api": "https://skopos.example.com/api/inbox/01a1…/state",
                    "dashboard": "https://skopos.example.com/?item=01a1…"}}}
```

The receiver:

- verifies `webhook-signature` with the secret, using a Standard Webhooks
  library, and rejects a `webhook-timestamp` more than 5 minutes off;
- answers any `2xx` quickly and does its work afterwards;
- drops a `webhook-id` it has already handled: a delivery can arrive more
  than once;
- reads `/api/inbox/{id}/state` for the details, since payloads carry ids
  and state only. `data.seq` orders events: one older than an event already
  handled can be ignored.

| Event | When |
|---|---|
| `item.created` | an item was created |
| `item.status_changed` | every status change (`status`, `previous_status`, `actor_kind`, `reason`) |
| `item.needs_you` | the item waits for a person (`gate`: `plan_approval`, `sign_off`, `blocked`, `failed`) |
| `plan.ready` | a plan waits for approval |
| `questions.waiting` / `questions.answered` | a run asked questions / answers were recorded |
| `checks.finished` | an implementation's checks ran (`passed`, `failed_names`) |
| `review.finished` | a review or second opinion reported (`passed`, `blocking`, `nits`) |
| `pr.opened` | an implement run opened or reused a pull request (`url`) |
| `item.closed` | done or discarded (`outcome`: `done`, `discarded`, or `nothing_to_do` with a `reason` when the planner found nothing to do) |
| `run.started` / `run.finished` | a run started / reported (only when named) |

### Failures and retries

- A delivery that doesn't get a `2xx` within 10 seconds is retried after 5
  seconds, 5 minutes, 30 minutes, 2, 5, 10 and 10 hours, then marked failed.
- Redirects aren't followed. Private, loopback and link-local addresses are
  refused unless they're in `integrations.allow_private`; plain http only
  goes to hosts in `integrations.allow_http`.
- Deliveries connect to the endpoint directly: proxy settings
  (`HTTPS_PROXY`, `HTTP_PROXY`) aren't used. The server needs a direct route
  to the endpoint.
- An endpoint answering `410 Gone`, or failing every attempt for 3 days
  (with no 12-hour stretch free of failures in between), gets its
  subscription disabled, with the reason, in the audit log. Events
  wait while it's disabled, for up to `cleanup.retention_days`;
  `skopos webhook enable <id>` delivers those still kept.
- `skopos webhook deliveries <id>` shows each delivery and its attempts;
  `skopos webhook redeliver <id> --delivery <id>` (or `--since 24h` for every
  failed one) sends again with the same `webhook-id`.
- `skopos webhook test <id>` sends a `webhook.test` event.
- The dashboard's Webhooks view does the same: each subscription's state and
  failures, its deliveries with their attempts, Test, Send again, Enable,
  Disable, Rotate secret, Edit and Delete.
- With the Slack relay on, a subscription that disables itself is posted to
  Slack with the reason.
- `skopos webhook rotate <id>` replaces the secret. For 24 hours deliveries
  carry two signatures, one per secret, so the receiver can switch over.

## Hear what happens: the event feed

A system that can't take incoming calls, or that was down longer than the
retries last, reads the same events:

```http
GET /api/events?after=812&wait=25
```

The answer is `{"events": [...], "next_after": 840}`; pass `next_after` back
as `after`. `wait` holds the request open (up to 25 seconds) until an event
arrives. `types=` and `workspace_id=` narrow it; a scoped key sees only its
workspaces' events. Events are kept for `cleanup.retention_days` (events a
subscription still has to deliver are kept until they're delivered). A
cursor older than the oldest kept event answers `410` with `oldest_seq`:
events were missed, so resync from `/api/inbox/{id}/state`.

## Follow the item

`GET /api/inbox/{id}/state` returns everything needed to show the item and
act on it:

| Field | What it holds |
|---|---|
| `turn` | `agent` (work in progress), `person` (someone must act, or start it), `none` (closed) |
| `gate` | `plan_approval`, `sign_off`, `blocked` or `failed` when a person must act |
| `reason` | why a blocked or failed item stopped |
| `brief` | the item's own text, without the pipeline's sections |
| `plan.revisions` | the plan(s) waiting for approval, with steps, `revision_id` and `content_hash` |
| `questions` | open questions, with their options |
| `implementation` | the newest implementation: head commit, checks, setup, pull request |
| `reviews` | the reviews of the newest reviewed commit, with findings and `passed`. `blocking` counts a review's own blocking findings and the earlier ones it found still open (`still_open`) |
| `findings` | the review findings in short: `counts` by state, and `open` — the ones something is still owed on (`open`, `fixed`, `still_open`). Absent when the item has none |
| `run` | the run in progress, if any |
| `actions` | what this key may do now |

A key limited by capabilities sees `plan` only with `plans:read`, and
`reason`, `implementation`, `reviews`, `findings` and `run` only with `runs:read`, as
on `/api/plans` and `/api/runs`. The `integration` preset has both.

`GET /api/inbox/{id}/findings` is the full record of review findings: each
with an id, its state (`open`, `fixed`, `addressed`, `still_open`, `waived`,
`noted`) and what happened to it. `request-changes` and `retry` take
`findings: [ids]` to send findings back to be fixed, and
`POST /api/inbox/{id}/findings/{finding}/waive` accepts one as it is. See
[Review findings](../configuration.md#review-findings).

Each action names the endpoint to POST: `/api/inbox/{id}/<action>`.

| Gate | Actions |
|---|---|
| not started | `queue`, plus `review` when the item has a branch or ref |
| `plan_approval` | `approve`, `request-changes`, `answers`, `reject` |
| `sign_off` | `mark-done`, `request-changes`, `check-commits` (when the branch has commits after the tested one), `second-opinion` (when none is running), `reject` |
| `blocked` | `retry`, `answers`, `second-opinion` (when its review blocked it), `reject` |
| `failed` | `retry`, `answers`, `reject` |
| agent working | `reject` |

## Show the item as a card

A system that shows skopos's item on its own record (the job's page, say)
can ask for a card instead of reading the state and drawing it itself.
The card is UI as data: an [A2UI 1.0](https://a2ui.org) surface built
from the basic catalog only, which the system draws with its own
components. Around it, an envelope says what the system needs to follow
the item without knowing skopos's workflow.

```http
POST /api/inbox/card?ref=JOB100235&workspace=github.com/acme/app
Authorization: Bearer sk_…
Content-Type: application/json

{"subject": {"title": "AI configurations",
             "url": "https://jobs.example.com/job/100235"}}
```

The reference goes in the address (`ref`), so a system set up with an
endpoint template fills in its own record's key
(`/api/inbox/card?ref={key}`); `subject.ref` in the body is taken too, and
both must agree when both are given. The body is optional: `title`, `url`
and `text` (up to 64 KB) only describe the record.

```json
{
  "status": "plan_approval",
  "attention": true,
  "summary": "Plan ready for approval — 2 questions",
  "key": "01a1…:plan_approval:01a1…:01a1…",
  "url": "https://skopos.example.com/?item=01a1…",
  "a2ui": [{"version": "1.0", "deleteSurface": {"surfaceId": "skopos-01a1…"}},
           {"version": "1.0", "createSurface": {"surfaceId": "skopos-01a1…",
    "catalogId": "https://a2ui.org/specification/v1_0/catalogs/basic/catalog.json",
    "components": [{"id": "root", "component": "Column", "children": ["c1", "c2"]}, …]}}]
}
```

- The item is the newest one with that `ref` that is not closed, else the
  newest. With none, the card says so (`status: none`). `workspace` limits
  the lookup to one workspace.
- `status` is one of the names below; map each to a state of your own.
- `attention`: a person must act now.
- `key` changes exactly when something new happened: another item or
  status, a new plan round, a new set of questions, a new reviewed commit,
  another reason to stop. Post a note to your record only when it changes,
  and ignore a repeated one.
- `a2ui` holds two A2UI messages: `deleteSurface`, then `createSurface`
  with every component. Every card of an item has the same surface id, so
  apply both in order to the renderer the card is shown with: the old card
  goes and the new one takes its place. A renderer that has not shown the
  surface yet takes the delete as nothing to do.
- The `Text` components hold Markdown: render it without raw HTML, and
  keep links to `https`.

| `status` | The item |
|---|---|
| `none` | no item has the reference |
| `not_started` | in skopos, waiting for someone to queue it |
| `agent` | the agents are working (one status from queued until the next gate) |
| `plan_approval` | the plan waits for approval: each plan in full as its planner wrote it, with the questions it asked (several plans as tabs, the merged one named so, the judge's comparison above them), and the open questions |
| `blocked` / `failed` | the work stopped: the reason and the open questions |
| `sign_off` | the branch waits for sign-off: the pull request, checks, reviews and the findings still open |
| `done` | signed off (and merged, when the worker reported it) |
| `discarded` | discarded |
| `nothing_to_do` | closed because the planner found nothing to do |

On a webhook, ask for the card again: the event names the item's `ref`
(`data.ref`), so the card endpoint is the one read a system needs to
follow its jobs. The card is built from the same state as
`/api/inbox/{id}/state`: a key limited by capabilities sees the plan only
with `plans:read` and the runs' details only with `runs:read`. Rendering a
card is a read, though it is a `POST`: a key with `inbox:read` may ask for
one, and a card is never replayed for an `Idempotency-Key`: every answer
is the item as it is now.

### Act on the card

For a key that may act on items for a person (`inbox:write` and the
approver permission: the `integration` preset with `--approver`, and
`--delegate` to name the person), the card is the item's page: it draws a
button for each action the item allows now (the `actions` of
`/api/inbox/{id}/state`) and the inputs they take. For any other key the
card stays text.

| Card | Inputs | Buttons (`name` of the action) |
|---|---|---|
| not started | | Send to the agents (`queue`), Review the existing branch (`review`) |
| agents working | a note | Reject (`reject`) |
| `plan_approval` | the open questions; the plan to approve when there are several; a note | Approve (`approve`), Request changes (`request-changes`), Only send the answers (`answers`), Reject (`reject`) |
| `blocked` / `failed` | the open questions; the open findings when a review stopped it; a note | Retry, or Send answers and continue (`retry`), Only send the answers (`answers`), Ask for a second opinion (`second-opinion`), Reject (`reject`) |
| `sign_off` | the open findings to send back; a note | Sign off (`mark-done`), Request changes (`request-changes`), Check the new commits (`check-commits`), Ask for a second opinion (`second-opinion`), Reject (`reject`) |

A card names no runner, so it offers a second opinion only where the
workspace has a default runner for it (`second_opinion_runner` in the
pipeline settings).

Every card also shows where the item is: Plan → Plan approval → Implement
→ Checks → Review → Sign-off, with the steps done and the current one.

The inputs are bound in the surface's data model: a question under
`/answers/<question id>/option` (the picked letter) and `/text`, the plan
under `/plan/choice`, a finding under `/findings/<id>` (ticked = send it
back), the note under `/notes`. When the person presses a button, send
the action your A2UI renderer gives (its `name` and resolved `context`)
back to the same endpoint with the same subject, naming the person:

```http
POST /api/inbox/card?ref=JOB100235&workspace=github.com/acme/app
X-Skopos-On-Behalf-Of: alex@acme.example

{"subject": {},
 "action": {"name": "approve", "surfaceId": "skopos-01a1…", "sourceComponentId": "c12",
            "timestamp": "2026-11-20T10:00:00Z",
            "context": {"item": "01a1…", "key": "01a1…:plan_approval:01a1…",
                        "since": "2026-11-20T09:58:12.031Z",
                        "answers": {"01a1…q": {"option": ["a"], "text": ""}},
                        "notes": "Keep the old report as it is."}}}
```

- The answer is the card after the action.
- Each action is the item's own (`/api/inbox/{id}/<action>`), with its
  checks, recorded as the person the request names, via `card`. Answers go with
  Approve, Request changes and Retry; a note is the approval's notes, what
  to change, the answer to what stopped the work, or why the item is
  rejected. Request changes needs a note or a finding to send back, and
  Reject a note. A question left blank stays open.
- An approval names the plan the card showed: the only one, or the one
  picked. Sign off names the reviewed commit the card showed.
- The context names the item, the card's `key` and `since` (when the item
  entered its status): when something has happened since the card was
  drawn (another item, another status, a new stop, a new stage of the
  agents' work, a new plan round or question set, a new reviewed commit)
  the action is refused with `409`, and nothing is recorded. Draw the card
  again. A double click is refused the same way.
- Open findings are tick boxes only when they can go back to the
  implementer now: at sign-off, and at a stop whose retry starts an
  implementation (`findings.send_back` of the state).
- A key that may not act gets `403`; an unknown action, or one missing
  what it needs, `400`. An action skopos itself refuses answers as its own
  endpoint does: `409` (a second opinion already running, or one from the
  same runner on the same commit), `403` (the runner policy). The `error`
  of a refusal is a sentence to show the person.

## Keep the item's report

`GET /api/inbox/{id}/report` returns the item's record as one document, in
the order things happened:

| Part | What it holds |
|---|---|
| Header | title, workspace, `ref` and `external_url`, status, branch and head commit, pull request |
| Outcome | where the item stands: who signed it off and when, the commit, the findings and questions in numbers |
| Brief | the item's own text |
| Plan | the approved plan in full, who approved it, the revision and its base commit; the plans not chosen |
| Questions and answers | each question, the answer in force, who gave it; corrected and passed-over ones marked |
| Decisions | every action a person took: start, plan approval, change requests, retries, waived findings, second opinions asked for, sign-off, with the notes |
| Implementation | each implement run: commit, diffstat, setup and checks, fix rounds, pull request |
| Reviews and QA | each review, QA run and second opinion: verdict, summary, findings |
| Findings | every finding and what became of it: fixed and confirmed, waived (by whom, why), still open |
| Who worked on it | each run's stage, runner, model (the one the runner reported, else the configured one), provider, executor and worker, and the tokens and cost the runners reported. Runs that recorded no provider, and runs that reported no token counts, are counted |

| Parameter | Values |
|---|---|
| `format` | `md` (the default), `html`, `json` |
| `level` | `full` (the default): every plan, review and section verbatim. `summary`: short enough for a comment |

- `md` and `html` answer with a file to save. The HTML is a fragment of
  plain elements (headings, lists, tables, code), with no styles or scripts.
- `json` is the whole record as a structure, at either level.
- The report is available in any state of the item. Before sign-off, the
  outcome says where the item stands.

Skopos builds the report from the item's records when it is asked for and
stores it nowhere. The server deletes a done or discarded item once it is
older than the retention (`cleanup.retention_days`), and from then on
there is no report of it. A system that keeps reports fetches one when it
receives `item.closed`:

```sh
curl -sS -H "Authorization: Bearer $SKOPOS_KEY" \
  "https://skopos.example.com/api/inbox/$ITEM/report?format=html&level=summary"
```

A key limited by capabilities needs `plans:read` and `runs:read` beside the
inbox; with one missing the answer is `403`. The `integration` preset has
both.

From the command line: `skopos inbox report --id <item> [--level summary]
[--format html] [--out report.md]`.

## Act on behalf of a person

A key with the delegate permission names the person deciding:

```http
POST /api/inbox/{id}/approve
Authorization: Bearer sk_…
X-Skopos-On-Behalf-Of: alex@acme.example
Idempotency-Key: job-100235-approve-01a1…

{"revision_id": "01a1…", "content_hash": "9f2c…"}
```

- The audit log, the approval and any answers record
  `alex@acme.example (key jobs)`; the audit entry also has
  `on_behalf_of`.
- A key without the delegate permission gets `400` for this header.
- `revision_id` and `content_hash` come from `plan.revisions` in the state:
  the approval binds what the person saw.
- Answers go with the decision (`answers` on approve, request-changes and
  retry) or on their own (`POST /api/inbox/{id}/answers`), by question id
  from `questions`.
