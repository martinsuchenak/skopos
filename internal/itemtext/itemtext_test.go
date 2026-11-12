package itemtext

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const threeRounds = `The author's brief.

## Context

his own heading stays in the brief

## Enrichment — skopos worker

PLAN-1

## Questions — planner

1. Keep the old endpoint?
   - (a) yes
   - (b) no

## Review notes

why sync?

## Enrichment — skopos worker

PLAN-2

## Answers

1. Keep the old endpoint?
   Answer: (b) no

## Enrichment — skopos worker

PLAN-3

## Questions — planner

1. Which cache?
`

func TestSplitKeepsTheAuthorsHeadings(t *testing.T) {
	brief, secs := Split(threeRounds)
	if !strings.Contains(brief, "## Context") || strings.Contains(brief, "PLAN-1") {
		t.Fatalf("brief: %q", brief)
	}
	if len(secs) != 7 || secs[0].Heading != "Enrichment — skopos worker" || secs[0].Body != "PLAN-1" {
		t.Fatalf("sections: %+v", secs)
	}
}

// The approver's notes close a round; the next agent output opens the next one.
func TestRoundsCloseOnTheApproversNotes(t *testing.T) {
	_, secs := Split(threeRounds)
	rounds := Rounds(secs)
	type got struct {
		kind  string
		n     int
		heads []string
	}
	var out []got
	for _, r := range rounds {
		g := got{kind: r.Kind, n: r.N}
		for _, s := range r.Sections {
			g.heads = append(g.heads, s.Heading)
		}
		out = append(out, g)
	}
	want := []got{
		{"plan", 1, []string{"Enrichment — skopos worker", "Questions — planner", "Review notes"}},
		{"plan", 2, []string{"Enrichment — skopos worker", "Answers"}},
		{"plan", 3, []string{"Enrichment — skopos worker", "Questions — planner"}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("rounds:\n got %+v\nwant %+v", out, want)
	}
}

func TestRoundKinds(t *testing.T) {
	content := "x\n\n## Questions — planner\n\n- which branch?\n\n## Answers\n\nmain\n\n" +
		"## Candidate — a@claude\n\nA\n\n## Candidate — b@codex\n\nB\n\n## Comparison — judge\n\nC\n\n## Approval notes\n\nok\n\n" +
		"## review — claude\n\nfail\n\n## Change requests\n\nfix\n\n## Review summary — round 1\n\npass\n\n## Second opinion — codex @ abc\n\nfine\n"
	_, secs := Split(content)
	var kinds []string
	for _, r := range Rounds(secs) {
		kinds = append(kinds, r.Title)
	}
	if want := []string{"Questions", "Plan", "Review", "Review"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("round titles %v, want %v", kinds, want)
	}
}

func TestQuestionsRoundTrip(t *testing.T) {
	qs := []Question{
		{Text: "Keep the old\nendpoint?", Options: []string{"(a) yes, deprecated", "b) no", "  "}},
		{Text: "  "},
		{Text: "Which cache backend?"},
	}
	if FormatQuestions(nil) != "" || FormatQuestions([]Question{{Text: " "}}) != "" {
		t.Fatal("no questions, no text")
	}
	text := FormatQuestions(qs)
	want := "1. Keep the old endpoint?\n   - (a) yes, deprecated\n   - (b) no\n2. (no question)\n3. Which cache backend?\n"
	if text != want {
		t.Fatalf("format:\n%q\nwant\n%q", text, want)
	}
	back := ParseQuestions(text)
	if wantQ := []Question{{Text: "Keep the old endpoint?", Options: []string{"yes, deprecated", "no"}}, {Text: "(no question)"}, {Text: "Which cache backend?"}}; !reflect.DeepEqual(back, wantQ) {
		t.Fatalf("parse: %+v", back)
	}
}

// Sections written before the numbered format: one bullet per question.
func TestParseQuestionsOldBullets(t *testing.T) {
	got := ParseQuestions("Intro line.\n\n- runes or bytes? (a) runes (b) bytes\n- keep the flag?\n  it is used by the CLI\n")
	want := []Question{{Text: "runes or bytes? (a) runes (b) bytes"}, {Text: "keep the flag? it is used by the CLI"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestQuestionUnmarshal(t *testing.T) {
	var qs []Question
	if err := json.Unmarshal([]byte(`["plain?",{"question":"pick?","options":["x","y"]}]`), &qs); err != nil {
		t.Fatal(err)
	}
	want := []Question{{Text: "plain?"}, {Text: "pick?", Options: []string{"x", "y"}}}
	if !reflect.DeepEqual(qs, want) {
		t.Fatalf("got %+v", qs)
	}
}

// Only the newest round's questions wait for an answer; a plan group's
// members ask under their labels.
func TestCurrentQuestions(t *testing.T) {
	got := CurrentQuestions(threeRounds)
	if len(got) != 1 || got[0].Label != "" || len(got[0].Questions) != 1 || got[0].Questions[0].Text != "Which cache?" {
		t.Fatalf("got %+v", got)
	}
	answered := threeRounds + "\n## Review notes\n\nredo\n"
	if got := CurrentQuestions(answered); len(got) != 0 {
		t.Fatalf("a round the approver answered asks nothing: %+v", got)
	}
	group := "x\n\n## Candidate — a@claude\n\nA\n\n## Questions — planner a@claude\n\n1. one?\n\n## Candidate — b@codex\n\nB\n\n## Questions — planner b@codex\n\n1. two?\n   - (a) p\n"
	got = CurrentQuestions(group)
	if len(got) != 2 || got[0].Label != "a@claude" || got[1].Label != "b@codex" || got[1].Questions[0].Options[0] != "p" {
		t.Fatalf("group: %+v", got)
	}
}

// The implementer asks with an amendment or a block: its questions are
// questions like the planner's, labelled by who asked.
func TestImplementerQuestions(t *testing.T) {
	content := "x\n\n## Enrichment — skopos worker\n\nP\n\n## Approval notes\n\nok\n\n" +
		"## Amendment proposed — worker\n\n- the plan misses a step\n\n## Questions — implementer\n\n1. New scope?\n   - (a) sales\n   - (b) quotes\n"
	got := CurrentQuestions(content)
	if len(got) != 1 || got[0].Label != "the implementer" || got[0].Questions[0].Options[1] != "quotes" {
		t.Fatalf("got %+v", got)
	}
	_, secs := Split(content)
	if r := Rounds(secs); len(r) != 2 || r[1].Title != "Amendment" || len(r[1].Sections) != 2 {
		t.Fatalf("rounds: %+v", r)
	}
	// Answered and approved together: one round, closed.
	_, secs = Split(content + "\n## Answers\n\n1. New scope?\n   Answer: (a) sales\n\n## Approval notes\n\ngo\n")
	if r := Rounds(secs); len(r) != 2 || len(r[1].Sections) != 4 || !r[1].Closed {
		t.Fatalf("answers and approval notes stay in the round: %+v", r)
	}
}
