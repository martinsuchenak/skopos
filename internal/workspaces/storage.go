package workspaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Store interface {
	Create(ctx context.Context, ws Workspace) (created bool, err error)
	List(ctx context.Context) ([]Workspace, error)
	Delete(ctx context.Context, id string) error
}

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

// Create upserts the workspace display name and reports whether a new row was
// inserted (false when an existing workspace was renamed).
func (s *Storage) Create(ctx context.Context, ws Workspace) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT true FROM workspaces WHERE id = ?`, ws.ID).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("checking workspace existence: %w", err)
	}
	if exists {
		if _, err := s.db.ExecContext(ctx, `UPDATE workspaces SET name = ? WHERE id = ?`, ws.Name, ws.ID); err != nil {
			return false, fmt.Errorf("updating workspace: %w", err)
		}
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, created_at) VALUES (?, ?, ?)`,
		ws.ID, ws.Name, formatTime(ws.CreatedAt)); err != nil {
		return false, fmt.Errorf("inserting workspace: %w", err)
	}
	return true, nil
}

func (s *Storage) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM workspaces ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var ws Workspace
		var name sql.NullString
		var created string
		if err := rows.Scan(&ws.ID, &name, &created); err != nil {
			return nil, fmt.Errorf("scanning workspace: %w", err)
		}
		if name.Valid {
			ws.Name = name.String
		}
		ws.CreatedAt = parseTime(created)
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *Storage) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting workspace: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	return nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
