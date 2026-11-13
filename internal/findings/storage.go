package findings

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Storage is the findings tables. Rows are inserted and never changed.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

// querier is what both *sql.DB and *sql.Tx offer: the record is read and
// written inside the run report's transaction as well as on its own.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func insertFinding(ctx context.Context, q querier, f Finding) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO review_findings (id, workspace_id, item_id, run_id, stage, runner, focus, head_sha, n, severity, text, file, source, created_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.WorkspaceID, f.ItemID, f.RunID, f.Stage, f.Runner, f.Focus, f.HeadSHA, f.N, f.Severity, f.Text, f.File, f.Source, ms(f.CreatedAt))
	if err != nil {
		return fmt.Errorf("recording finding: %w", err)
	}
	return nil
}

func insertUpdate(ctx context.Context, q querier, u Update) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO finding_updates (id, finding_id, workspace_id, item_id, kind, note, run_id, head_sha, actor, actor_key_id, via, created_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.FindingID, u.WorkspaceID, u.ItemID, u.Kind, u.Note, u.RunID, u.HeadSHA, u.Actor, u.ActorKeyID, u.Via, ms(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("recording finding update: %w", err)
	}
	return nil
}

// itemFindings reads an item's findings, oldest first, each with its
// updates in order and its state derived.
func itemFindings(ctx context.Context, q querier, itemID string) ([]Finding, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, workspace_id, item_id, run_id, stage, runner, focus, head_sha, n, severity, text, file, source, created_ms
		FROM review_findings WHERE item_id = ? ORDER BY created_ms, run_id, n`, itemID)
	if err != nil {
		return nil, fmt.Errorf("reading findings: %w", err)
	}
	out := []Finding{}
	at := map[string]int{}
	for rows.Next() {
		var f Finding
		var created int64
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.ItemID, &f.RunID, &f.Stage, &f.Runner, &f.Focus, &f.HeadSHA, &f.N, &f.Severity, &f.Text, &f.File, &f.Source, &created); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning finding: %w", err)
		}
		f.CreatedAt = fromMS(created)
		f.Updates = []Update{}
		at[f.ID] = len(out)
		out = append(out, f)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// rowid breaks ties between updates written in the same millisecond
	// (a report's answers): insertion order is the order they apply in.
	urows, err := q.QueryContext(ctx, `
		SELECT id, finding_id, workspace_id, item_id, kind, note, run_id, head_sha, actor, actor_key_id, via, created_ms
		FROM finding_updates WHERE item_id = ? ORDER BY created_ms, rowid`, itemID)
	if err != nil {
		return nil, fmt.Errorf("reading finding updates: %w", err)
	}
	for urows.Next() {
		var u Update
		var created int64
		if err := urows.Scan(&u.ID, &u.FindingID, &u.WorkspaceID, &u.ItemID, &u.Kind, &u.Note, &u.RunID, &u.HeadSHA, &u.Actor, &u.ActorKeyID, &u.Via, &created); err != nil {
			_ = urows.Close()
			return nil, fmt.Errorf("scanning finding update: %w", err)
		}
		u.CreatedAt = fromMS(created)
		if i, ok := at[u.FindingID]; ok {
			out[i].Updates = append(out[i].Updates, u)
		}
	}
	if err := urows.Close(); err != nil {
		return nil, err
	}
	if err := urows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		derive(&out[i])
	}
	return out, nil
}

// ItemFindings is an item's record.
func (st *Storage) ItemFindings(ctx context.Context, itemID string) ([]Finding, error) {
	return itemFindings(ctx, st.db, strings.TrimSpace(itemID))
}

// InsertUpdates appends updates in one transaction.
func (st *Storage) InsertUpdates(ctx context.Context, us []Update) error {
	if len(us) == 0 {
		return nil
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, u := range us {
		if err := insertUpdate(ctx, tx, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// unrecordedRun is a run that reported findings before they were records.
type unrecordedRun struct {
	ID, WorkspaceID, ItemID, Stage, Runner, Focus, HeadSHA, Findings string
	UpdatedAt                                                        string
}

// unrecordedRuns lists the review runs whose findings (runs.findings JSON)
// have no rows yet. A run that reports now is recorded in its report's
// transaction, so these are only runs from before the record existed.
func (st *Storage) unrecordedRuns(ctx context.Context) ([]unrecordedRun, error) {
	rows, err := st.db.QueryContext(ctx, `
		SELECT r.id, r.workspace_id, r.item_id, r.stage, r.runner, COALESCE(r.focus, ''), r.head_sha, r.findings, r.updated_at
		FROM runs r
		WHERE r.stage IN ('review', 'qa', 'branch_review', 'second_opinion')
		  AND r.status IN ('succeeded', 'failed')
		  AND r.findings IS NOT NULL AND r.findings NOT IN ('', '[]', 'null')
		  AND NOT EXISTS (SELECT 1 FROM review_findings f WHERE f.run_id = r.id)
		ORDER BY r.created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing runs to import findings from: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []unrecordedRun
	for rows.Next() {
		var r unrecordedRun
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.ItemID, &r.Stage, &r.Runner, &r.Focus, &r.HeadSHA, &r.Findings, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// importRun records one earlier run's findings, all or nothing.
func (st *Storage) importRun(ctx context.Context, fs []Finding) error {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, f := range fs {
		if err := insertFinding(ctx, tx, f); err != nil {
			return err
		}
	}
	return tx.Commit()
}
