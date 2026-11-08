package itemtext

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Question is one of the planner's questions: the decision it asks for and,
// when there is a choice, the options (answered by letter: "1) b").
type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// UnmarshalJSON reads the planner's result shape ({"question", "options"})
// and also a bare string, the shape the schema had before options.
func (q *Question) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*q = Question{Text: s}
		return nil
	}
	var raw struct {
		Question string   `json:"question"`
		Text     string   `json:"text"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	q.Text = raw.Question
	if q.Text == "" {
		q.Text = raw.Text
	}
	q.Options = raw.Options
	return nil
}

// OptionLetter is the letter the nth option (0-based) is answered by.
func OptionLetter(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return fmt.Sprint(i + 1)
}

// FormatQuestions writes questions as a numbered list ("1. Keep the old
// endpoint?"), each option an indented bullet with its letter
// ("   - (a) yes, deprecated"); ParseQuestions reads it back. A question is
// numbered by its position in qs, the number the plan's steps cite; an
// empty one keeps its number as "(no question)".
func FormatQuestions(qs []Question) string {
	var b strings.Builder
	empty := 0
	for _, q := range qs {
		if oneLine(q.Text) == "" {
			empty++
		}
	}
	if empty == len(qs) {
		return ""
	}
	for i, q := range qs {
		text := oneLine(q.Text)
		if text == "" {
			text = "(no question)"
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
		j := 0
		for _, o := range q.Options {
			o = optionLabel.ReplaceAllString(oneLine(o), "")
			if o == "" {
				continue
			}
			fmt.Fprintf(&b, "   - (%s) %s\n", OptionLetter(j), o)
			j++
		}
	}
	return b.String()
}

var (
	numbered    = regexp.MustCompile(`^\d+[.)]\s+(.+)$`)
	topBullet   = regexp.MustCompile(`^[-*]\s+(.+)$`)
	subBullet   = regexp.MustCompile(`^\s+[-*]\s+(.+)$`)
	optionLabel = regexp.MustCompile(`^(?:\([a-z]\)|[a-z][.)])\s+`)
)

// ParseQuestions reads a questions section body: FormatQuestions' numbered
// list, or the plain bullets older sections hold (one question each, any
// options inline). Lines that belong to no question are ignored.
func ParseQuestions(body string) []Question {
	var out []Question
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := numbered.FindStringSubmatch(line); m != nil {
			out = append(out, Question{Text: strings.TrimSpace(m[1])})
			continue
		}
		if m := topBullet.FindStringSubmatch(line); m != nil {
			out = append(out, Question{Text: strings.TrimSpace(m[1])})
			continue
		}
		if len(out) == 0 {
			continue
		}
		q := &out[len(out)-1]
		if m := subBullet.FindStringSubmatch(line); m != nil {
			q.Options = append(q.Options, optionLabel.ReplaceAllString(strings.TrimSpace(m[1]), ""))
			continue
		}
		// A continuation line: the option it follows, else the question.
		if n := len(q.Options); n > 0 {
			q.Options[n-1] += " " + strings.TrimSpace(line)
		} else {
			q.Text += " " + strings.TrimSpace(line)
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Asked is one questions section: the planner's questions and, for a plan
// group member, its candidate label.
type Asked struct {
	Label     string
	Questions []Question
}

// CurrentQuestions is what the newest round asked (Rounds): the questions
// waiting for Martin's answer at the gate, one entry per planner that asked.
// Questions of earlier rounds were answered or superseded by the newer plan,
// and a round Martin's notes closed asks nothing new.
func CurrentQuestions(content string) []Asked {
	_, secs := Split(content)
	rounds := Rounds(secs)
	if len(rounds) == 0 {
		return nil
	}
	last := rounds[len(rounds)-1]
	if last.Closed {
		return nil
	}
	var out []Asked
	for _, s := range last.Sections {
		if !IsQuestions(s) {
			continue
		}
		if qs := ParseQuestions(s.Body); len(qs) > 0 {
			out = append(out, Asked{Label: QuestionsLabel(s), Questions: qs})
		}
	}
	return out
}
