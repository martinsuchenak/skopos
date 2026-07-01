package blackboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Store interface {
	Write(ctx context.Context, entry Entry) error
	Bundle(ctx context.Context, workspaceID, branchName, sessionID string) ([]Entry, error)
	Promote(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	DeleteBySession(ctx context.Context, sessionID string) error
	Search(ctx context.Context, filters SearchFilters) ([]Entry, error)
	Get(ctx context.Context, id string) (*Entry, error)
}

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func (s *Storage) Write(ctx context.Context, entry Entry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blackboard_entries (
			id, scope, workspace_id, branch_name, session_id, entry_type, title, content, code_ref,
			author_agent_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID,
		string(entry.Scope),
		nullableString(entry.WorkspaceID),
		nullableString(entry.BranchName),
		nullableString(entry.SessionID),
		string(entry.EntryType),
		entry.Title,
		entry.Content,
		nullableString(entry.CodeRef),
		entry.AuthorAgentID,
		formatTime(entry.CreatedAt),
		formatTime(entry.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("inserting blackboard entry: %w", err)
	}
	return nil
}

func (s *Storage) Bundle(ctx context.Context, workspaceID, branchName, sessionID string) ([]Entry, error) {
	query := `
		SELECT id, scope, workspace_id, branch_name, session_id, entry_type, title, content, code_ref,
		       author_agent_id, created_at, updated_at
		FROM blackboard_entries
		WHERE (scope = 'project'
		   OR entry_type IN ('bug', 'debt')
		   OR (scope = 'branch' AND (? = '' OR branch_name = ?))
		   OR (scope = 'session' AND (? = '' OR session_id = ?)))
	`
	args := []any{branchName, branchName, sessionID, sessionID}
	if workspaceID != "" {
		query += ` AND workspace_id = ?`
		args = append(args, workspaceID)
	}
	query += ` ORDER BY entry_type, created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying bundle: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating entries: %w", err)
	}
	return entries, nil
}

func (s *Storage) Get(ctx context.Context, id string) (*Entry, error) {
	var e Entry
	var workspaceID, branchName, sessionID, codeRef sql.NullString
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, scope, workspace_id, branch_name, session_id, entry_type, title, content, code_ref,
		       author_agent_id, created_at, updated_at
		FROM blackboard_entries WHERE id = ?
	`, id).Scan(
		&e.ID, &e.Scope, &workspaceID, &branchName, &sessionID, &e.EntryType,
		&e.Title, &e.Content, &codeRef, &e.AuthorAgentID, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting entry: %w", err)
	}
	if workspaceID.Valid {
		e.WorkspaceID = workspaceID.String
	}
	if branchName.Valid {
		e.BranchName = branchName.String
	}
	if sessionID.Valid {
		e.SessionID = sessionID.String
	}
	if codeRef.Valid {
		e.CodeRef = codeRef.String
	}
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return &e, nil
}

func (s *Storage) Promote(ctx context.Context, id string) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	var newScope Scope
	var newBranch any
	switch entry.Scope {
	case ScopeSession:
		newScope = ScopeBranch
		newBranch = nullableString(entry.BranchName)
	case ScopeBranch:
		newScope = ScopeProject
		newBranch = nil
	case ScopeProject:
		return ErrAlreadyAtTopScope
	}

	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `
		UPDATE blackboard_entries
		SET scope = ?, branch_name = ?, session_id = NULL, updated_at = ?
		WHERE id = ?
	`, string(newScope), newBranch, now, id)
	if err != nil {
		return fmt.Errorf("promoting entry: %w", err)
	}
	return nil
}

func (s *Storage) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM blackboard_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking delete result: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) DeleteBySession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM blackboard_entries WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("deleting entries by session: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(row rowScanner) (Entry, error) {
	var e Entry
	var workspaceID, branchName, sessionID, codeRef sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(
		&e.ID, &e.Scope, &workspaceID, &branchName, &sessionID, &e.EntryType,
		&e.Title, &e.Content, &codeRef, &e.AuthorAgentID, &createdAt, &updatedAt,
	); err != nil {
		return e, fmt.Errorf("scanning entry: %w", err)
	}
	if workspaceID.Valid {
		e.WorkspaceID = workspaceID.String
	}
	if branchName.Valid {
		e.BranchName = branchName.String
	}
	if sessionID.Valid {
		e.SessionID = sessionID.String
	}
	if codeRef.Valid {
		e.CodeRef = codeRef.String
	}
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return e, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *Storage) Search(ctx context.Context, f SearchFilters) ([]Entry, error) {
	query := `SELECT id, scope, workspace_id, branch_name, session_id, entry_type, title, content, code_ref, author_agent_id, created_at, updated_at FROM blackboard_entries WHERE 1=1`
	var args []any
	if f.WorkspaceID != "" {
		query += " AND workspace_id = ?"
		args = append(args, f.WorkspaceID)
	}
	if f.BranchName != "" {
		query += " AND branch_name = ?"
		args = append(args, f.BranchName)
	}
	if f.EntryType != "" {
		query += " AND entry_type = ?"
		args = append(args, f.EntryType)
	}
	if f.AuthorAgentID != "" {
		query += " AND author_agent_id = ?"
		args = append(args, f.AuthorAgentID)
	}
	if f.Query != "" {
		query += " AND (title LIKE ? OR content LIKE ?)"
		args = append(args, "%"+f.Query+"%", "%"+f.Query+"%")
	}
	query += " ORDER BY created_at DESC LIMIT 100"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("searching blackboard: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
