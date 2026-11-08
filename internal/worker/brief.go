package worker

import (
	"strings"

	"github.com/martinsuchenak/skopos/internal/itemtext"
)

// An item's content is Martin's own text followed by the sections the
// pipeline appends: plans, questions and answers, review notes, approval
// notes, change requests, review verdicts. The dashboard shows the whole
// history; an agent gets his text plus only the sections that are current
// for its stage. Fed everything, a planner on its third round saw two
// earlier plans and every round of notes, with nothing saying which plan
// was current, and an implementer read rejected plans ahead of the
// approved one.

type itemSection = itemtext.Section

// splitItem returns Martin's text and the pipeline's sections, in order.
func splitItem(content string) (string, []itemSection) { return itemtext.Split(content) }

// isPipelineHeading reports whether line starts a section the pipeline
// writes (itemtext.Headings; TestPipelineHeadingsCoverWriters keeps that
// list in step with the code that appends sections).
func isPipelineHeading(line string) bool { return itemtext.IsHeading(line) }

func headingIs(s itemSection, prefixes ...string) bool { return itemtext.HeadingIs(s, prefixes...) }

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
// order: decisions that hold for every later round. A round's questions
// count when he answered them: the round has an Answers section, his
// approval closed it (approval notes answer at the gate), or it is the
// round his newest notes closed (a re-plan reads those notes). Questions
// of rounds he moved past without answering were superseded.
func questionsAndAnswers(secs []itemSection) string {
	rounds := itemtext.Rounds(secs)
	newestClosed := -1
	for i, r := range rounds {
		if r.Closed {
			newestClosed = i
		}
	}
	var out []string
	for i, r := range rounds {
		answered := i == newestClosed
		for _, s := range r.Sections {
			if headingIs(s, "Answers", "Approval notes") {
				answered = true
			}
		}
		for _, s := range r.Sections {
			if headingIs(s, "Answers") || (answered && itemtext.IsQuestions(s)) {
				out = append(out, s.Text())
			}
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
