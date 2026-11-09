{{define "skopos"}}# Skopos (shared memory{{if .IndexBranch}} and the code index{{end}})

You have the skopos MCP tools for workspace `{{.Workspace}}`. Pass `workspace_id: "{{.Workspace}}"`
to every call.

- First read what earlier agents and Martin recorded: `blackboard_read`{{if .Branch}} with
  `branch: "{{.Branch}}"`{{end}}. Decisions there hold unless the item says otherwise.
{{- if .IndexBranch}}
- Use the code index before grepping or opening files to look around.
  Query branch `{{.IndexBranch}}` (`branch: "{{.IndexBranch}}"`). {{.IndexNote}}
{{- if .Semantic}}
  - `code_find`: describe what you are looking for in words when you don't know its name ("where
    are retries scheduled?", "which handler saves the user's settings?"). Start here to find where
    something lives.
{{- end}}
  - `code_search`: names, signatures and text; `code_symbol`: a definition by its exact name.
  - `code_callers` / `code_impact`: who calls it, and what breaks if it changes.
  Confirm what you rely on in the files themselves.
{{- end}}
- Record what a later agent or Martin should know: a decision and why, a bug you found but did not
  fix, a constraint that was not obvious — `blackboard_write` with `scope: "branch"` and
  `branch_name: "{{if .Branch}}{{.Branch}}{{else}}{{.Base}}{{end}}"`, or `scope: "project"` for repository-wide facts; `entry_type`
  `decision`, `bug`, `finding` or `warning`; and `author_agent_id: "{{.Author}}"`. Be selective: no progress notes, nothing the
  diff or your result already says.
{{- if and .Implement .PlanID}}
- When a step is done and committed, mark it: `plan_update_item` with `plan_id: "{{.PlanID}}"`,
  status `done` and the step's item id:
{{range $i, $s := .Steps}}{{if $s.ID}}  - step {{inc $i}} ({{$s.Title}}): `{{$s.ID}}`
{{end}}{{end}}  Change only the status; the steps' text is Martin's approved plan.
{{- end}}

{{end}}
