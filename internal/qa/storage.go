package qa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	return st.InsertRecord(ctx, []Set{s}, nil)
}

// InsertAnswers writes answer rows in one transaction.
func (st *Storage) InsertAnswers(ctx context.Context, as []Answer) error {
	if len(as) == 0 {
		return nil
	}
	return st.InsertRecord(ctx, nil, as)
}

// InsertRecord writes sets and answers in one transaction: all or nothing.
func (st *Storage) InsertRecord(ctx context.Context, sets []Set, as []Answer) error {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range sets {
		if err := insertSet(ctx, tx, s); err != nil {
			return err
		}
	}
	for _, a := range as {
		if err := insertAnswer(ctx, tx, a); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertSet(ctx context.Context, tx *sql.Tx, s Set) error {
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
	return nil
}

func insertAnswer(ctx context.Context, tx *sql.Tx, a Answer) error {
	setID := nullable(a.SetID)
	if a.Kind == KindGeneral {
		setID.Valid = true // '' = a free answer, to no set
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO answers (id, workspace_id, item_id, kind, question_id, set_id, option, text, replaces,
			action, approval_id, actor, actor_key_id, via, used_by_run, created_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.WorkspaceID, a.ItemID, a.Kind, nullable(a.QuestionID), setID, nullable(a.Option), a.Text,
		nullable(a.Replaces), a.Action, nullable(a.ApprovalID), a.Actor, nullable(a.ActorKeyID), a.Via,
		nullable(a.UsedByRun), ms(a.CreatedAt)); err != nil {
		return fmt.Errorf("inserting answer: %w", err)
	}
	return nil
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

// HasRecord reports whether an item has any question or answer rows (the
// import is per item and runs once).
func (st *Storage) HasRecord(ctx context.Context, itemID string) (bool, error) {
	var n int
	err := st.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM questions WHERE item_id = ?) OR EXISTS (SELECT 1 FROM answers WHERE item_id = ?)`, itemID, itemID).Scan(&n)
	return n == 1, err
}

// MarkUsed sets used_by_run on the answers that have none: the first run
// whose prompt carried them. Set once; never changed after.
func (st *Storage) MarkUsed(ctx context.Context, ids []string, runID string) error {
	if len(ids) == 0 || runID == "" {
		return nil
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE answers SET used_by_run = ? WHERE id = ? AND used_by_run IS NULL`, runID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ImportVersion is the import version an item went through (0 = none).
func (st *Storage) ImportVersion(ctx context.Context, itemID string) (int, error) {
	var v int
	err := st.db.QueryRowContext(ctx, `SELECT version FROM qa_imports WHERE item_id = ?`, itemID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// SetImportVersion records the import version an item went through.
func (st *Storage) SetImportVersion(ctx context.Context, itemID string, v int) error {
	_, err := st.db.ExecContext(ctx, `INSERT INTO qa_imports (item_id, version) VALUES (?, ?)
		ON CONFLICT (item_id) DO UPDATE SET version = excluded.version`, itemID, v)
	return err
}

// ItemsWithRecord lists the items with a question asked, or a free answer
// given, in [from, to) (zero = open-ended), optionally in one workspace or
// one item, oldest first: the items the decision log's rows can come from.
func (st *Storage) ItemsWithRecord(ctx context.Context, workspaceID, itemID string, from, to time.Time) ([]string, error) {
	lo, hi := int64(0), int64(1)<<62
	if !from.IsZero() {
		lo = ms(from)
	}
	if !to.IsZero() {
		hi = ms(to)
	}
	q := `SELECT item_id FROM (
		SELECT item_id, workspace_id, created_ms FROM questions WHERE created_ms >= ? AND created_ms < ?
		UNION ALL
		SELECT item_id, workspace_id, created_ms FROM answers
			WHERE kind = 'general' AND set_id = '' AND created_ms >= ? AND created_ms < ?)
		WHERE (? = '' OR workspace_id = ?) AND (? = '' OR item_id = ?)
		GROUP BY item_id ORDER BY MIN(created_ms)`
	rows, err := st.db.QueryContext(ctx, q, lo, hi, lo, hi, workspaceID, workspaceID, itemID, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
