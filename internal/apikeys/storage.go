package apikeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// Storage persists API keys. Plaintext keys never reach this layer — only
// their SHA-256 hash (auth.HashKey) and a display prefix.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

// LookupKey implements auth.KeyLookup: it resolves a key hash to its scope.
// Revoked and unknown keys both return (nil, nil) — indistinguishable to the
// caller, which rejects with 401 either way.
func (s *Storage) LookupKey(ctx context.Context, keyHash string) (*auth.KeyInfo, error) {
	var (
		id, name string
		all      int
		revoked  sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, all_workspaces, revoked_at FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&id, &name, &all, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up api key: %w", err)
	}
	if revoked.Valid {
		return nil, nil
	}
	workspaces, err := s.listWorkspaces(ctx, id)
	if err != nil {
		return nil, err
	}
	return &auth.KeyInfo{
		ID:            id,
		Name:          name,
		AllWorkspaces: all != 0,
		Workspaces:    workspaces,
	}, nil
}

func (s *Storage) listWorkspaces(ctx context.Context, keyID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_id FROM api_key_workspaces WHERE api_key_id = ? ORDER BY workspace_id`, keyID)
	if err != nil {
		return nil, fmt.Errorf("listing api key workspaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// WorkspaceExists reports whether the workspace registry contains id.
func (s *Storage) WorkspaceExists(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking workspace: %w", err)
	}
	return true, nil
}

// Write inserts a key with its hash and explicit workspace scope.
func (s *Storage) Write(ctx context.Context, key Key, keyHash string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		key.ID, key.Name, keyHash, key.Prefix, boolToInt(key.AllWorkspaces), formatTime(key.CreatedAt)); err != nil {
		return fmt.Errorf("inserting api key: %w", err)
	}
	for _, ws := range key.Workspaces {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO api_key_workspaces (api_key_id, workspace_id) VALUES (?, ?)`, key.ID, ws); err != nil {
			return fmt.Errorf("inserting api key workspace: %w", err)
		}
	}
	return nil
}

// List returns all keys (including revoked), newest first, without hashes.
func (s *Storage) List(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, key_prefix, all_workspaces, created_at, last_used_at, revoked_at
		FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var all int
		var createdAt string
		var lastUsed, revoked sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &all, &createdAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		k.AllWorkspaces = all != 0
		k.CreatedAt = parseTime(createdAt)
		if lastUsed.Valid {
			t := parseTime(lastUsed.String)
			k.LastUsedAt = &t
		}
		if revoked.Valid {
			t := parseTime(revoked.String)
			k.RevokedAt = &t
		}
		k.Workspaces, err = s.listWorkspaces(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Revoke soft-deletes by id; unknown ids return ErrNotFound. Revoking an
// already-revoked key is a no-op (idempotent).
// Revoke soft-deletes by id and reports whether this call performed the
// transition (false = already revoked; unknown ids return ErrNotFound).
func (s *Storage) Revoke(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		formatTime(timeNowUTC()), id)
	if err != nil {
		return false, fmt.Errorf("revoking api key: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrNotFound
			}
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }

func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// TouchKey records last_used_at (best-effort, throttled by the authenticator).
func (s *Storage) TouchKey(ctx context.Context, keyID string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL`,
		formatTime(timeNowUTC()), keyID)
}
