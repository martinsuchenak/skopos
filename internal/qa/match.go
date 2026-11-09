package qa

import (
	"encoding/json"
	"regexp"
	"strings"
)

// AnswersInput is what an action carries as answers: a list of answers by
// question id, or free text the server reads against the open questions.
// JSON: an array of {question_id, option, text}, or a string.
type AnswersInput struct {
	Text  string
	Items []AnswerItem
	// Notes are the action's notes (approval notes, review notes): lines
	// in them that answer open questions are recorded as answers; the rest
	// stays notes. Set by the server, not read from JSON.
	Notes string
}

// AnswerItem answers one question by id.
type AnswerItem struct {
	QuestionID string `json:"question_id"`
	Option     string `json:"option,omitempty"`
	Text       string `json:"text,omitempty"`
	// Replaces names the current answer a correction replaces (required to
	// answer an answered question again).
	Replaces string `json:"replaces,omitempty"`
}

func (in *AnswersInput) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		in.Text = s
		return nil
	}
	return json.Unmarshal(b, &in.Items)
}

func (in AnswersInput) MarshalJSON() ([]byte, error) {
	if len(in.Items) > 0 {
		return json.Marshal(in.Items)
	}
	return json.Marshal(in.Text)
}

// Empty reports whether there is nothing to record.
func (in AnswersInput) Empty() bool { return strings.TrimSpace(in.Text) == "" && len(in.Items) == 0 }

// Match says what the server made of one line of free text (or one item):
// a linked answer (QuestionID set) or part of a general answer (General).
type Match struct {
	Line       string `json:"line,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	SetID      string `json:"set_id,omitempty"`
	N          int    `json:"n,omitempty"`
	Option     string `json:"option,omitempty"`
	Replaces   string `json:"replaces,omitempty"`
	General    bool   `json:"general,omitempty"`
}

var (
	toLabel    = regexp.MustCompile(`^\s*To\s+(.+?)\s*:\s*$`)
	numberLine = regexp.MustCompile(`^\s*(?:([^\s\d][^\s]*)\s+)?(\d+)\s*[.)]\s*(.*)$`)
	answerLine = regexp.MustCompile(`^\s+Answer:\s*(.*)$`)
	optionPick = regexp.MustCompile(`^\(?([a-z])(?:\)|\.|,|:|\s*[—–-](?:\s|$)|$)\s*(.*)$`)
	leadingSep = regexp.MustCompile(`^\s*[—–:,-]\s*`)
)

// matched is the matcher's result: linked answers, general text per set,
// what stays notes (notes mode), and the line-by-line report.
type matched struct {
	answers []Answer // Kind answer: QuestionID, SetID, Option, Text
	general map[string]string
	notes   string
	report  []Match
}

// matchText reads free text against the open sets (§3.3):
//
//   - "1) a", "1. b — words", "2) Redis": a linked answer to question n of
//     the set in context (the only open set, or the one a "To <label>:"
//     line or a "<label> 1) a" prefix names); a letter is an option when
//     that question has one, the rest is the answer's text;
//   - "1. <question text>" followed by "   Answer: …" (the dashboard's
//     rendering) reads the same;
//   - an indented line continues the answer above it;
//   - anything else is a general answer to the set in context (every open
//     set when none is), or in notes mode stays notes.
func matchText(open []Set, text string, notesMode bool) matched {
	m := matched{general: map[string]string{}}
	var ctxSet *Set
	if len(open) == 1 {
		ctxSet = &open[0]
	}
	generalLines := map[string][]string{}
	var noteLines []string
	last := -1 // index in m.answers of the answer an indented line continues
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	byLabel := func(label string) *Set {
		for i := range open {
			if strings.EqualFold(open[i].Asked(), label) || (open[i].Label != "" && strings.EqualFold(open[i].Label, label)) {
				return &open[i]
			}
		}
		return nil
	}
	rest := func(line string) {
		if strings.TrimSpace(line) == "" && len(generalLines) == 0 && len(noteLines) == 0 {
			return
		}
		if notesMode {
			noteLines = append(noteLines, line)
			return
		}
		targets := open
		if ctxSet != nil {
			targets = []Set{*ctxSet}
		}
		for _, s := range targets {
			generalLines[s.ID] = append(generalLines[s.ID], line)
		}
		if strings.TrimSpace(line) != "" {
			m.report = append(m.report, Match{Line: strings.TrimSpace(line), General: true})
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if g := toLabel.FindStringSubmatch(line); g != nil {
			if s := byLabel(g[1]); s != nil {
				ctxSet, last = s, -1
				continue
			}
		}
		if last >= 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && strings.TrimSpace(line) != "" && !answerLine.MatchString(line) {
			m.answers[last].Text = strings.TrimSpace(m.answers[last].Text + " " + strings.TrimSpace(line))
			continue
		}
		g := numberLine.FindStringSubmatch(line)
		if g == nil {
			last = -1
			rest(line)
			continue
		}
		set := ctxSet
		if g[1] != "" {
			set = byLabel(g[1])
		}
		n := atoi(g[2])
		var q *Question
		if set != nil {
			for j := range set.Questions {
				if set.Questions[j].N == n && set.Questions[j].State == StateOpen {
					q = &set.Questions[j]
				}
			}
		}
		if q == nil {
			last = -1
			rest(line)
			continue
		}
		body := strings.TrimSpace(g[3])
		// The dashboard's rendering: "1. <question>" then "   Answer: …".
		if i+1 < len(lines) && answerLine.MatchString(lines[i+1]) && (body == "" || strings.HasPrefix(norm(body), norm(q.Text)[:min(len(norm(q.Text)), 20)])) {
			body = strings.TrimSpace(answerLine.FindStringSubmatch(lines[i+1])[1])
			i++
		} else if norm(body) == norm(q.Text) {
			// The question restated, with no answer on the line.
			last = -1
			rest(line)
			continue
		}
		a := Answer{Kind: KindAnswer, QuestionID: q.ID, SetID: set.ID}
		if o := optionPick.FindStringSubmatch(body); o != nil && len(q.Options) > 0 {
			if idx := int(o[1][0] - 'a'); idx < len(q.Options) {
				a.Option = o[1]
				body = o[2]
				if opt := q.Options[idx]; strings.HasPrefix(strings.ToLower(body), strings.ToLower(opt)) {
					body = body[len(opt):]
				}
				body = leadingSep.ReplaceAllString(body, "")
			}
		}
		a.Text = strings.TrimSpace(body)
		if a.Option == "" && a.Text == "" {
			last = -1
			rest(line)
			continue
		}
		// A later line for the same question wins.
		replacedAt := -1
		for k := range m.answers {
			if m.answers[k].QuestionID == q.ID {
				replacedAt = k
			}
		}
		if replacedAt >= 0 {
			m.answers[replacedAt] = a
			last = replacedAt
		} else {
			m.answers = append(m.answers, a)
			last = len(m.answers) - 1
		}
		m.report = append(m.report, Match{Line: strings.TrimSpace(line), QuestionID: q.ID, SetID: set.ID, N: q.N, Option: a.Option})
	}
	for id, ls := range generalLines {
		if t := strings.TrimSpace(strings.Join(ls, "\n")); t != "" {
			m.general[id] = t
		}
	}
	m.notes = strings.TrimSpace(strings.Join(noteLines, "\n"))
	return m
}

func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
