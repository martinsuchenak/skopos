package secretbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrNotSet: the secret has no stored value.
var ErrNotSet = errors.New("secret is not set")

// MaxValueBytes bounds one stored secret.
const MaxValueBytes = 16 << 10

// Store keeps named secrets, sealed, apart from the definitions that use
// them: a definition names its secrets, the values are only here. An owner
// is what the secrets belong to ("mcp_server:<name>").
type Store struct {
	db   *sql.DB
	ring *Ring
	now  func() time.Time
}

func NewStore(db *sql.DB, ring *Ring) *Store {
	return &Store{db: db, ring: ring, now: time.Now}
}

// Ring is the keys the store seals with.
func (s *Store) Ring() *Ring { return s.ring }

// Meta is what may be shown about a stored secret: never its value.
type Meta struct {
	Name      string    `json:"name"`
	KeyID     string    `json:"key_id"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Set stores a secret's value, replacing an earlier one.
func (s *Store) Set(ctx context.Context, owner, name string, value []byte, actor string) error {
	if len(value) == 0 {
		return errors.New("the secret's value is empty")
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("the secret's value is %d bytes (at most %d)", len(value), MaxValueBytes)
	}
	sealed, err := s.ring.Seal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO secrets (owner, name, sealed, key_id, updated_ms, updated_by) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, name) DO UPDATE SET sealed = excluded.sealed, key_id = excluded.key_id, updated_ms = excluded.updated_ms, updated_by = excluded.updated_by`,
		owner, name, sealed, s.ring.Current(), s.now().UnixMilli(), actor)
	return err
}

// Get opens a secret's value.
func (s *Store) Get(ctx context.Context, owner, name string) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT sealed FROM secrets WHERE owner = ? AND name = ?`, owner, name).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotSet
	}
	if err != nil {
		return nil, err
	}
	return s.ring.Open(sealed)
}

// List says which of an owner's secrets are set.
func (s *Store) List(ctx context.Context, owner string) (map[string]Meta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, key_id, updated_ms, updated_by FROM secrets WHERE owner = ?`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Meta{}
	for rows.Next() {
		var m Meta
		var ms int64
		if err := rows.Scan(&m.Name, &m.KeyID, &ms, &m.UpdatedBy); err != nil {
			return nil, err
		}
		m.UpdatedAt = time.UnixMilli(ms).UTC()
		out[m.Name] = m
	}
	return out, rows.Err()
}

// Delete removes one secret; it reports whether there was one.
func (s *Store) Delete(ctx context.Context, owner, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE owner = ? AND name = ?`, owner, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteOwner removes every secret of an owner.
func (s *Store) DeleteOwner(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE owner = ?`, owner)
	return err
}

// Keep removes an owner's secrets whose names are not in keep.
func (s *Store) Keep(ctx context.Context, owner string, keep []string) ([]string, error) {
	have, err := s.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	var gone []string
	for name := range have {
		wanted := false
		for _, k := range keep {
			wanted = wanted || k == name
		}
		if wanted {
			continue
		}
		if _, err := s.Delete(ctx, owner, name); err != nil {
			return gone, err
		}
		gone = append(gone, name)
	}
	sort.Strings(gone)
	return gone, nil
}

// ResealRows seals the given blob columns of a table again under the
// current key, row by row, inside tx. what is an SQL expression naming a
// row in an error; keyCol, when set, is the column that records the key id.
// It returns how many values were sealed again.
func ResealRows(ctx context.Context, tx *sql.Tx, ring *Ring, table, what string, cols []string, keyCol string) (int, error) {
	type change struct {
		rowid int64
		col   string
		blob  []byte
	}
	var changes []change
	for _, col := range cols {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, %s, %s FROM %s WHERE %s IS NOT NULL`, what, col, table, col))
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var rowid int64
			var name string
			var sealed []byte
			if err := rows.Scan(&rowid, &name, &sealed); err != nil {
				_ = rows.Close()
				return 0, err
			}
			out, changed, err := ring.Reseal(sealed)
			if err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("%s %s (%s): %w", table, name, col, err)
			}
			if changed {
				changes = append(changes, change{rowid, col, out})
			}
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}
	for _, c := range changes {
		q := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, c.col)
		args := []any{c.blob, c.rowid}
		if keyCol != "" {
			q = fmt.Sprintf(`UPDATE %s SET %s = ?, %s = ? WHERE rowid = ?`, table, c.col, keyCol)
			args = []any{c.blob, ring.Current(), c.rowid}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return 0, err
		}
	}
	return len(changes), nil
}
