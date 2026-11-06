package worker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An item after two plan rounds, a third plan, approval and a review that
// sent it back: what the pipeline leaves in the content.
const roundsItem = `Add CSV export to the stock report.

## Context

Finance wants it monthly.

## Questions — planner

- Which columns?

## Answers

Same as the screen.

## Enrichment — skopos worker

**Plan:** p1

### Steps

1. **OLD-PLAN-ONE** — export via a cron job

## Review notes

NOTES-ONE: no cron, a button.

## Enrichment — skopos worker

**Plan:** p1

### Steps

1. **OLD-PLAN-TWO** — a button, PDF

## Review notes

NOTES-TWO: CSV, not PDF.

## Enrichment — skopos worker

**Plan:** p1

### Steps

1. **CURRENT-PLAN** — a button, CSV

## Approval notes

APPROVAL: keep the header row.

## review — claude-1

**Verdict:** fail — the header row is missing.

## Change requests

CHANGES: add the header row.
`

func TestSplitItem(t *testing.T) {
	brief, secs := splitItem(roundsItem)
	if !strings.HasPrefix(brief, "Add CSV export") || !strings.Contains(brief, "## Context") || !strings.Contains(brief, "Finance wants it monthly.") {
		t.Fatalf("Martin's text, his own headings included: %q", brief)
	}
	if strings.Contains(brief, "Enrichment") || strings.Contains(brief, "Questions") {
		t.Fatalf("the brief must stop at the first pipeline section: %q", brief)
	}
	if len(secs) != 10 {
		t.Fatalf("sections: %d", len(secs))
	}
	if got := newestBody(secs, "Change requests"); got != "CHANGES: add the header row." {
		t.Fatalf("newest change requests: %q", got)
	}
	if got := newestBody(secs, "Missing"); got != "" {
		t.Fatalf("no such section: %q", got)
	}
	qa := questionsAndAnswers(secs)
	if !strings.Contains(qa, "## Questions — planner\n\n- Which columns?") || !strings.Contains(qa, "## Answers\n\nSame as the screen.") {
		t.Fatalf("questions and answers: %q", qa)
	}
	if got := lastReview(secs); !strings.HasPrefix(got, "## review — claude-1") {
		t.Fatalf("last review: %q", got)
	}
}

// The round the newest notes are about: plan two, not plan one, and never
// the plan written after them.
func TestPreviousPlans(t *testing.T) {
	_, secs := splitItem(roundsItem)
	got := previousPlans(secs)
	if !strings.Contains(got, "OLD-PLAN-TWO") || strings.Contains(got, "OLD-PLAN-ONE") || strings.Contains(got, "CURRENT-PLAN") {
		t.Fatalf("previous plans: %q", got)
	}
	_, first := splitItem("Item text.\n\n## Enrichment — skopos worker\n\nPLAN-A\n\n## Review notes\n\nfix it")
	if got := previousPlans(first); !strings.Contains(got, "PLAN-A") {
		t.Fatalf("one round: %q", got)
	}
	_, group := splitItem("Item.\n\n## Candidate — a@claude\n\nCAND-A\n\n## Candidate — b@codex\n\nCAND-B\n\n## Comparison — judge\n\nCMP\n\n## Review notes\n\ncombine them")
	if got := previousPlans(group); !strings.Contains(got, "CAND-A") || !strings.Contains(got, "CAND-B") || !strings.Contains(got, "CMP") {
		t.Fatalf("a group round: %q", got)
	}
}

// The prompts carry Martin's text and the current sections only.
func TestPromptsGetTheBriefNotTheHistory(t *testing.T) {
	brief, secs := splitItem(roundsItem)
	plan, err := renderPrompt("plan.md", map[string]any{
		"Workspace": "ws", "Base": "main", "Title": "t", "Content": brief,
		"QA": questionsAndAnswers(secs), "Previous": previousPlans(secs), "Notes": newestBody(secs, "Review notes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Finance wants it monthly.", "Same as the screen.", "# The previous plan (Martin asked for changes", "OLD-PLAN-TWO", "NOTES-TWO"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan prompt missing %q", want)
		}
	}
	for _, gone := range []string{"OLD-PLAN-ONE", "NOTES-ONE", "CURRENT-PLAN", "CHANGES:"} {
		if strings.Contains(plan, gone) {
			t.Errorf("plan prompt must not carry %q", gone)
		}
	}

	impl, err := renderPrompt("implement.md", map[string]any{
		"Branch": "b", "Base": "main", "Title": "t", "Content": brief, "QA": questionsAndAnswers(secs),
		"Steps": []Step{{Title: "CURRENT-PLAN"}}, "Notes": newestBody(secs, "Change requests"),
		"LastReview": lastReview(secs), "ApprovalNotes": newestBody(secs, "Approval notes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Finance wants it monthly.", "Same as the screen.", "1. CURRENT-PLAN", "APPROVAL: keep the header row.", "the header row is missing", "CHANGES: add the header row."} {
		if !strings.Contains(impl, want) {
			t.Errorf("implement prompt missing %q", want)
		}
	}
	for _, gone := range []string{"OLD-PLAN-ONE", "OLD-PLAN-TWO", "NOTES-ONE", "NOTES-TWO"} {
		if strings.Contains(impl, gone) {
			t.Errorf("implement prompt must not carry %q", gone)
		}
	}

	adv := advisoryBrief(roundsItem)
	if !strings.Contains(adv, "Finance wants it monthly.") || !strings.Contains(adv, "NOTES-TWO") || strings.Contains(adv, "NOTES-ONE") || strings.Contains(adv, "OLD-PLAN") {
		t.Errorf("advisory brief: %q", adv)
	}
}

// Every heading the code appends to an item must be a pipeline heading, or
// its section would leak into Martin's text in every later prompt. Literal
// headings are found in the source; the dynamic review heading
// (run.Stage + " — " + account) is covered by "## review — " / "## qa — ".
func TestPipelineHeadingsCoverWriters(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`AppendSection\(ctx, [^,]+, "([^"]+)"`),
		regexp.MustCompile(`appendNotes\(ctx, "## ([^"]+)"`),
		regexp.MustCompile(`heading :?= "(?:## )?([^"]+)"`),
		regexp.MustCompile(`(?:heading :?=|return) fmt\.Sprintf\("([^"%]+)`),
	}
	var files []string
	for _, dir := range []string{".", "../inbox", "../runs"} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range matches {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	found := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllSubmatch(raw, -1) {
				h := string(m[1])
				if re == patterns[3] && !strings.Contains(h, " — ") {
					continue // a Sprintf that isn't a heading
				}
				found++
				if !isPipelineHeading("## " + h) {
					t.Errorf("%s appends %q, which pipelineHeadings (brief.go) doesn't know", f, h)
				}
			}
		}
	}
	if found < 10 {
		t.Fatalf("found only %d headings in the source: the patterns no longer match the code", found)
	}
}
