# Plans

Plans are shared to-do lists with dependency tracking and automatic blocking/unblocking.

## Plans

A plan has a name, optional branch, description, and status. Plan statuses are **auto-managed**:

| Status | Set by | When |
|--------|--------|------|
| `active` | default | Plan created |
| `completed` | auto | All items `done` |
| `blocked` | auto | Plan depends on an incomplete plan |
| `archived` | manual | User retires the plan |

Manual `active`/`completed`/`blocked` changes are overridden by the automation on the next item/dependency update. Only `archived` is a meaningful manual action (retire a plan; archived/completed plans are eventually cleaned up by the retention worker).

## Items

| Status | Meaning |
|--------|---------|
| `pending` | Ready to work on |
| `in_progress` | Being worked on |
| `done` | Completed |
| `blocked` | Waiting on a dependency |

Items can be claimed by an agent (`claimed_by_agent_id`) to prevent duplicate work.

## Dependencies and auto-blocking

- Adding a dependency (`item A depends on item B`) auto-blocks item A if B is not `done`.
- Completing B auto-unblocks A (if all of A's deps are done).
- Completing the last item auto-completes the plan.
- Plan-to-plan dependencies work the same way: adding one auto-blocks the dependent plan; completing the dependency auto-unblocks it.

**Cycle detection**: the service runs BFS cycle detection inside the same transaction as the dependency insert — cycles are rejected with a 409 Conflict.

**Idempotent**: adding an existing dependency is a no-op (`INSERT OR IGNORE`) — no 500 on duplicates.

## Transactional safety

All multi-step plan operations (add item with deps, add/remove dependency + re-check, update item status + cascade unblock + auto-complete plan) are wrapped in `Store.RunInTx` — a single SQLite transaction. If any step fails, the entire operation rolls back.

## MCP tools

| Tool | Description |
|------|-------------|
| `plan_create` | Create a plan |
| `plan_read` | Get a plan with all items and dependencies |
| `plan_add_item` | Add an item (optionally with deps) |
| `plan_update_item` | Update status / claim |
| `plan_archive` | Soft-delete a plan (set status to `archived`) |
| `plan_add_dependency` | Add an item dependency |
| `plan_remove_dependency` | Remove an item dependency |
| `plan_add_plan_dependency` | Add a plan-to-plan dependency |
| `plan_remove_plan_dependency` | Remove a plan-to-plan dependency |

Hard delete (plan and items) is REST/UI only — there is no hard-delete MCP tool. Use `plan_archive` from agents instead.

## REST endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/plans` | Create plan |
| GET | `/api/plans` | List plans (filter by workspace/branch) |
| GET | `/api/plans/{id}` | Get plan with items |
| PATCH | `/api/plans/{id}` | Update plan (archive) |
| DELETE | `/api/plans/{id}` | Delete plan (cascades items + deps) |
| POST | `/api/plans/{id}/items` | Add item |
| PATCH | `/api/plans/{id}/items/{item_id}` | Update item status/claim |
| DELETE | `/api/plans/{id}/items/{item_id}` | Delete item |
| POST | `/api/plans/{id}/items/{item_id}/dependencies` | Add item dependency |
| DELETE | `/api/plans/{id}/items/{item_id}/dependencies/{dep_id}` | Remove item dependency |
| POST | `/api/plans/{id}/dependencies` | Add plan-to-plan dependency |
| DELETE | `/api/plans/{id}/dependencies/{dep_id}` | Remove plan-to-plan dependency |
