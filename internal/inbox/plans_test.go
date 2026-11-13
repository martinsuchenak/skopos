package inbox

import (
	"errors"
	"strings"
	"testing"
)

const plansContent = `Count the words.

Also the letters.

## Enrichment — skopos worker

**Plan:** p1 · **Revision:** 01a1d000-0000-7000-8000-000000000001

The first plan.

### Steps

1. **One** — do it

## Questions — planner

1. Which words?
   - (a) all
   - (b) some

## Review notes

Use all of them.

## Candidate — claude

**Candidate:** claude · **Size:** m · **Base:** ` + "`b254b9c31b`" + ` · **Revision:** 01a1d342-e9d3-7036-bbef-16902557fb85

Adds Letters.

### Steps

1. **Add Letters** — count runes

## Questions — planner claude

1. Again?
   - (a) yes

## Candidate — security@codex

**Candidate:** security@codex · **Revision:** 01a1d342-e9d3-7036-bbef-16902557fb86

Adds Letters, safely.

## Questions — planner security@codex

1. Limits?

## Plan candidates — round 1

**Outcome:** candidates · policy ` + "`candidates`" + ` · 2 members

## Comparison — codex

**Recommendation:** Candidate 1 (claude).

| Candidate | Fit |
|---|---|
| 1 · claude | 5 |

## Candidate — synthesis

**Candidate:** synthesis · **Revision:** 01a1d343-ee1d-7854-87fa-a377b8747aa1

Both, merged.
`

// The plans of a round: every planner's plan with its own questions, the
// comparison, the synthesis — numbered as the gate numbers them, each with
// the revision an approval would name.
func TestPlansOf(t *testing.T) {
	v, err := PlansOf("item-1", "Count things", plansContent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Round != 2 || v.Rounds != 2 || len(v.Plans) != 3 {
		t.Fatalf("the newest round of two, three plans: %+v", v)
	}
	want := []struct{ kind, label, title, rev, text, question string }{
		{"candidate", "claude", "Candidate 1 · claude", "01a1d342-e9d3-7036-bbef-16902557fb85", "Adds Letters.", "Again?"},
		{"candidate", "security@codex", "Candidate 2 · security@codex", "01a1d342-e9d3-7036-bbef-16902557fb86", "Adds Letters, safely.", "Limits?"},
		{"synthesis", "synthesis", "Candidate 3 · synthesis", "01a1d343-ee1d-7854-87fa-a377b8747aa1", "Both, merged.", ""},
	}
	for i, w := range want {
		d := v.Plans[i]
		if d.Kind != w.kind || d.Label != w.label || d.Title != w.title || d.RevisionID != w.rev || !strings.Contains(d.Markdown, w.text) || !strings.Contains(d.HTML, w.text) {
			t.Fatalf("plan %d: %+v", i, d)
		}
		if (w.question == "") != (d.QuestionsMarkdown == "") || !strings.Contains(d.QuestionsMarkdown, w.question) || !strings.Contains(d.QuestionsHTML, w.question) {
			t.Fatalf("plan %d carries its own questions: %q", i, d.QuestionsMarkdown)
		}
	}
	if v.Comparison == nil || v.Comparison.Heading != "Comparison — codex" || !strings.Contains(v.Comparison.HTML, "<table>") || !strings.Contains(v.Comparison.Markdown, "Candidate 1 (claude)") {
		t.Fatalf("the comparison, rendered with its table: %+v", v.Comparison)
	}
	if v.Summary == nil || !strings.Contains(v.Summary.Markdown, "2 members") || len(v.Notes) != 0 {
		t.Fatalf("the round's summary, and no notes yet: %+v %+v", v.Summary, v.Notes)
	}

	// The first round: one planner's plan, its questions, and the notes
	// that closed it.
	first, err := PlansOf("item-1", "Count things", plansContent, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Plans) != 1 || first.Plans[0].Kind != "plan" || first.Plans[0].Title != "Plan" || first.Plans[0].RevisionID != "01a1d000-0000-7000-8000-000000000001" ||
		!strings.Contains(first.Plans[0].QuestionsMarkdown, "Which words?") || first.Comparison != nil {
		t.Fatalf("round one: %+v", first)
	}
	if len(first.Notes) != 1 || first.Notes[0].Heading != "Review notes" || !strings.Contains(first.Notes[0].Markdown, "Use all of them.") {
		t.Fatalf("the notes that closed round one: %+v", first.Notes)
	}

	if _, err := PlansOf("item-1", "t", plansContent, 3); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a round the item does not have: %v", err)
	}
	if _, err := PlansOf("item-1", "t", "only a brief", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an item without a plan: %v", err)
	}
}

// The Markdown export is the round as the item has it: title, brief, then
// every section verbatim, so nothing is reworded on the way out.
func TestPlansMarkdown(t *testing.T) {
	doc, err := PlansMarkdown("item-1", "Count things", plansContent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc, "# Count things\n\n_Item item-1 · planning round 2 of 2_\n\nCount the words.\n\nAlso the letters.\n\n## Candidate — claude\n") {
		t.Fatalf("the head of the document:\n%s", doc[:200])
	}
	for _, want := range []string{
		"## Questions — planner claude\n\n1. Again?\n   - (a) yes",
		"## Candidate — security@codex",
		"## Comparison — codex\n\n**Recommendation:** Candidate 1 (claude).",
		"| 1 · claude | 5 |",
		"## Candidate — synthesis",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("the export lacks %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "The first plan.") || strings.Contains(doc, "Use all of them.") {
		t.Fatalf("round one is not part of round two's export:\n%s", doc)
	}
	if !strings.HasSuffix(doc, "Both, merged.\n") {
		t.Fatalf("one trailing newline: %q", doc[len(doc)-30:])
	}
	one, _ := PlansMarkdown("item-1", "Count things", plansContent, 1)
	if !strings.Contains(one, "## Enrichment — skopos worker") || !strings.Contains(one, "## Review notes\n\nUse all of them.") || strings.Contains(one, "Candidate — claude") {
		t.Fatalf("round one's export:\n%s", one)
	}
	if got := plansFileName(&Item{ID: "item-1", Title: "Count things: words & letters!"}); got != "count-things-words-letters-plans.md" {
		t.Fatalf("file name: %q", got)
	}
	if got := plansFileName(&Item{ID: "item-1", Title: "…"}); got != "item-1-plans.md" {
		t.Fatalf("file name without words: %q", got)
	}
}
