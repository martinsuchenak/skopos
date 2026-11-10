# Connecting another system

Another system (a ticketing or job system, say) can hand work to the agent
pipeline and follow it over REST:

1. create the item from its job and start it, in one call;
2. hear what happens to it: webhooks, or the event feed;
3. read the item's state when it needs the details;
4. post people's decisions back: approve the plan, answer questions,
   request changes, sign off.

This guide uses a job system with work requests numbered like `WR110375`.
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
Idempotency-Key: wr-110375-create

{
  "workspace_id": "github.com/acme/app",
  "title": "WR110375 AI configurations",
  "content": "What the job asks for…",
  "author_agent_id": "jobs",
  "ref": "WR110375",
  "external_url": "https://jobs.example/wr/110375",
  "start": "queue"
}
```

- `start`: `queue` runs the pipeline (plan, approval, implement, checks,
  review); `review` reviews an existing branch, named by `branch` or by
  `ref` through the repository's branch template. Without `start` the item
  stays open.
- `ref` is the job's number. The worker's branch template can use it
  (`{ref}`), and `GET /api/inbox?ref=WR110375` finds the job's items.
- `external_url` links the item back to the job; the dashboard shows it on
  the item.
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

Derive the key from what the request does, e.g. `wr-110375-create` or
`wr-110375-approve-<revision id>`.

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

Create the subscription with the root key, or an approver key for its own
workspaces:

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
          "ref": "WR110375", "external_url": "https://jobs.example/wr/110375",
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
| `item.closed` | done or discarded (`outcome`) |
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
| `reviews` | the reviews of the newest reviewed commit, with findings and `passed` |
| `run` | the run in progress, if any |
| `actions` | what this key may do now |

A key limited by capabilities sees `plan` only with `plans:read`, and
`reason`, `implementation`, `reviews` and `run` only with `runs:read`, as
on `/api/plans` and `/api/runs`. The `integration` preset has both.

Each action names the endpoint to POST: `/api/inbox/{id}/<action>`.

| Gate | Actions |
|---|---|
| not started | `queue`, plus `review` when the item has a branch or ref |
| `plan_approval` | `approve`, `request-changes`, `answers`, `reject` |
| `sign_off` | `mark-done`, `request-changes`, `second-opinion` (when none is running), `reject` |
| `blocked` | `retry`, `answers`, `second-opinion` (when its review blocked it), `reject` |
| `failed` | `retry`, `answers`, `reject` |
| agent working | `reject` |

## Act on behalf of a person

A key with the delegate permission names the person deciding:

```http
POST /api/inbox/{id}/approve
Authorization: Bearer sk_…
X-Skopos-On-Behalf-Of: martin@acme.example
Idempotency-Key: wr-110375-approve-01a1…

{"revision_id": "01a1…", "content_hash": "9f2c…"}
```

- The audit log, the approval and any answers record
  `martin@acme.example (key jobs)`; the audit entry also has
  `on_behalf_of`.
- A key without the delegate permission gets `400` for this header.
- `revision_id` and `content_hash` come from `plan.revisions` in the state:
  the approval binds what the person saw.
- Answers go with the decision (`answers` on approve, request-changes and
  retry) or on their own (`POST /api/inbox/{id}/answers`), by question id
  from `questions`.
