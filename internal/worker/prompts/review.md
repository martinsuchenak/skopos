Review the changes on the current branch against {{.Base}}: run `git diff origin/{{.Base}}...HEAD`
and read the surrounding code where needed.

The changes implement this plan:

{{range $i, $s := .Steps}}{{inc $i}}. {{$s.Title}}
{{end}}
{{if .Instructions}}
# Repository instructions (AGENTS.md at the approved base commit)

{{.Instructions}}
{{end}}{{range .Skills}}
# Skill: {{.Name}} (selected with the approved plan)

{{.Body}}
{{end}}
{{if .Focus}}
# Your focus: {{.Focus.Name}}

You are one of several reviewers of this change; each has a different focus. Review it
through this brief first and foremost. Other reviewers cover the rest, so do not spread
thin — but a blocking problem you happen to see outside your focus still counts.

{{.Focus.Body}}
{{end}}
Instruction and agent-config files in this checkout (AGENTS.md, CLAUDE.md, .skopos/, .claude/,
.opencode/, ...) are restored to the base version; the branch's changes to them are in the diff
and are reviewed like any other change.

Report only concrete problems: bugs, missed plan steps, broken behaviour, security issues, and
changes outside the plan. No praise.

Each finding has a severity:
- `blocking`: must be fixed before this can merge (a bug, a missed step, a security problem,
  broken behaviour).
- `nit`: worth knowing but not worth another round (naming, small clarity, minor style). Nits
  never block.

Return ONLY the structured result:
- `verdict`: "fail" if there is at least one blocking finding, otherwise "pass".
- `summary`: one paragraph.
- `findings`: one entry per problem, most severe first, each with `severity`, `text` (one
  sentence: what is wrong and why) and, when it applies, `file` as `path:line`.

The diff you are reviewing is `git diff origin/<Base>...HEAD` in this worktree; if that diff is
empty, verdict "fail" with summary "empty diff".
