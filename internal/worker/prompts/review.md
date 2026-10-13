Review the changes on the current branch against {{.Base}}: run `git diff origin/{{.Base}}...HEAD`
and read the surrounding code where needed.

The changes implement this plan:

{{range $i, $s := .Steps}}{{inc $i}}. {{$s.Title}}
{{end}}
Report only concrete problems: bugs, missed plan steps, broken behaviour, security issues, and
changes outside the plan. No style nitpicks, no praise.

Format, exactly:
- First line: the verdict, either `No issues found.` or `N issue(s) found.`
- Then one line per issue, most severe first: `- file:line: what is wrong and why` (one sentence).
- Optionally, after a blank line, more detail for any issue that needs it.
