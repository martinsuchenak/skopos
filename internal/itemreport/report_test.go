package itemreport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
)

type fakeItems struct{ item *inbox.Item }

func (f fakeItems) ReadItem(context.Context, string) (*inbox.Item, error) {
	if f.item == nil {
		return nil, inbox.ErrNotFound
	}
	return f.item, nil
}

type fakePlans map[string]*plans.Revision

func (f fakePlans) GetRevision(_ context.Context, id string) (*plans.Revision, error) {
	if rev, ok := f[id]; ok {
		return rev, nil
	}
	return nil, plans.ErrNotFound
}

type fakeRuns []runs.Run

func (f fakeRuns) List(context.Context, runs.Status, string) ([]runs.Run, error) { return f, nil }

type fakeQuestions struct{ rec *qa.Record }

func (f fakeQuestions) Record(context.Context, string) (*qa.Record, error) { return f.rec, nil }

type fakeFindings []findings.Finding

func (f fakeFindings) Item(context.Context, string) ([]findings.Finding, error) { return f, nil }

type fakeApprovals []approvals.Entry

func (f fakeApprovals) List(context.Context, string) ([]approvals.Entry, error) { return f, nil }

// fakeAudit serves the entries newest first, a page at a time, as the
// audit list does.
type fakeAudit []audit.Entry

func (f fakeAudit) List(_ context.Context, flt audit.ListFilter) ([]audit.Entry, error) {
	var out []audit.Entry
	for i := len(f) - 1; i >= 0; i-- {
		if flt.Before != "" && f[i].ID >= flt.Before {
			continue
		}
		if len(flt.Actions) > 0 && !slices.Contains(flt.Actions, f[i].Action) {
			continue
		}
		out = append(out, f[i])
		if len(out) == flt.Limit {
			break
		}
	}
	return out, nil
}

func at(minute int) time.Time { return time.Date(2026, 3, 4, 9, minute, 0, 0, time.UTC) }

const (
	revA = "11111111-1111-7111-8111-111111111111"
	revB = "22222222-2222-7222-8222-222222222222"
)

// content is an item two planners planned (the second plan was approved),
// that was implemented, reviewed and tested.
const content = `Limit requests per client.

## Context

Clients hammer the search endpoint.

## Candidate — claude

**Revision:** ` + revA + `

# Plan A

Count in memory.

## Candidate — codex

**Revision:** ` + revB + `

# Plan B

## Steps

Count in the cache.

` + "```sh\n# not a heading\n```" + `

## Comparison — judge

B survives a restart.

## Approval notes

Go with B.

## review — claude-1

**Verdict:** fail

### Blocking

- counts per process

## Change requests

Fix the counter.

## qa — claude-1

### Scenarios

1. Burst of 20 requests: passed
`

func fixture() (*Service, *inbox.Item) {
	item := &inbox.Item{ID: "item-1", WorkspaceID: "github.com/acme/shop", Title: "Rate limit", Content: content,
		Status: inbox.StatusDone, Ref: "JOB-7", ExternalURL: "https://jobs.example.com/7", Needs: []string{"stock"},
		CreatedAt: at(0), UpdatedAt: at(50)}
	pushed := true
	cost1, cost2 := 0.75, 1.5
	all := fakeRuns{ // newest first, as the runs service lists them
		{ID: "r6", Stage: runs.StageQA, Status: runs.StatusSucceeded, HeadSHA: "bbbbbbbbbbbbbbbb", Runner: "claude", ResultSummary: "works", CreatedAt: at(40), UpdatedAt: at(44),
			Usage: &runs.Usage{Counts: map[string]int64{"input_tokens": 10, "output_tokens": 5}, Models: map[string]map[string]int64{"claude-opus-5-5": {"input_tokens": 10}}}},
		{ID: "r5", Stage: runs.StageReview, Status: runs.StatusSucceeded, HeadSHA: "bbbbbbbbbbbbbbbb", Runner: "claude", Focus: "security",
			ResultSummary: "fixed", CreatedAt: at(35), UpdatedAt: at(38), CostUSD: &cost2,
			Usage: &runs.Usage{Counts: map[string]int64{"input_tokens": 1000, "cache_read_tokens": 20000, "output_tokens": 500, "reasoning_tokens": 50}},
			Facts: []byte(`{"name":"claude","kind":"claude","model":"sonnet","provider":"anthropic","executor":"local"}`)},
		{ID: "r4", Stage: runs.StageImplement, Status: runs.StatusSucceeded, Branch: "agent/rate-limit", HeadSHA: "bbbbbbbbbbbbbbbb", Runner: "claude",
			ResultSummary: "# Done\n\nMoved the counter.", Diffstat: "2 files changed", Pushed: &pushed, PRURL: "https://git.example.com/acme/shop/pull/3",
			Setup:     []runs.Check{{Name: "deps", Passed: true}},
			Checks:    []runs.Check{{Name: "test", Passed: true}, {Name: "lint", Passed: false, Output: strings.Repeat("noise\n", 60) + "lint: ```unused```"}},
			FixRounds: 1, CreatedAt: at(28), UpdatedAt: at(33)},
		{ID: "r3", Stage: runs.StageReview, Status: runs.StatusFailed, HeadSHA: "aaaaaaaaaaaaaaaa", Runner: "claude", Focus: "security",
			ResultSummary: "per process", Findings: []runs.Finding{{ID: "f1", Severity: runs.SeverityBlocking, Text: "counts per process", File: "limit.go:41"}, {ID: "f2", Severity: runs.SeverityNit, Text: "rename | lim"}},
			CreatedAt: at(20), UpdatedAt: at(24), CostUSD: &cost1,
			Facts: []byte(`{"name":"claude","kind":"claude","model":"sonnet","provider":"anthropic"}`), Executor: "wrapper:bwrap:abc"},
		{ID: "r2", Stage: runs.StageImplement, Status: runs.StatusSucceeded, Branch: "agent/rate-limit", HeadSHA: "aaaaaaaaaaaaaaaa", Runner: "claude", CreatedAt: at(12), UpdatedAt: at(18)},
		{ID: "r1", Stage: runs.StagePlan, Status: runs.StatusSucceeded, Runner: "codex", CreatedAt: at(1), UpdatedAt: at(5),
			Facts: []byte(`{"name":"codex","kind":"codex","model":"gpt","provider":"openai"}`)},
		{ID: "r0", Stage: runs.StageReview, Status: runs.StatusRunning, CreatedAt: at(45), UpdatedAt: at(45)},
	}
	rec := &qa.Record{
		Sets: []qa.Set{{ID: "s1", Asker: qa.AskerPlanner, Label: "codex", CreatedAt: at(5), Questions: []qa.Question{
			{ID: "q1", N: 1, Text: "Per client or per key?", Options: []string{"client", "key"}, State: qa.StateAnswered, Answers: []qa.Answer{
				{Kind: qa.KindAnswer, Option: "key", Actor: "alex", Via: "slack", CreatedAt: at(6), Replaced: true},
				{Kind: qa.KindAnswer, Option: "client", Text: "keys rotate", Actor: "alex", Via: "dashboard", CreatedAt: at(7)},
			}},
			{ID: "q2", N: 2, Text: "Which window?", State: qa.StateSkipped, Answers: []qa.Answer{{Kind: qa.KindSkipped, Actor: "alex", CreatedAt: at(8)}}},
		}}},
		Free: []qa.Answer{{Kind: qa.KindGeneral, Text: "Keep it small.", Actor: "alex", Via: "slack", CreatedAt: at(9)}},
	}
	fs := fakeFindings{
		{ID: "f1", N: 1, Stage: runs.StageReview, Focus: "security", Severity: runs.SeverityBlocking, Text: "counts per process", File: "limit.go:41",
			State: findings.StateAddressed, Tracked: true, CreatedAt: at(24)},
		{ID: "f2", N: 2, Stage: runs.StageReview, Focus: "security", Severity: runs.SeverityNit, Text: "rename | lim",
			State: findings.StateWaived, CreatedAt: at(24), Updates: []findings.Update{{Kind: findings.KindWaived, Actor: "alex", Note: "a matter of taste"}}},
		{ID: "f0", N: 1, Stage: runs.StageReview, Severity: runs.SeverityNit, Text: "an old one", Source: findings.SourceImported, State: findings.StateNoted, CreatedAt: at(2)},
	}
	approved := fakeApprovals{ // newest first
		{Gate: approvals.GateReview, Decision: approvals.DecisionApproved, Subject: "bbbbbbbbbbbbbbbb", Actor: "alex", CreatedAt: at(50)},
		{Gate: approvals.GatePlan, Decision: approvals.DecisionApproved, Subject: revB + "+0a1b2c3d4e5f6a7b", Actor: "alex", Notes: "Go with B.", CreatedAt: at(10)},
		// An approval of plan A whose status change failed: it has a
		// record and no audit entry, and must not lend its subject.
		{Gate: approvals.GatePlan, Decision: approvals.DecisionApproved, Subject: revA + "+0a1b2c3d4e5f6a7b", Actor: "alex", CreatedAt: at(8)},
	}
	timeline := fakeAudit{
		{ID: "a01", Action: "inbox.queue", Actor: "alex", Via: "slack", CreatedAt: at(0)},
		{ID: "a02", Action: "inbox.transition", Actor: "system", CreatedAt: at(1)},
		{ID: "a03", Action: "inbox.edit", Actor: "key worker", Notes: "appended a section", CreatedAt: at(5)},
		{ID: "a04", Action: "inbox.approve", Actor: "alex", Via: "dashboard", Notes: "Go with B.", CreatedAt: at(10)},
		{ID: "a05", Action: "inbox.request_changes", Actor: "alex", Via: "dashboard", Notes: "Fix the counter.\nAnd the name.", CreatedAt: at(26)},
		{ID: "a06", Action: "inbox.findings_send_back", Actor: "alex", Notes: "1 sent back to be fixed: f1", CreatedAt: at(26)},
		{ID: "a07", Action: "inbox.finding_waive", Actor: "alex", Via: "dashboard", Notes: "f2 (nit, review · security) a matter of taste", CreatedAt: at(46)},
		{ID: "a08", Action: "inbox.mark_done", Actor: "alex", Via: "dashboard", Notes: "findings at sign-off: 1 waived (f2); 1 addressed", CreatedAt: at(50)},
	}
	svc := &Service{
		Items: fakeItems{item}, Runs: all, Questions: fakeQuestions{rec}, Findings: fs, Approvals: approved, Audit: timeline,
		Plans: fakePlans{revB: {ID: revB, RevisionNo: 2, Steps: []plans.RevisionStep{{Title: "Add the counter"}, {Title: "Wire it in"}}}},
		Now:   func() time.Time { return at(55) },
	}
	return svc, item
}

func build(t *testing.T, svc *Service) *Report {
	t.Helper()
	r, err := svc.Build(context.Background(), "item-1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustHold(t *testing.T, name, doc string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(doc, want) {
			t.Fatalf("%s lacks %q:\n%s", name, want, doc)
		}
	}
}

func mustLack(t *testing.T, name, doc string, nots ...string) {
	t.Helper()
	for _, not := range nots {
		if strings.Contains(doc, not) {
			t.Fatalf("%s holds %q:\n%s", name, not, doc)
		}
	}
}

// The report's record: the approved plan is the one the approval names,
// decisions are a person's actions in order with what each approval named,
// runs are oldest first with the provider the worker reported.
func TestBuild(t *testing.T) {
	svc, _ := fixture()
	r := build(t, svc)

	if r.Item.Branch != "agent/rate-limit" || r.Item.HeadSHA != "bbbbbbbbbbbbbbbb" || r.Item.PullRequest == "" {
		t.Fatalf("the header's branch, head and pull request come from the newest implementation: %+v", r.Item)
	}
	if r.Brief != "Limit requests per client.\n\n## Context\n\nClients hammer the search endpoint." {
		t.Fatalf("brief: %q", r.Brief)
	}
	p := r.Plan
	if p == nil || p.Approved == nil || p.Approved.Title != "Candidate 2 · codex" || p.RevisionNo != 2 || p.BaseSHA != "0a1b2c3d4e5f6a7b" {
		t.Fatalf("the approved plan: %+v", p)
	}
	if len(p.Others) != 1 || p.Others[0].Title != "Candidate 1 · claude" || len(p.Notes) != 1 || strings.Join(p.Steps, ",") != "Add the counter,Wire it in" {
		t.Fatalf("the other plans, the comparison and the steps: %+v", p)
	}

	var actions []string
	for _, d := range r.Decisions {
		actions = append(actions, d.Action)
	}
	if got := strings.Join(actions, ","); got != "queue,approve,request_changes,finding_waive,mark_done" {
		t.Fatalf("decisions: %s", got)
	}
	if d := r.Decisions[1]; d.Subject != revB+"+0a1b2c3d4e5f6a7b" || d.Via != "dashboard" {
		t.Fatalf("the plan approval names its revision and base: %+v", d)
	}
	if d := r.Decisions[4]; d.Subject != "bbbbbbbbbbbbbbbb" || d.Notes != "findings at sign-off: 1 waived (finding 3); 1 addressed" {
		t.Fatalf("the sign-off names its commit, and findings by their number: %+v", d)
	}

	if len(r.Implementations) != 2 || r.Implementations[1].RunID != "r4" || len(r.Reviews) != 3 || r.Reviews[0].RunID != "r3" {
		t.Fatalf("implementations and reviews, oldest first, finished runs only: %+v %+v", r.Implementations, r.Reviews)
	}
	lint := r.Implementations[1].Checks[1]
	if lines := strings.Count(lint.Output, "\n") + 1; lines != maxCheckLines || !strings.HasSuffix(lint.Output, "lint: ```unused```") {
		t.Fatalf("a failed check keeps the end of its output: %d lines", lines)
	}
	if rv := r.Reviews[0]; rv.Passed || rv.Blocking != 1 || rv.Nits != 1 {
		t.Fatalf("the first review: %+v", rv)
	}
	if len(r.Runs) != 7 || r.Runs[0].ID != "r1" || r.Runs[0].Provider != "openai" || r.Runs[2].Executor != "wrapper:bwrap:abc" || r.Runs[4].Executor != "local" {
		t.Fatalf("runs: %+v", r.Runs)
	}
	if r.CostUSD == nil || *r.CostUSD != 2.25 {
		t.Fatalf("cost: %v", r.CostUSD)
	}
	if r.Usage == nil || r.Usage.Counts["input_tokens"] != 1010 || r.Usage.Counts["cache_read_tokens"] != 20000 || len(r.Runs[5].ModelsUsed) != 1 || r.Runs[5].ModelsUsed[0] != "claude-opus-5-5" {
		t.Fatalf("usage: %+v, %+v", r.Usage, r.Runs[5])
	}
	if len(r.Findings) != 3 || !r.Findings[0].Earlier || r.Findings[2].Outcome != "waived by alex: a matter of taste" {
		t.Fatalf("findings: %+v", r.Findings)
	}
	q := r.Questions[0].Questions[0]
	if q.Answer == nil || q.Answer.Text != "client — keys rotate" || len(q.Replaced) != 1 || q.Replaced[0].Text != "key" {
		t.Fatalf("an answer and the one it corrected: %+v", q)
	}
	want := []string{
		"Signed off by alex on 2026-03-04 (dashboard).",
		"The work is at commit bbbbbbbbbb on branch agent/rate-limit.",
		"2 findings: 1 fixed and confirmed, 1 waived.",
		"2 questions asked: 1 answered, 1 passed over.",
	}
	if strings.Join(r.Outcome, "\n") != strings.Join(want, "\n") {
		t.Fatalf("outcome: %q", r.Outcome)
	}
	var kinds []string
	for _, s := range r.Sections {
		kinds = append(kinds, s.Kind+":"+s.Heading)
	}
	if got := strings.Join(kinds, ","); got != "review:review — claude-1,review:Change requests,qa:qa — claude-1" {
		t.Fatalf("sections as written: %s", got)
	}
}

// The full report holds everything verbatim, with an agent's headings below
// the report's own; the summary holds the short form of the same parts.
func TestMarkdown(t *testing.T) {
	svc, _ := fixture()
	r := build(t, svc)
	full := Markdown(r, LevelFull)
	mustHold(t, "the full report", full,
		"# Rate limit\n\n_Item report · generated 2026-03-04 09:55 UTC_",
		"- **Reference:** [JOB-7](https://jobs.example.com/7)",
		"- **Branch:** `agent/rate-limit` at `bbbbbbbbbb`",
		"- **Needs a worker with:** stock",
		"## Brief\n\nLimit requests per client.\n\n### Context",
		"Approved by alex on 2026-03-04 09:10 UTC (dashboard): revision 2, on base commit `0a1b2c3d4e`.\n\n> Go with B.",
		"### Candidate 2 · codex, approved\n\n**Revision:** "+revB+"\n\n#### Plan B\n\n##### Steps",
		"```sh\n# not a heading\n```",
		"### Candidate 1 · claude, not chosen\n\n**Revision:** "+revA+"\n\n#### Plan A",
		"### Comparison — judge\n\nB survives a restart.",
		"### Asked by codex on 2026-03-04 09:05 UTC\n\n1. Per client or per key?\n   - Options: (a) client; (b) key\n   - Answer: **client — keys rotate** — alex, 2026-03-04 09:07 UTC (dashboard)\n   - Corrected; before that: key — alex, 2026-03-04 09:06 UTC (slack)\n2. Which window?\n   - passed over at the gate",
		"### Said without a question\n\n- Keep it small. — alex, 2026-03-04 09:09 UTC (slack)",
		"- 2026-03-04 09:26 UTC · Changes requested by alex (dashboard)\n\n  > Fix the counter.\n  > And the name.",
		"### Implementation 2 · 2026-03-04 09:33 UTC\n\n- claude: succeeded\n- Commit `bbbbbbbbbb`: 2 files changed\n- Setup: 1 passed\n- Checks: 1 passed, 1 failed (lint); 1 fix round to get there\n- Pull request: <https://git.example.com/acme/shop/pull/3>\n\n#### Done\n\nMoved the counter.",
		"The end of `lint`'s output:\n\n````\n",
		"lint: ```unused```\n````",
		"### Review · security · claude · commit `aaaaaaaaaa` · 2026-03-04 09:24 UTC\n\nVerdict: 1 blocking finding, 1 nit.",
		"- **blocking** counts per process (`limit.go:41`)",
		"| 2 | blocking | review · security | counts per process (`limit.go:41`) | fixed, confirmed by a review |",
		`| 3 | nit | review · security | rename \| lim | waived by alex: a matter of taste |`,
		"| 1 | nit | review | an old one | from an earlier run; not followed |",
		"## Reviews, QA and amendments as written\n\n### review — claude-1\n\n**Verdict:** fail\n\n#### Blocking",
		"### qa — claude-1\n\n#### Scenarios",
		"| review · security | claude | sonnet | anthropic | wrapper:bwrap:abc | ",
		"- Providers that received the code: openai (codex: gpt), anthropic (claude: sonnet)",
		"- Cost as the runners reported it: $2.25\n- Tokens as the runners reported them: 21k in (20k from the cache), 505 out (50 reasoning)\n",
		"| 21k in · 500 out | $1.50 |",
		"| claude | claude-opus-5-5 | ",
	)
	// Nothing an agent wrote sits at the level of the report's own parts.
	for _, line := range strings.Split(full, "\n") {
		if strings.HasPrefix(line, "## ") && !strings.Contains("## Outcome ## Brief ## Plan ## Questions and answers ## Decisions ## Implementation ## Reviews and QA ## Findings ## Reviews, QA and amendments as written ## Who worked on it", line) {
			t.Fatalf("a second-level heading that is not the report's: %q", line)
		}
		if strings.HasPrefix(line, "# ") && line != "# Rate limit" && line != "# not a heading" {
			t.Fatalf("a first-level heading that is not the title: %q", line)
		}
	}

	summary := Markdown(r, LevelSummary)
	mustHold(t, "the summary", summary,
		"_Item report, summary · generated 2026-03-04 09:55 UTC_",
		"1. Add the counter\n2. Wire it in\n\nNot chosen: Candidate 1 · claude.",
		"- Per client or per key? (codex) → **client — keys rotate**\n- Which window? (codex) → passed over at the gate\n- Keep it small. — alex",
		"- 2026-03-04 09:26 UTC · Changes requested by alex (dashboard): Fix the counter. And the name.",
		"The newest of 2 implementation runs:\n\n- claude: succeeded, 2026-03-04 09:33 UTC",
		"## Brief\n\nLimit requests per client.\n\n## Plan",
		"Of commit `bbbbbbbbbb`, the newest reviewed (the full report has 1 earlier review):\n\n- Review · security · claude: passed\n- QA · claude: passed",
		"2 findings: 1 fixed and confirmed, 1 waived.\n\n- rename | lim (nit, review · security): waived by alex: a matter of taste",
		"- Providers that received the code: openai (codex: gpt), anthropic (claude: sonnet)\n- Runs: 7\n- Cost as the runners reported it: $2.25\n- Tokens as the runners reported them: 21k in",
	)
	mustLack(t, "the summary", summary, "Plan B", "Plan A", "### ", "| # |", "an old one", "noise", "#### Scenarios")
	if Markdown(r, LevelFull) != full {
		t.Fatal("the same report gives another text")
	}
}

// An item in progress, with no plan approved and nothing reviewed: the
// report says where it stands and leaves the empty parts out.
func TestInProgress(t *testing.T) {
	item := &inbox.Item{ID: "item-2", WorkspaceID: "github.com/acme/shop", Status: inbox.StatusAwaitingApproval,
		Content: "Do it.\n\n## Enrichment — skopos worker\n\nThe plan.", CreatedAt: at(0), UpdatedAt: at(3)}
	svc := &Service{Items: fakeItems{item}, Runs: fakeRuns{{ID: "r1", Stage: runs.StagePlan, Status: runs.StatusSucceeded, CreatedAt: at(1), UpdatedAt: at(2)}},
		Now: func() time.Time { return at(4) }}
	r := build(t, svc)
	if r.Plan == nil || r.Plan.Approved != nil || len(r.Plan.Others) != 1 || len(r.Outcome) != 1 || r.Outcome[0] != "Not finished: the item is awaiting approval." {
		t.Fatalf("report: %+v plan %+v", r, r.Plan)
	}
	full := Markdown(r, LevelFull)
	mustHold(t, "the report", full, "# Item item-2", "## Plan\n\nNo plan is approved yet.\n\n### Plan, not approved\n\nThe plan.")
	mustLack(t, "the report", full, "## Decisions", "## Implementation", "## Reviews", "## Findings", "## Questions")
	mustHold(t, "the summary", Markdown(r, LevelSummary), "No plan is approved yet.\n\nWritten so far: Plan.")

	// A plan that does not name its revision is the approved one when its
	// round has no other; without the audit log the approvals are listed.
	svc.Approvals = fakeApprovals{{Gate: approvals.GatePlan, Decision: approvals.DecisionApproved, Subject: revA + "+abc", Actor: "alex", Via: "cli", CreatedAt: at(3)}}
	r = build(t, svc)
	if r.Plan.Approved == nil || len(r.Plan.Others) != 0 || len(r.Decisions) != 1 || r.Decisions[0].Action != ActionApprove {
		t.Fatalf("plan %+v decisions %+v", r.Plan, r.Decisions)
	}
	mustHold(t, "the report", Markdown(r, LevelFull), "### Plan, approved\n\nThe plan.", "· Plan approved by alex (cli), revision `"+revA[:10]+"` on base `abc`")
}

// A person's actions beyond one page of the audit list are read whole,
// other entries are not read at all, and past what a report reads the
// oldest are left out and the report says so.
func TestTimelinePages(t *testing.T) {
	item := &inbox.Item{ID: "item-3", WorkspaceID: "ws", Status: inbox.StatusInReview}
	id := func(i int) string { return fmt.Sprintf("a%06d", i) }
	timeline := fakeAudit{{ID: id(0), Action: "inbox.queue", Actor: "alex"}}
	for i := 1; i <= 2*auditPage+5; i++ {
		action := "inbox.edit"
		if i%2 == 0 {
			action = "inbox.answer"
		}
		timeline = append(timeline, audit.Entry{ID: id(i), Action: action, Actor: "alex"})
	}
	timeline = append(timeline, audit.Entry{ID: id(900000), Action: "inbox.request_changes", Actor: "alex"})
	r, err := (&Service{Items: fakeItems{item}, Audit: timeline}).Build(context.Background(), "item-3")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.Decisions); n != auditPage+4 || r.Decisions[0].Action != ActionQueue || r.Decisions[n-1].Action != ActionRequestChange || r.DecisionsCut {
		t.Fatalf("%d decisions, cut %v, first %+v", n, r.DecisionsCut, r.Decisions[0])
	}

	for i := 1; i <= maxAuditPages*auditPage; i++ {
		timeline = append(timeline, audit.Entry{ID: id(1000000 + i), Action: "inbox.answer", Actor: "alex"})
	}
	r, err = (&Service{Items: fakeItems{item}, Audit: timeline}).Build(context.Background(), "item-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Decisions) != maxAuditPages*auditPage || !r.DecisionsCut || r.Decisions[0].Action == ActionQueue {
		t.Fatalf("%d decisions, cut %v", len(r.Decisions), r.DecisionsCut)
	}
	mustHold(t, "the report", Markdown(r, LevelSummary), "## Decisions\n\nThe item has more decisions than a report reads; the oldest are not shown.")
}

// An approved amendment names a revision no plan section carries: the plan
// it amends stays the approved one, with the amended steps beside it.
func TestAmendedPlan(t *testing.T) {
	const revC = "33333333-3333-7333-8333-333333333333"
	svc, _ := fixture()
	svc.Approvals = append(fakeApprovals{{Gate: approvals.GatePlan, Decision: approvals.DecisionApproved, Subject: revC + "+0a1b2c3d4e5f6a7b", Actor: "alex", CreatedAt: at(30)}}, svc.Approvals.(fakeApprovals)...)
	svc.Audit = append(svc.Audit.(fakeAudit)[:5:5], append(fakeAudit{{ID: "a055", Action: "inbox.approve", Actor: "alex", Via: "slack", Notes: "Fine.", CreatedAt: at(30)}}, svc.Audit.(fakeAudit)[5:]...)...)
	svc.Plans.(fakePlans)[revC] = &plans.Revision{ID: revC, RevisionNo: 3, Steps: []plans.RevisionStep{{Title: "Add the counter"}, {Title: "Wire it in"}, {Title: "Expire old counts"}}}
	r := build(t, svc)
	p := r.Plan
	if p.Approved == nil || p.Approved.Title != "Candidate 2 · codex" || !p.Amended || p.RevisionID != revC || p.RevisionNo != 3 || len(p.Others) != 1 || len(p.Steps) != 3 {
		t.Fatalf("plan: %+v", p)
	}
	full := Markdown(r, LevelFull)
	mustHold(t, "the report", full,
		"Approved by alex on 2026-03-04 09:30 UTC (slack): revision 3, on base commit `0a1b2c3d4e`.\n\n> Fine.\n\nThat revision amends the plan approved before it; the steps are the amended ones.",
		"### Candidate 2 · codex, approved", "### Candidate 1 · claude, not chosen",
		"### The steps as amended\n\n1. Add the counter\n2. Wire it in\n3. Expire old counts",
	)
	mustLack(t, "the report", full, "Candidate 2 · codex, not chosen")
}

// A review-only item has no implementation: its head is the newest
// reviewed commit, not the first.
func TestReviewOnlyHead(t *testing.T) {
	item := &inbox.Item{ID: "item-4", WorkspaceID: "ws", Status: inbox.StatusInReview, Flow: "review", Branch: "feature/x"}
	all := fakeRuns{
		{ID: "r2", Stage: runs.StageBranchReview, Status: runs.StatusSucceeded, HeadSHA: "bbbbbbbbbbbb", Branch: "feature/x", WorkerID: "w1", CreatedAt: at(9), UpdatedAt: at(10)},
		{ID: "r1", Stage: runs.StageBranchReview, Status: runs.StatusFailed, HeadSHA: "aaaaaaaaaaaa", Branch: "feature/x", WorkerID: "w1", CreatedAt: at(1), UpdatedAt: at(2),
			Facts: []byte(`{"kind":"claude","model":"sonnet","provider":"anthropic"}`)},
	}
	r, err := (&Service{Items: fakeItems{item}, Runs: all}).Build(context.Background(), "item-4")
	if err != nil {
		t.Fatal(err)
	}
	if r.Item.HeadSHA != "bbbbbbbbbbbb" {
		t.Fatalf("head: %s", r.Item.HeadSHA)
	}
	// One of the two runs recorded no provider: the list says so.
	mustHold(t, "the report", Markdown(r, LevelSummary), "- Providers that received the code: anthropic (claude: sonnet)\n- 1 run did not record a provider\n- Runs: 2")
}

// fakeRunDetail serves the runs and, for GetFull, what each observed.
type fakeRunDetail struct {
	fakeRuns
	observed map[string]*runs.Observed
	gone     map[string]bool // removed since the list was read
}

func (f fakeRunDetail) GetFull(_ context.Context, id string) (*runs.Run, error) {
	if f.gone[id] {
		return nil, runs.ErrNotFound
	}
	for _, r := range f.fakeRuns {
		if r.ID == id {
			r.Observed = f.observed[id]
			return &r, nil
		}
	}
	return nil, runs.ErrNotFound
}

// What the runs say they did is listed under each, and counted in the
// summary.
func TestWorkLists(t *testing.T) {
	item := &inbox.Item{ID: "item-5", WorkspaceID: "ws", Status: inbox.StatusInReview}
	src := fakeRunDetail{
		fakeRuns: fakeRuns{
			{ID: "q1", Stage: runs.StageQA, Status: runs.StatusSucceeded, HeadSHA: "cccccccccccc", CreatedAt: at(5), UpdatedAt: at(6)},
			{ID: "i1", Stage: runs.StageImplement, Status: runs.StatusSucceeded, HeadSHA: "cccccccccccc", CreatedAt: at(1), UpdatedAt: at(2)},
			{ID: "v0", Stage: runs.StageReview, Status: runs.StatusSucceeded, HeadSHA: "bbbbbbbbbbbb", CreatedAt: at(0), UpdatedAt: at(1)},
		},
		gone: map[string]bool{"v0": true},
		observed: map[string]*runs.Observed{
			"i1": {Work: []runs.WorkEntry{{Name: "go test ./...", Result: "pass"}, {Name: "end to end", Result: "skipped", Note: "no instance"}}},
			"q1": {Work: []runs.WorkEntry{
				{Name: "edit customer 3", Result: "pass", Note: "the change log lists the new name"},
				{Name: "a caller without permissions", Result: "skipped", Note: "no second user"},
				{Name: "login totals", Result: "pass"},
			}},
		},
	}
	r, err := (&Service{Items: fakeItems{item}, Runs: src}).Build(context.Background(), "item-5")
	if err != nil {
		t.Fatal(err)
	}
	mustHold(t, "the full report", Markdown(r, LevelFull),
		"- The implementer's verifications: 1 passed, 1 skipped\n",
		"**What the implementer verified**\n\n- go test ./...: **passed**\n- end to end: **skipped** — no instance\n",
		"**Scenarios**\n\n- edit customer 3: **passed** — the change log lists the new name\n- a caller without permissions: **skipped** — no second user\n- login totals: **passed**\n",
	)
	mustHold(t, "the summary", Markdown(r, LevelSummary), "- QA: passed (3 scenarios: 2 passed, 1 skipped)\n")
}

func TestNest(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"the highest heading goes below the report's", "# A\n\ntext\n\n## B", "### A\n\ntext\n\n#### B"},
		{"deeper headings stay", "#### A", "#### A"},
		{"not past six", "# A\n###### B", "### A\n###### B"},
		{"a fence is left alone", "~~~\n# x\n~~~\n# A", "~~~\n# x\n~~~\n### A"},
		{"a longer fence closes only on its own length", "````\n```\n# x\n````\n## A", "````\n```\n# x\n````\n### A"},
		{"not a heading", "#hashtag\n    # code", "#hashtag\n    # code"},
		{"underlined headings move too", "Title\n=====\n\ntext\n\nPart\n----", "### Title\n\ntext\n\n#### Part"},
		{"a rule under a list, a quote or a table row stays a rule", "- item\n---\n> said\n---\na | b\n---", "- item\n---\n> said\n---\na | b\n---"},
		{"an underline inside a fence is text", "```\nTitle\n=====\n```", "```\nTitle\n=====\n```"},
	} {
		if got := nest(tc.in, 2); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The summary's brief is the first paragraph of text, never a heading.
func TestFirstParagraph(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Limit requests\nper client.\n\nMore.", "Limit requests per client."},
		{"## Context\n\nClients hammer it.\n\n## Goal", "Clients hammer it."},
		{"Context\n-------\nClients hammer it.", "Clients hammer it."},
		{"# Only a title", "Only a title"},
		{"", ""},
	} {
		if got := firstParagraph(tc.in); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.in, got, tc.want)
		}
	}
	r := &Report{Brief: "## Context\n\nClients hammer it."}
	mustHold(t, "the summary", Markdown(r, LevelSummary), "## Brief\n\nClients hammer it.\n")
	mustLack(t, "the summary", Markdown(r, LevelSummary), "Context")
}

// The endpoint: formats and levels, the file's name, and who may read it.
func TestHandler(t *testing.T) {
	svc, _ := fixture()
	h := NewHandler(svc)
	get := func(query string, p *auth.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/inbox/item-1/report"+query, nil)
		req.SetPathValue("id", "item-1")
		if p != nil {
			req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		}
		w := httptest.NewRecorder()
		h.Get(w, req)
		return w
	}
	w := get("", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || w.Header().Get("Content-Disposition") != `attachment; filename="job-7-report.md"` {
		t.Fatalf("markdown: %d %v", w.Code, w.Header())
	}
	w = get("?format=html&level=summary", nil)
	if w.Code != 200 || w.Header().Get("Content-Disposition") != `attachment; filename="job-7-summary.html"` {
		t.Fatalf("html: %d %v", w.Code, w.Header())
	}
	mustHold(t, "the HTML", w.Body.String(), "<h1>Rate limit</h1>", `<a href="https://jobs.example.com/7" rel="nofollow">JOB-7</a>`, "<h2>Findings</h2>")
	mustLack(t, "the HTML", w.Body.String(), "<script", "<style", "<html")
	if w = get("?format=json", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":["Signed off by alex`) {
		t.Fatalf("json: %d %s", w.Code, w.Body.String())
	}
	for _, q := range []string{"?format=pdf", "?level=short"} {
		if w = get(q, nil); w.Code != 400 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}

	caps := func(list ...string) *auth.Principal {
		p := &auth.Principal{KeyID: "k", Capabilities: map[string]struct{}{}}
		for _, c := range list {
			p.Capabilities[c] = struct{}{}
		}
		return p
	}
	if w = get("", caps("rest", "inbox:read", "plans:read")); w.Code != 403 {
		t.Fatalf("a key without runs:read: %d", w.Code)
	}
	if w = get("", caps("rest", "inbox:read", "runs:read")); w.Code != 403 {
		t.Fatalf("a key without plans:read: %d", w.Code)
	}
	if w = get("", caps("rest", "inbox:write", "plans:read", "runs:read")); w.Code != 200 {
		t.Fatalf("a key with the three: %d %s", w.Code, w.Body.String())
	}
	if w = get("", &auth.Principal{KeyID: "run:r1", Run: &auth.RunIdentity{ID: "r1"}}); w.Code != 403 {
		t.Fatalf("a run's token: %d", w.Code)
	}
	if w = httptest.NewRecorder(); true {
		req := httptest.NewRequest(http.MethodGet, "/api/inbox/gone/report", nil)
		NewHandler(&Service{Items: fakeItems{}}).Get(w, req)
		if w.Code != 404 {
			t.Fatalf("an item that is not there: %d", w.Code)
		}
	}
}

// The count: an automatic approval followed by the person's own approval
// (an amendment) counts as input before the sign-off; holds are sorted by
// reason; entries before the period are left out.
func TestSummary(t *testing.T) {
	e := func(id, item, action, via, notes string, minute int) audit.Entry {
		return audit.Entry{ID: id, WorkspaceID: "ws", EntityType: "inbox_item", EntityID: item, Action: action, Via: via, Notes: notes, CreatedAt: at(minute)}
	}
	log := fakeAudit{
		e("a01", "old", "inbox.approve", "auto", "", 1), // before the period
		e("a02", "i1", "inbox.approve", "auto", "", 10),
		e("a03", "i1", "inbox.mark_done", "", "", 11),
		e("a04", "i2", "inbox.approve", "auto", "", 12),
		e("a05", "i2", "inbox.approve", "dashboard", "", 13), // an amendment approved by the person
		e("a06", "i2", "inbox.mark_done", "", "", 14),
		e("a07", "i3", "inbox.auto_approve_held", "system", "the planner asked 2 questions", 15),
		e("a08", "i3", "inbox.auto_approve_held", "system", "you asked for this plan again", 16),
		e("a09", "i4", "inbox.approve", "auto", "", 17),
		e("a10", "i5", "inbox.nothing_to_do", "system", "current", 18),
		e("a11", "i6", "inbox.edit", "", "", 19),
	}
	s, err := (&Service{Audit: log}).Summary(context.Background(), at(5), "")
	if err != nil {
		t.Fatal(err)
	}
	tot := s.Total
	if tot.AutoApproved != 3 || tot.NoInputAfter != 1 || tot.InputAfter != 1 || tot.Open != 1 || tot.NothingToDo != 1 ||
		tot.Held["questions"] != 1 || tot.Held["replan"] != 1 {
		t.Fatalf("total: %+v", tot)
	}
	if len(s.Rows) != 1 || s.Rows[0].WorkspaceID != "ws" {
		t.Fatalf("rows: %+v", s.Rows)
	}
}

// What became of the branch is part of where the item stands.
func TestOutcomeNamesTheMerge(t *testing.T) {
	merged := time.Date(2026, 11, 16, 14, 53, 13, 0, time.UTC)
	r := &Report{Item: Item{Status: string(inbox.StatusDone), Merge: &inbox.BranchOutcome{State: inbox.OutcomeMerged, MergeSHA: "f2c033863b4a7c633f7c44aadfe10d53ade3a890", MergedAt: &merged}}}
	if got := strings.Join(outcome(r), " "); !strings.Contains(got, "The pull request was merged on 2026-11-16 (merge commit f2c033863b).") {
		t.Fatalf("outcome: %q", got)
	}
	r.Item.Merge = &inbox.BranchOutcome{State: inbox.OutcomeClosed}
	if got := strings.Join(outcome(r), " "); !strings.Contains(got, "closed without merging.") {
		t.Fatalf("outcome: %q", got)
	}
}

// Runs a worker finished without token counts are named beside the sum.
func TestMarkdownRunsWithoutUsage(t *testing.T) {
	r := &Report{Item: Item{ID: "i", Title: "t"}, Usage: &runs.Usage{Counts: map[string]int64{"input_tokens": 5}}, Runs: []Run{
		{ID: "a", Stage: "plan", WorkerID: "w", Status: "succeeded", Usage: &runs.Usage{Counts: map[string]int64{"input_tokens": 5}}},
		{ID: "b", Stage: "review", WorkerID: "w", Status: "failed"},
		{ID: "c", Stage: "review", WorkerID: "w", Status: "running"},
	}}
	mustHold(t, "the summary", Markdown(r, LevelSummary), "- Tokens as the runners reported them: 5 in, 0 out\n- 1 run recorded no usage: a worker older than the record, or no agent started\n")
}

// Time on workers: each run's in the table (its agent's time, marked, for
// a run from before skopos kept it), the sum below with the runs that
// recorded none counted, and the agents' own time from the usage record.
func TestMarkdownRunTime(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	r := &Report{Item: Item{ID: "i", Title: "t"}, WorkMS: ms(6_300_000),
		Usage: &runs.Usage{Counts: map[string]int64{"agent_ms": 5_400_000}}, Runs: []Run{
			{ID: "a", Stage: "plan", WorkerID: "w", Status: "succeeded", WorkMS: ms(45_000)},
			{ID: "b", Stage: "implement", WorkerID: "w", Status: "succeeded", WorkMS: ms(6_255_000)},
			{ID: "c", Stage: "review", WorkerID: "w", Status: "succeeded", Usage: &runs.Usage{Counts: map[string]int64{"agent_ms": 720_000}}},
		}}
	full := Markdown(r, LevelFull)
	mustHold(t, "the table", full, "| Status | Time | Tokens | Cost |", "| succeeded | 45s |", "| succeeded | 1h 44m |", "| succeeded | 12m agent |")
	mustHold(t, "the totals", Markdown(r, LevelSummary),
		"- Time on workers: 1h 45m\n- 1 run recorded no time on workers: from before skopos kept it, or still in progress\n- Agent time as the workers measured it: 1h 30m\n")
	for ms, want := range map[int64]string{59_400: "59s", 90_000: "2m", 3_600_000: "1h 0m", 93_600_000: "1d 2h"} {
		if got := span(ms); got != want {
			t.Errorf("span(%d) = %q, want %q", ms, got, want)
		}
	}
}
