package qa

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/ids"
	"github.com/martinsuchenak/skopos/internal/itemtext"
)

// ImportItem turns an item's markdown questions and answers into rows, once
// per item (qa_imports version 2; an item already recorded only gets the
// free answers a version 1 import left out). Sections are read in order:
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
	if !strings.Contains(content, "## Questions — ") && !strings.Contains(content, "## Answers") {
		return false, nil
	}
	if v, err := s.storage.ImportVersion(ctx, itemID); err != nil || v >= importVersion {
		return false, err
	}
	has, err := s.storage.HasRecord(ctx, itemID)
	if err != nil {
		return false, err
	}
	sets, answers, err := s.replay(ctx, itemID, workspaceID, content, created)
	if err != nil {
		return false, err
	}
	if has {
		// Imported by v0.27.0 (version 1), or recorded live under it: add
		// only the free answers version 1 left out.
		var free []Answer
		for _, a := range answers {
			if a.Kind == KindGeneral && a.SetID == "" {
				free = append(free, a)
			}
		}
		if err := s.storage.InsertAnswers(ctx, free); err != nil {
			return false, err
		}
		return len(free) > 0, s.storage.SetImportVersion(ctx, itemID, importVersion)
	}
	// A fresh import: everything, in one transaction.
	if err := s.storage.InsertRecord(ctx, sets, answers); err != nil {
		return false, err
	}
	return true, s.storage.SetImportVersion(ctx, itemID, importVersion)
}

// importVersion 2: free answers (given when no question was open) too.
const importVersion = 2

// replay runs an item's import in a scratch (in-memory) database and
// returns the rows it made: the real record is written in one step from
// them, or only the free answers a version 1 import left out are taken.
func (s *Service) replay(ctx context.Context, itemID, workspaceID, content string, created time.Time) ([]Set, []Answer, error) {
	scratch, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = scratch.Close() }()
	scratch.SetMaxOpenConns(1)
	if err := db.RunMigrations(scratch); err != nil {
		return nil, nil, err
	}
	r := NewService(NewStorage(scratch))
	if err := r.importAll(ctx, itemID, workspaceID, content, created); err != nil {
		return nil, nil, err
	}
	sets, err := r.storage.ItemSets(ctx, itemID)
	if err != nil {
		return nil, nil, err
	}
	answers, err := r.storage.ItemAnswers(ctx, itemID)
	return sets, answers, err
}

// importAll records an item's markdown questions and answers (see
// ImportItem).
func (s *Service) importAll(ctx context.Context, itemID, workspaceID, content string, created time.Time) error {
	_, secs := itemtext.Split(content)
	at := created.UTC()
	tick := func() time.Time { at = at.Add(time.Millisecond); return at }
	for i, sec := range secs {
		switch {
		case itemtext.IsQuestions(sec):
			asker, label, stage := AskerPlanner, itemtext.QuestionsLabel(sec), ""
			switch {
			case itemtext.HeadingIs(sec, "Questions — implementer"):
				asker, label = AskerImplementer, ""
			case itemtext.HeadingIs(sec, "Questions — QA"):
				asker, label, stage = AskerWorker, "", StageQA
			}
			if _, err := s.Ask(ctx, AskInput{
				WorkspaceID: workspaceID, ItemID: itemID, Asker: asker, Label: label, Stage: stage,
				SubjectKind: SubjectNone, Source: SourceImported, Questions: itemtext.ParseQuestions(sec.Body), At: tick(),
			}); err != nil {
				return err
			}
		case itemtext.IsHumanNotes(sec):
			sets, err := s.Item(ctx, itemID)
			if err != nil {
				return err
			}
			open := openSets(sets)
			var rows []Answer
			if len(open) == 0 && itemtext.HeadingIs(sec, "Answers") && strings.TrimSpace(sec.Body) != "" {
				// An answer to a blocker, with no question open.
				rows = append(rows, Answer{Kind: KindGeneral, Text: strings.TrimSpace(sec.Body)})
			}
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
				return err
			}
		}
	}
	return nil
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
		asker, label, stage := AskerPlanner, itemtext.QuestionsLabel(sec), ""
		switch {
		case itemtext.HeadingIs(sec, "Questions — implementer"):
			asker, label = AskerImplementer, ""
		case itemtext.HeadingIs(sec, "Questions — QA"):
			asker, label, stage = AskerWorker, "", StageQA
		}
		if _, err := s.Ask(ctx, AskInput{
			WorkspaceID: workspaceID, ItemID: itemID, Asker: asker, Label: label, Stage: stage, SubjectKind: SubjectNone,
			Blocking: false, Source: SourceWorker, Questions: itemtext.ParseQuestions(sec.Body),
		}); err != nil {
			return err
		}
	}
	return nil
}
