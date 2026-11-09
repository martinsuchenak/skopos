package qa

// withState attaches the answers to their questions and sets, and derives
// each question's state — the one definition the API, the reports and the
// answer matching read:
//
//   - answered: it has an answer that no later answer replaces;
//   - answered_free_text: none, but its set has a general answer;
//   - skipped: neither, and a gate passed it (a skipped row);
//   - superseded: none of the above, and a later set from the same asker
//     and label exists (a plan group's candidates ask under different
//     labels, so one candidate's questions don't supersede another's);
//   - open: otherwise.
//
// A set is Open when one of its questions is.
func withState(sets []Set, answers []Answer) []Set {
	replaced := map[string]bool{}
	usedBy := map[string]string{}
	for _, a := range answers {
		if a.Replaces != "" {
			replaced[a.Replaces] = true
		}
		usedBy[a.ID] = a.UsedByRun
	}
	for i := range answers {
		answers[i].Replaced = replaced[answers[i].ID]
		answers[i].AfterUse = answers[i].Replaces != "" && usedBy[answers[i].Replaces] != ""
	}
	byQuestion := map[string][]Answer{}
	general := map[string][]Answer{}
	for _, a := range answers {
		switch a.Kind {
		case KindGeneral:
			general[a.SetID] = append(general[a.SetID], a)
		default:
			byQuestion[a.QuestionID] = append(byQuestion[a.QuestionID], a)
		}
	}
	later := map[string]bool{} // set id → a later set of the same asker and label exists
	for i := range sets {
		for j := i + 1; j < len(sets); j++ {
			if sets[j].Asker == sets[i].Asker && sets[j].Label == sets[i].Label {
				later[sets[i].ID] = true
				break
			}
		}
	}
	for i := range sets {
		s := &sets[i]
		s.General = general[s.ID]
		s.Open = false
		for j := range s.Questions {
			q := &s.Questions[j]
			q.Answers = byQuestion[q.ID]
			answered, skipped := false, false
			for _, a := range q.Answers {
				switch {
				case a.Kind == KindAnswer && !replaced[a.ID]:
					answered = true
				case a.Kind == KindSkipped:
					skipped = true
				}
			}
			switch {
			case answered:
				q.State = StateAnswered
			case len(s.General) > 0:
				q.State = StateAnsweredFreeText
			case skipped:
				q.State = StateSkipped
			case later[s.ID]:
				q.State = StateSuperseded
			default:
				q.State = StateOpen
				s.Open = true
			}
		}
	}
	return sets
}

// current is a question's answer no correction replaces ("" when none).
func current(q Question) *Answer {
	for i := len(q.Answers) - 1; i >= 0; i-- {
		if a := &q.Answers[i]; a.Kind == KindAnswer && !a.Replaced {
			return a
		}
	}
	return nil
}

// openSets are the sets with a question still open, in order.
func openSets(sets []Set) []Set {
	var out []Set
	for _, s := range sets {
		if s.Open {
			out = append(out, s)
		}
	}
	return out
}
