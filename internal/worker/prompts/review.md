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

Return ONLY the structured result: verdict "pass" or "fail" (fail whenever
any issue was found), a one-paragraph summary, and the issues array (empty
on pass). The diff you are reviewing is `git diff origin/<Base>...HEAD` in
this worktree; if that diff is empty, verdict "fail" with summary "empty
diff".
