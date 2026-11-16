# Templates and schedules

Work that repeats, such as "update the dependencies and fix what breaks",
doesn't have to be typed again each time. Hold it as a **template** and
start items from it.

## Templates

A template is held for a workspace or for a group, like a brief. A
workspace's own template of a name is used before its group's.

| Field | What it sets on the item |
|---|---|
| `title` | the title |
| `brief` | the item's text |
| `tags` | tags, which hint skills to the planner |
| `needs` | the [worker labels](../deployment/worker.md#worker-labels) the item needs |
| `agents` | `panel`, `solo`, or the repository default |
| `auto_approve_plan` | [approve the plan automatically](pipeline.md#approve-the-plan-automatically) when the planner asks no question |

In the title and the text, `{date}`, `{month}`, `{year}` and `{workspace}`
(the repository's short name) are filled in when an item is started.

### Hold a template

- **Dashboard:** Workspaces → Settings → Templates.
- **CLI:**

  ```sh
  skopos workspace template set --workspace github.com/acme/shop --name deps \
    --title "Update the dependencies ({month})" --file deps.md --auto-approve
  skopos group template set --group acme --name deps --title "Update the dependencies" --file deps.md
  skopos workspace template list --workspace github.com/acme/shop
  ```

- **API:** `PUT /api/workspaces/{id}/templates/{name}` (or
  `/api/groups/{id}/…`) with the fields above.

An approver in the workspace's scope holds a workspace's templates; root
holds a group's. Two groups may not hold a template of the same name for a
workspace they share. Every change is in the workspace's or group's audit
trail.

### Start an item from a template

- **Dashboard:** pick it under "Start from a template" when you capture an
  item. A title you type is used instead of the template's, and text you
  type is added to its text.
- **Slack:** `new shop: template deps`, followed by anything to add to its
  text. `#panel`, `#solo`, `#auto` and a `needs:` line work as in any
  capture.
- **CLI:** `skopos inbox add --workspace github.com/acme/shop --template deps
  [--extra "…"] [--queue]`.
- **API:** `POST /api/inbox` with `"template": "deps"` and `"extra"`; any
  other field you give is used instead of the template's.

The item records which template, and which revision of it, it came from.
Starting from a template that approves plans automatically needs the
approver permission.

## Nothing to do

Routine work often finds nothing to do: every dependency is already
current. The planner then says so instead of writing a plan, with what it
checked.

- **An item whose plan is approved automatically** is closed as done
  without changes. Slack says "Nothing to do" with the reason, and other
  systems get `item.closed` with `outcome: nothing_to_do` and the `reason`.
- **Any other item** waits at the plan gate with the reason. Reject it to
  close it, or request changes to ask for a plan anyway.

The item report says "Closed without changes: the planner found nothing to
do".
