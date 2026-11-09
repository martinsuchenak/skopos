package qa

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Storage writes and reads the questions and answers tables. Inserts only.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func ms(t time.Time) int64             { return t.UnixMilli() }
func fromMS(v int64) time.Time         { return time.UnixMilli(v).UTC() }
func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// InsertSet writes a set's questions in one transaction.
func (st *Storage) InsertSet(ctx context.Context, s Set) error {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range s.Questions {
		opts, err := json.Marshal(q.Options)
		if err != nil {
			return err
		}
		if q.Options == nil {
			opts = []byte("[]")
		}
		blocking := 0
		if s.Blocking {
			blocking = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO questions (id, workspace_id, item_id, set_id, n, asker, stage, run_id, runner, label,
				subject_kind, subject, blocking, text, options, source, created_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			q.ID, s.WorkspaceID, s.ItemID, s.ID, q.N, s.Asker, s.Stage, nullable(s.RunID), s.Runner, s.Label,
			s.SubjectKind, s.Subject, blocking, q.Text, string(opts), s.Source, ms(s.CreatedAt)); err != nil {
			return fmt.Errorf("inserting question: %w", err)
		}
	}
	return tx.Commit()
}

// InsertAnswers writes answer rows in one transaction.
func (st *Storage) InsertAnswers(ctx context.Context, as []Answer) error {
	if len(as) == 0 {
		return nil
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, a := range as {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO answers (id, workspace_id, item_id, kind, question_id, set_id, option, text, replaces,
				action, approval_id, actor, actor_key_id, via, used_by_run, created_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.WorkspaceID, a.ItemID, a.Kind, nullable(a.QuestionID), nullable(a.SetID), nullable(a.Option), a.Text,
			nullable(a.Replaces), a.Action, nullable(a.ApprovalID), a.Actor, nullable(a.ActorKeyID), a.Via,
			nullable(a.UsedByRun), ms(a.CreatedAt)); err != nil {
			return fmt.Errorf("inserting answer: %w", err)
		}
	}
	return tx.Commit()
}

// ItemSets returns an item's sets in the order they were asked, with their
// questions (no state).
func (st *Storage) ItemSets(ctx context.Context, itemID string) ([]Set, error) {
	rows, err := st.db.QueryContext(ctx, `
		SELECT id, workspace_id, item_id, set_id, n, asker, stage, COALESCE(run_id, ''), runner, label,
			subject_kind, subject, blocking, text, options, source, created_ms
		FROM questions WHERE item_id = ? ORDER BY created_ms, set_id, n`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sets []Set
	index := map[string]int{}
	for rows.Next() {
		var s Set
		var q Question
		var blocking int
		var opts string
		var created int64
		if err := rows.Scan(&q.ID, &s.WorkspaceID, &s.ItemID, &s.ID, &q.N, &s.Asker, &s.Stage, &s.RunID, &s.Runner, &s.Label,
			&s.SubjectKind, &s.Subject, &blocking, &q.Text, &opts, &s.Source, &created); err != nil {
			return nil, err
		}
		q.SetID = s.ID
		_ = json.Unmarshal([]byte(opts), &q.Options)
		if q.Options == nil {
			q.Options = []string{}
		}
		i, ok := index[s.ID]
		if !ok {
			s.Blocking = blocking == 1
			s.CreatedAt = fromMS(created)
			sets = append(sets, s)
			i = len(sets) - 1
			index[s.ID] = i
		}
		sets[i].Questions = append(sets[i].Questions, q)
	}
	return sets, rows.Err()
}

// ItemAnswers returns an item's answers, oldest first.
func (st *Storage) ItemAnswers(ctx context.Context, itemID string) ([]Answer, error) {
	rows, err := st.db.QueryContext(ctx, `
		SELECT id, workspace_id, item_id, kind, COALESCE(question_id, ''), COALESCE(set_id, ''), COALESCE(option, ''), text,
			COALESCE(replaces, ''), action, COALESCE(approval_id, ''), actor, COALESCE(actor_key_id, ''), via,
			COALESCE(used_by_run, ''), created_ms
		FROM answers WHERE item_id = ? ORDER BY created_ms, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Answer
	for rows.Next() {
		var a Answer
		var created int64
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.ItemID, &a.Kind, &a.QuestionID, &a.SetID, &a.Option, &a.Text,
			&a.Replaces, &a.Action, &a.ApprovalID, &a.Actor, &a.ActorKeyID, &a.Via, &a.UsedByRun, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromMS(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// HasRecord reports whether an item has any question rows (the import is
// per item and runs once).
func (st *Storage) HasRecord(ctx context.Context, itemID string) (bool, error) {
	var n int
	err := st.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM questions WHERE item_id = ?)`, itemID).Scan(&n)
	return n == 1, err
}
