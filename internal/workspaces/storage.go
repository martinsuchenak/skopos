package workspaces

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Store interface {
	Create(ctx context.Context, ws Workspace) error
	List(ctx context.Context) ([]Workspace, error)
	Delete(ctx context.Context, id string) error
}

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func (s *Storage) Create(ctx context.Context, ws Workspace) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, created_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name
	`, ws.ID, ws.Name, formatTime(ws.CreatedAt))
	if err != nil {
		return fmt.Errorf("upserting workspace: %w", err)
	}
	return nil
}

func (s *Storage) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM workspaces ORDER BY created_at ASC`)
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

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
