package worker

import "strings"

// An item's content is Martin's own text followed by the sections the
// pipeline appends: plans, questions and answers, review notes, approval
// notes, change requests, review verdicts. The dashboard shows the whole
// history; an agent gets his text plus only the sections that are current
// for its stage. Fed everything, a planner on its third round saw two
// earlier plans and every round of notes, with nothing saying which plan
// was current, and an implementer read rejected plans ahead of the
// approved one.

// pipelineHeadings are the headings the pipeline writes, as line prefixes:
// worker sections (AppendSection), the server's notes (appendNotes) and
// run-group and second-opinion summaries. The text before the first of
// them is Martin's. TestPipelineHeadingsCoverWriters keeps the list in step
// with the code that appends sections.
var pipelineHeadings = []string{
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

type itemSection struct {
	Heading string // without the leading "## "
	Body    string
}

// Text is the section as it appears in the item.
func (s itemSection) Text() string { return "## " + s.Heading + "\n\n" + s.Body }

// splitItem returns Martin's text and the pipeline's sections, in order.
// Headings of his own ("## Context") stay in his text.
func splitItem(content string) (string, []itemSection) {
	var brief []string
	var secs []itemSection
	var body []string
	flush := func() {
		if len(secs) > 0 {
			secs[len(secs)-1].Body = strings.TrimSpace(strings.Join(body, "\n"))
		}
		body = nil
	}
	for _, line := range strings.Split(content, "\n") {
		if isPipelineHeading(line) {
			flush()
			secs = append(secs, itemSection{Heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))})
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

func isPipelineHeading(line string) bool {
	for _, h := range pipelineHeadings {
		if strings.HasPrefix(line, h) {
			return true
		}
	}
	return false
}

func headingIs(s itemSection, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s.Heading, p) {
			return true
		}
	}
	return false
}

// newestBody is the body of the newest section whose heading starts with
// prefix ("" when there is none).
func newestBody(secs []itemSection, prefix string) string {
	for i := len(secs) - 1; i >= 0; i-- {
		if headingIs(secs[i], prefix) {
			return secs[i].Body
		}
	}
	return ""
}

// questionsAndAnswers is the planner's questions and Martin's answers, in
// order: decisions that hold for every later round.
func questionsAndAnswers(secs []itemSection) string {
	var out []string
	for _, s := range secs {
		if headingIs(s, "Questions — planner", "Answers") {
			out = append(out, s.Text())
		}
	}
	return strings.Join(out, "\n\n")
}

// previousPlans is the round of plans the newest review notes are about:
// the plan sections (a plan, a group's candidates, their comparison)
// between the second-newest and the newest "Review notes". Earlier rounds
// were superseded by the plan these notes reviewed.
func previousPlans(secs []itemSection) string {
	lo, hi := -1, len(secs)
	for i := len(secs) - 1; i >= 0; i-- {
		if headingIs(secs[i], "Review notes") {
			if hi == len(secs) {
				hi = i
			} else {
				lo = i
				break
			}
		}
	}
	var out []string
	for i := lo + 1; i < hi; i++ {
		if headingIs(secs[i], "Enrichment — skopos worker", "Candidate — ", "Comparison — ") {
			out = append(out, secs[i].Text())
		}
	}
	return strings.Join(out, "\n\n")
}

// lastReview is the newest agent review of the branch (a single reviewer,
// a group's summary, or a second opinion): what change requests after a
// review usually answer.
func lastReview(secs []itemSection) string {
	for i := len(secs) - 1; i >= 0; i-- {
		if headingIs(secs[i], "review — ", "qa — ", "Review summary — round", "QA summary — round", "Second opinion — ") {
			return secs[i].Text()
		}
	}
	return ""
}

// advisoryBrief is what the compare and synthesize stages read about the
// item: Martin's text, the questions and answers, and his newest review
// notes. The candidates themselves come from the plan revisions.
func advisoryBrief(content string) string {
	brief, secs := splitItem(content)
	parts := []string{brief}
	if qa := questionsAndAnswers(secs); qa != "" {
		parts = append(parts, qa)
	}
	for i := len(secs) - 1; i >= 0; i-- {
		if headingIs(secs[i], "Review notes") {
			parts = append(parts, secs[i].Text())
			break
		}
	}
	return strings.Join(parts, "\n\n")
}

// newestComparison is the newest judge's comparison section.
func newestComparison(content string) string {
	_, secs := splitItem(content)
	for i := len(secs) - 1; i >= 0; i-- {
		if headingIs(secs[i], "Comparison — ") {
			return secs[i].Text()
		}
	}
	return ""
}
