package qa

import (
	"context"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/itemtext"
)

// ImportItem turns an item's markdown questions and answers into rows, once
// (an item with rows is skipped). Sections are read in order:
//
//   - a questions section becomes a set (source imported; asker from the
//     heading; no run or subject);
//   - an Answers section is read like free-text answers (matchText);
//   - approval notes, review notes and change requests: lines that match
//     open questions become answers, the rest stays notes;
//   - when a round's notes end and questions of it are still open, they
//     are recorded as skipped (a gate passed them).
//
// Rows carry the action "imported" and the actor "migration"; their times
// follow the item's creation in section order.
func (s *Service) ImportItem(ctx context.Context, itemID, workspaceID, content string, created time.Time) (bool, error) {
	if !strings.Contains(content, "## Questions — ") {
		return false, nil
	}
	if has, err := s.storage.HasRecord(ctx, itemID); err != nil || has {
		return false, err
	}
	_, secs := itemtext.Split(content)
	at := created.UTC()
	tick := func() time.Time { at = at.Add(time.Millisecond); return at }
	for i, sec := range secs {
		switch {
		case itemtext.IsQuestions(sec):
			asker, label := AskerPlanner, itemtext.QuestionsLabel(sec)
			if itemtext.HeadingIs(sec, "Questions — implementer") {
				asker, label = AskerImplementer, ""
			}
			if _, err := s.Ask(ctx, AskInput{
				WorkspaceID: workspaceID, ItemID: itemID, Asker: asker, Label: label,
				SubjectKind: SubjectNone, Source: SourceImported, Questions: itemtext.ParseQuestions(sec.Body), At: tick(),
			}); err != nil {
				return false, err
			}
		case itemtext.IsHumanNotes(sec):
			sets, err := s.Item(ctx, itemID)
			if err != nil {
				return false, err
			}
			open := openSets(sets)
			var rows []Answer
			if len(open) > 0 {
				m := matchText(open, sec.Body, !itemtext.HeadingIs(sec, "Answers"))
				rows = append(rows, m.answers...)
				for id, g := range m.general {
					rows = append(rows, Answer{Kind: KindGeneral, SetID: id, Text: g})
				}
				// The round's notes end here: what is still open was passed.
				if i+1 == len(secs) || !itemtext.IsHumanNotes(secs[i+1]) {
					answered := map[string]bool{}
					for _, a := range rows {
						answered[a.QuestionID] = true
					}
					for _, st := range open {
						if m.general[st.ID] != "" {
							continue
						}
						for _, q := range st.Questions {
							if q.State == StateOpen && !answered[q.ID] {
								rows = append(rows, Answer{Kind: KindSkipped, QuestionID: q.ID, SetID: st.ID})
							}
						}
					}
				}
			}
			for j := range rows {
				rows[j].ID = ids.New()
				rows[j].WorkspaceID, rows[j].ItemID = workspaceID, itemID
				rows[j].Action, rows[j].Actor, rows[j].Via = ActionImported, audit.ViaMigration, audit.ViaMigration
				rows[j].CreatedAt = tick()
			}
			if err := s.storage.InsertAnswers(ctx, rows); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// ImportAppended records the questions sections an older worker appended by
// editing the item (it doesn't post them to the run): appended is the text
// the edit added after the old content. Asked now, no run.
func (s *Service) ImportAppended(ctx context.Context, itemID, workspaceID, appended string) error {
	_, secs := itemtext.Split("x\n\n" + strings.TrimLeft(appended, "\n"))
	for _, sec := range secs {
		if !itemtext.IsQuestions(sec) {
			continue
		}
		asker, label := AskerPlanner, itemtext.QuestionsLabel(sec)
		if itemtext.HeadingIs(sec, "Questions — implementer") {
			asker, label = AskerImplementer, ""
		}
		if _, err := s.Ask(ctx, AskInput{
			WorkspaceID: workspaceID, ItemID: itemID, Asker: asker, Label: label, SubjectKind: SubjectNone,
			Blocking: false, Source: SourceWorker, Questions: itemtext.ParseQuestions(sec.Body),
		}); err != nil {
			return err
		}
	}
	return nil
}
