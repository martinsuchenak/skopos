package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Storage persists audit entries. The table is append-only: this type has
// Write and List, never update or delete (retention, when configured, is a
// dedicated pruning path — see the cleanup integration in plan 01a0bf7c).
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Write appends one entry. IDs are UUIDv7, so id order is time order and the
// List cursor paginates on the primary key.
func (s *Storage) Write(ctx context.Context, e Entry) error {
	if e.ID == "" {
		e.ID = ids.New()
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, workspace_id, entity_type, entity_id, action, actor, actor_key_id, via, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.WorkspaceID, e.EntityType, e.EntityID, e.Action, e.Actor, nullable(e.ActorKeyID), e.Via, e.Notes, formatTime(e.CreatedAt)); err != nil {
		return fmt.Errorf("inserting audit entry: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListFilter selects entries. Empty fields match anything; Before paginates
// by id (UUIDv7 sorts by time); Limit caps the page (default and hard cap 200).
type ListFilter struct {
	WorkspaceID  string
	EntityType   string
	EntityID     string
	ActionPrefix string // e.g. "inbox." matches inbox.queue, inbox.approve, …
	Before       string // return entries with id strictly before this one
	Limit        int
}

// List returns entries newest-first.
func (s *Storage) List(ctx context.Context, f ListFilter) ([]Entry, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.WorkspaceID != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, f.WorkspaceID)
	}
	if f.EntityType != "" {
		where = append(where, "entity_type = ?")
		args = append(args, f.EntityType)
	}
	if f.EntityID != "" {
		where = append(where, "entity_id = ?")
		args = append(args, f.EntityID)
	}
	if f.ActionPrefix != "" {
		where = append(where, "action LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(f.ActionPrefix)+"%")
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	query := fmt.Sprintf(`
		SELECT id, workspace_id, entity_type, entity_id, action, actor, actor_key_id, via, notes, created_at
		FROM audit_log WHERE %s ORDER BY id DESC LIMIT %d`,
		strings.Join(where, " AND "), limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing audit entries: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var workspaceID, actorKeyID sql.NullString
		var createdAt string
		if err := rows.Scan(&e.ID, &workspaceID, &e.EntityType, &e.EntityID, &e.Action, &e.Actor, &actorKeyID, &e.Via, &e.Notes, &createdAt); err != nil {
			return nil, err
		}
		e.WorkspaceID = workspaceID.String
		e.ActorKeyID = actorKeyID.String
		e.CreatedAt = parseTime(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
