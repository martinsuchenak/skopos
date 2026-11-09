package qa

import (
	"context"
	"fmt"
	"strings"
)

// ForRun is the item's record as a run's prompt reads it: the sets with
// answers, and the answers it carries marked as used by the run (the first
// run that used each one). A correction made after that is flagged.
func (s *Service) ForRun(ctx context.Context, itemID, runID string) (*Record, error) {
	rec, err := s.Record(ctx, itemID)
	if err != nil {
		return nil, err
	}
	sets := rec.Sets
	var ids []string
	for i := range sets {
		for j := range sets[i].Questions {
			for k := range sets[i].Questions[j].Answers {
				a := &sets[i].Questions[j].Answers[k]
				if a.Kind == KindAnswer && !a.Replaced && a.UsedByRun == "" {
					ids = append(ids, a.ID)
					a.UsedByRun = runID
				}
			}
		}
		for k := range sets[i].General {
			a := &sets[i].General[k]
			if a.UsedByRun == "" {
				ids = append(ids, a.ID)
				a.UsedByRun = runID
			}
		}
	}
	for k := range rec.Free {
		if a := &rec.Free[k]; a.UsedByRun == "" {
			ids = append(ids, a.ID)
			a.UsedByRun = runID
		}
	}
	if err := s.storage.MarkUsed(ctx, ids, runID); err != nil {
		return nil, err
	}
	return rec, nil
}

// PromptText renders the decisions for a prompt: every set a gate or an
// answer closed, each answered question with its current answer, the
// questions passed unanswered marked as such, his free text to the set,
// and his free answers given when no question was open. Open and
// superseded questions are left out: they are not decisions.
func PromptText(rec *Record) string {
	if rec == nil {
		return ""
	}
	var blocks []string
	for _, st := range rec.Sets {
		decided := len(st.General) > 0
		for _, q := range st.Questions {
			switch q.State {
			case StateAnswered, StateAnsweredFreeText, StateSkipped:
				decided = true
			}
		}
		if !decided {
			continue
		}
		who := st.Asked()
		if who == "" {
			who = "the " + st.Asker
		}
		lines := []string{fmt.Sprintf("Asked by %s:", who)}
		for _, q := range st.Questions {
			switch q.State {
			case StateAnswered:
				a := current(q)
				var parts []string
				if a.Option != "" {
					parts = append(parts, "("+a.Option+") "+optionAt(q, a.Option))
				}
				if a.Text != "" {
					parts = append(parts, a.Text)
				}
				lines = append(lines, fmt.Sprintf("%d. %s\n   Answer: %s", q.N, q.Text, strings.Join(parts, " — ")))
			case StateAnsweredFreeText:
				lines = append(lines, fmt.Sprintf("%d. %s\n   (answered in the free text below)", q.N, q.Text))
			case StateSkipped:
				lines = append(lines, fmt.Sprintf("%d. %s\n   (not answered: decide it yourself, or ask again if it blocks you)", q.N, q.Text))
			}
		}
		for _, g := range st.General {
			lines = append(lines, "Martin's answer to these questions: "+g.Text)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	for _, a := range rec.Free {
		with := ""
		if a.Action != ActionImported {
			with = " with " + strings.ReplaceAll(a.Action, "_", " ")
		}
		blocks = append(blocks, "Martin's answer"+with+" (no question open): "+a.Text)
	}
	return strings.Join(blocks, "\n\n")
}

func optionAt(q Question, letter string) string {
	if letter == "" {
		return "" // answered in words only
	}
	if i := int(letter[0] - 'a'); i >= 0 && i < len(q.Options) {
		return q.Options[i]
	}
	return ""
}
