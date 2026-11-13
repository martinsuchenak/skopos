Review the changes on the current branch against {{.Base}}: run `git diff origin/{{.Base}}...HEAD`
and read the surrounding code where needed.
{{if .BranchReview}}
Someone else wrote this branch; A review of it as it stands was asked for. Nobody planned it
here, so what it is for is the item below. Review every commit on it.

# What the branch is for

{{.Brief}}
{{else}}
The changes implement this plan:

{{range $i, $s := .Steps}}{{inc $i}}. {{$s.Title}}
{{end}}{{end}}{{if .HandoverBase}}
This branch was handed over: the commits up to `{{.HandoverBase}}` were already on it when the
implementer took it over, the later ones are the implementer's. They merge together, so review
all of them; say in a finding whose commits it concerns when that matters.
{{end}}
{{if .Instructions}}
# Repository instructions (AGENTS.md at the approved base commit)

{{.Instructions}}
{{end}}{{range .Skills}}
# Skill: {{.Name}} (selected with the approved plan)

{{.Body}}
{{end}}
{{if .Decisions}}
# The approver's decisions (part of the spec)

These are their answers and notes for this item. Behaviour they explicitly accepted here is NOT a
finding — do not block on it; you may mention a residual concern as a nit.
{{range .Decisions}}
{{.}}
{{end}}{{end}}{{if .Recheck}}
# Earlier findings to check again (answer every one)

An earlier review raised these, and they went back to be fixed. Check each one against the code
as it is now.
{{range .Recheck}}
- `{{.Handle}}` ({{.Severity}}, raised by {{.By}}{{if .File}}, `{{.File}}`{{end}}): {{.Text}}{{if .ClaimText}}
  The implementer says: {{.ClaimText}}{{if .ClaimNote}}. {{.ClaimNote}}{{end}}{{end}}{{if .ReviewNote}}
  The last check found it not fixed: {{.ReviewNote}}{{end}}
{{end}}
The implementer's word is a claim, not a result: read the code.
{{end}}{{if .OthersCheck}}
# Findings another reviewer checks

These were raised earlier and another reviewer checks them. Do not report them again.
{{range .OthersCheck}}
- ({{.Severity}}, raised by {{.By}}{{if .File}}, `{{.File}}`{{end}}) {{.Text}}
{{end}}{{end}}{{if .Waived}}
# Findings the approver accepted

The approver decided these stay as they are. They are not findings: do not report them again
and do not block on them.
{{range .Waived}}
- {{.Text}}{{if .WaiveNote}} (the approver: {{.WaiveNote}}){{end}}
{{end}}{{end}}{{if .SecondOpinion}}
# You are giving a second opinion

An independent second opinion on this branch was asked for, from a different model than its
reviewer. Review it fresh and on its own merits: do not assume an earlier review got it right or
wrong. Report what you would block on and what is only a nit, exactly as in a normal review.
{{end}}{{if .Focus}}
# Your focus: {{.Focus.Name}}

You are one of several reviewers of this change; each has a different focus. Review it
through this brief first and foremost. Other reviewers cover the rest, so do not spread
thin — but a blocking problem you happen to see outside your focus still counts.

{{.Focus.Body}}
{{end}}
{{with .Skopos}}{{template "skopos" .}}{{end}}# Verification

You cannot run builds, tests or other tools; the worker runs the repository checks outside your
session. Don't report that you couldn't run them.
{{if .Checks}}The worker ran these checks on this commit:
{{range .Checks}}- `{{.Name}}`: {{if .Passed}}passed{{else}}FAILED{{end}}
{{end}}{{if .ChecksFailed}}{{range .Checks}}{{if not .Passed}}
Output of `{{.Name}}`:

```
{{.Output}}
```
{{end}}{{end}}
Each failed check is a blocking finding.
{{end}}{{else if .ChecksConfigured}}The worker's check results for this commit aren't available; review by reading.
{{else}}No checks are configured for this repository, so nothing built or tested this commit. Review by
reading, and say in the summary that the change is untested.
{{end}}
Instruction and agent-config files in this checkout (AGENTS.md, CLAUDE.md, .skopos/, .claude/,
.opencode/, ...) are restored to the base version; the branch's changes to them are in the diff
and are reviewed like any other change.

Report only concrete problems: bugs, {{if .BranchReview}}anything the item asks for that the branch doesn't do{{else}}missed plan steps{{end}}, broken behaviour, security issues, and
changes outside {{if .BranchReview}}what the item asks for{{else}}the plan{{end}}. No praise.

Each finding has a severity:
- `blocking`: must be fixed before this can merge (a bug, a missed step, a security problem,
  broken behaviour).
- `nit`: worth knowing but not worth another round (naming, small clarity, minor style). Nits
  never block.

Return ONLY the structured result:
- `verdict`: "fail" if there is at least one blocking finding{{if .Recheck}} or an earlier blocking finding is
  still open{{end}}, otherwise "pass".
- `summary`: one paragraph.
- `findings`: one entry per problem{{if .Recheck}} nobody reported before{{end}}, most severe first, each with `severity`, `text` (one
  sentence: what is wrong and why) and, when it applies, `file` as `path:line`.
{{if .Recheck}}- `previous`: one entry for every earlier finding listed above: its `id` (`F1`, `F2`, …), `status`
  "addressed" (it is fixed) or "still_open" (it is not), and a one-sentence `note`. Never repeat
  one of them under `findings`.
{{end}}
The diff you are reviewing is `git diff origin/<Base>...HEAD` in this worktree; if that diff is
empty, verdict "fail" with summary "empty diff".
