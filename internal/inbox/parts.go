package inbox

import "github.com/martinsuchenak/skopos/internal/itemtext"

// ContentParts is an item's content as the detail view shows it: Martin's
// text (everything before the first pipeline heading), then the sections
// the pipeline appended, grouped into rounds (itemtext.Rounds) — a plan and
// its questions, a review, each closed by Martin's notes on it.
type ContentParts struct {
	BriefHTML string         `json:"brief_html"`
	Rounds    []ContentRound `json:"rounds"`
}

// ContentRound is one round: its kind (plan, questions, amendment, review,
// qa, second_opinion, notes), title, and number among rounds of that kind.
type ContentRound struct {
	Kind     string           `json:"kind"`
	Title    string           `json:"title"`
	N        int              `json:"n"`
	Closed   bool             `json:"closed"` // Martin's notes ended it: it asks nothing open
	Sections []ContentSection `json:"sections"`
}

// ContentSection is one appended section, rendered. A planner's questions
// section also carries the questions parsed (itemtext.ParseQuestions) and,
// for a plan group member, its candidate label.
type ContentSection struct {
	Heading   string              `json:"heading"`
	HTML      string              `json:"html"`
	Questions []itemtext.Question `json:"questions,omitempty"`
	Label     string              `json:"label,omitempty"`
}

// ContentPartsOf cuts and renders the content for the detail view.
func ContentPartsOf(content string) *ContentParts {
	brief, secs := itemtext.Split(content)
	parts := &ContentParts{Rounds: []ContentRound{}}
	if brief != "" {
		parts.BriefHTML = RenderMarkdown(brief)
	}
	for _, r := range itemtext.Rounds(secs) {
		cr := ContentRound{Kind: r.Kind, Title: r.Title, N: r.N, Closed: r.Closed}
		for _, s := range r.Sections {
			cs := ContentSection{Heading: s.Heading, HTML: RenderMarkdown(s.Body)}
			if itemtext.IsQuestions(s) {
				cs.Questions = itemtext.ParseQuestions(s.Body)
				cs.Label = itemtext.QuestionsLabel(s)
			}
			cr.Sections = append(cr.Sections, cs)
		}
		parts.Rounds = append(parts.Rounds, cr)
	}
	return parts
}
