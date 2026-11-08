// Package itemtext reads an inbox item's content: Martin's own text followed
// by the sections the agent pipeline appends (plans, the planner's
// questions, review notes, answers, reviews). The worker cuts it into what a
// stage's prompt needs; the dashboard groups it into rounds.
package itemtext

import "strings"

// Headings are the headings the pipeline writes, as line prefixes: worker
// sections (AppendSection), the server's notes (appendNotes) and run-group
// and second-opinion summaries. The text before the first of them is
// Martin's. The worker's TestPipelineHeadingsCoverWriters keeps the list in
// step with the code that appends sections.
var Headings = []string{
	"## Enrichment — skopos worker",
	"## Candidate — ",
	"## Comparison — ",
	"## Questions — planner",
	"## Amendment proposed — worker",
	"## Review notes",
	"## Change requests",
	"## Approval notes",
	"## Answers",
	"## review — ",
	"## qa — ",
	"## Plan candidates — round",
	"## Review summary — round",
	"## QA summary — round",
	"## Second opinion — ",
}

// Section is one appended section.
type Section struct {
	Heading string // without the leading "## "
	Body    string
}

// Text is the section as it appears in the item.
func (s Section) Text() string { return "## " + s.Heading + "\n\n" + s.Body }

// Split returns Martin's text and the pipeline's sections, in order.
// Headings of his own ("## Context") stay in his text.
func Split(content string) (string, []Section) {
	var brief []string
	var secs []Section
	var body []string
	flush := func() {
		if len(secs) > 0 {
			secs[len(secs)-1].Body = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = nil
	}
	for line := range strings.SplitSeq(content, "\n") {
		if IsHeading(line) {
			flush()
			secs = append(secs, Section{Heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))})
			continue
		}
		if len(secs) == 0 {
			brief = append(brief, line)
		} else {
			body = append(body, line)
		}
	}
	flush()
	return strings.TrimSpace(strings.Join(brief, "\n")), secs
}

// IsHeading reports whether line starts a pipeline section.
func IsHeading(line string) bool {
	for _, h := range Headings {
		if strings.HasPrefix(line, h) {
			return true
		}
	}
	return false
}

// HeadingIs reports whether the section's heading starts with one of the
// prefixes (headings without the leading "## ").
func HeadingIs(s Section, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s.Heading, p) {
			return true
		}
	}
	return false
}

// IsQuestions reports whether the section holds the planner's questions.
func IsQuestions(s Section) bool { return HeadingIs(s, "Questions — planner") }

// QuestionsLabel is the candidate label of a plan group member's questions
// ("Questions — planner focus@runner" → "focus@runner"), "" for a single
// planner.
func QuestionsLabel(s Section) string {
	return strings.TrimSpace(strings.TrimPrefix(s.Heading, "Questions — planner"))
}

// humanNotes are the sections Martin writes: each one answers the agent
// output before it, closing a round.
var humanNotes = []string{"Review notes", "Change requests", "Approval notes", "Answers"}

// IsHumanNotes reports whether the section is one of Martin's notes.
func IsHumanNotes(s Section) bool { return HeadingIs(s, humanNotes...) }

// Round is the agent output of one pass (a plan and its questions, a
// group's candidates, a review) and Martin's notes on it.
type Round struct {
	Kind     string // plan, questions, amendment, review, qa, second_opinion, notes
	Title    string // "Plan", "Review", …
	N        int    // 1-based count among rounds of the same kind
	Sections []Section
	Closed   bool // Martin's notes ended it
}

// Rounds groups the sections into rounds: Martin's notes close the round
// they answer, so the next agent output starts a new one. A round is named
// after its first section.
func Rounds(secs []Section) []Round {
	var out []Round
	open := false
	count := map[string]int{}
	for _, s := range secs {
		if !open {
			kind, title := roundKind(s)
			count[kind]++
			out = append(out, Round{Kind: kind, Title: title, N: count[kind]})
			open = true
		}
		r := &out[len(out)-1]
		r.Sections = append(r.Sections, s)
		if IsHumanNotes(s) {
			open = false
			r.Closed = true
		}
	}
	return out
}

func roundKind(s Section) (string, string) {
	switch {
	case HeadingIs(s, "Enrichment — skopos worker", "Candidate — ", "Comparison — ", "Plan candidates — round"):
		return "plan", "Plan"
	case IsQuestions(s):
		return "questions", "Questions"
	case HeadingIs(s, "Amendment proposed — worker"):
		return "amendment", "Amendment"
	case HeadingIs(s, "review — ", "Review summary — round"):
		return "review", "Review"
	case HeadingIs(s, "qa — ", "QA summary — round"):
		return "qa", "QA"
	case HeadingIs(s, "Second opinion — "):
		return "second_opinion", "Second opinion"
	}
	return "notes", "Notes"
}
