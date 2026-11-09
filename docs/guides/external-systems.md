# Connecting another system

Another system (a ticketing or job system, say) can hand work to the agent
pipeline and follow it over REST:

1. create the item from its job and start it, in one call;
2. read the item's state when it wants to know how the work is going;
3. post people's decisions back: approve the plan, answer questions,
   request changes, sign off.

This guide uses a job system with work requests numbered like `WR110375`.
The endpoints are in [openapi.yaml](../../openapi.yaml).

skopos doesn't push notifications to other systems yet: the other system
reads `/api/inbox/{id}/state` (below) on its own schedule.

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
