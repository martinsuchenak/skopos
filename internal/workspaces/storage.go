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
	Get(ctx context.Context, id string) (*Workspace, error)
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
		if _, err := s.db.ExecContext(ctx, `UPDATE workspaces SET name = ?, git_url = COALESCE(NULLIF(?, ''), git_url) WHERE id = ?`, ws.Name, ws.GitURL, ws.ID); err != nil {
			return false, fmt.Errorf("updating workspace: %w", err)
		}
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, git_url, created_at) VALUES (?, ?, ?, ?)`,
		ws.ID, ws.Name, ws.GitURL, formatTime(ws.CreatedAt)); err != nil {
		return false, fmt.Errorf("inserting workspace: %w", err)
	}
	return true, nil
}

func (s *Storage) List(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, git_url, created_at FROM workspaces ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var ws Workspace
		var name sql.NullString
		var created string
		if err := rows.Scan(&ws.ID, &name, &ws.GitURL, &created); err != nil {
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

func (s *Storage) Get(ctx context.Context, id string) (*Workspace, error) {
	var ws Workspace
	var name, gitURL sql.NullString
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, git_url, created_at FROM workspaces WHERE id = ?`, id).
		Scan(&ws.ID, &name, &gitURL, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting workspace: %w", err)
	}
	if name.Valid {
		ws.Name = name.String
	}
	if gitURL.Valid {
		ws.GitURL = gitURL.String
	}
	ws.CreatedAt = parseTime(created)
	return &ws, nil
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
