package approvals

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Storage persists approvals, append-only.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (s *Storage) Write(ctx context.Context, e Entry) error {
	var actorKeyID any
	if e.ActorKeyID != "" {
		actorKeyID = e.ActorKeyID
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (id, workspace_id, item_id, gate, subject, decision, actor, actor_key_id, via, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.WorkspaceID, e.ItemID, e.Gate, e.Subject, e.Decision, e.Actor, actorKeyID, e.Via, e.Notes, formatTime(e.CreatedAt)); err != nil {
		return fmt.Errorf("inserting approval: %w", err)
	}
	return nil
}

// List returns an item's decisions, newest-first.
func (s *Storage) List(ctx context.Context, itemID string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workspace_id, item_id, gate, subject, decision, actor, actor_key_id, via, notes, created_at
		FROM approvals WHERE item_id = ? ORDER BY id DESC`, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var actorKeyID sql.NullString
		var createdAt string
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.ItemID, &e.Gate, &e.Subject, &e.Decision, &e.Actor, &actorKeyID, &e.Via, &e.Notes, &createdAt); err != nil {
			return nil, err
		}
		e.ActorKeyID = actorKeyID.String
		e.CreatedAt = parseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestApproved returns the newest approved entry for one gate, or nil when
// none exists. The stale check (subject vs current head) is the caller's —
// only the caller knows the current head.
func (s *Storage) LatestApproved(ctx context.Context, itemID, gate string) (*Entry, error) {
	var e Entry
	var actorKeyID sql.NullString
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, workspace_id, item_id, gate, subject, decision, actor, actor_key_id, via, notes, created_at
		FROM approvals WHERE item_id = ? AND gate = ? AND decision = ?
		ORDER BY id DESC LIMIT 1`, itemID, gate, DecisionApproved,
	).Scan(&e.ID, &e.WorkspaceID, &e.ItemID, &e.Gate, &e.Subject, &e.Decision, &e.Actor, &actorKeyID, &e.Via, &e.Notes, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest approval: %w", err)
	}
	e.ActorKeyID = actorKeyID.String
	e.CreatedAt = parseTime(createdAt)
	return &e, nil
}
