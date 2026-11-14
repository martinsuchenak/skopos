// Package confrev stores policy and configuration as append-only revisions
// (tmp/central-config — design.md, phase 0). Rows that change in place
// leave nothing to show what applied last March; here every change is a new
// revision with a content hash, who made it and how, and a head per owner
// points at the current one. A revision is never updated or deleted by this
// package: it is evidence of what a run was decided with.
package confrev

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

var (
	ErrInvalid  = errors.New("invalid revision")
	ErrNotFound = errors.New("revision not found")
)

// Kinds of content a revision holds. Each later phase adds its own.
const (
	KindPolicy = "policy"
)

// Owners. A group or workspace owner is "group:<id>" / "workspace:<id>".
const (
	OwnerFleetCeiling = "fleet:ceiling"
	OwnerFleetDefault = "fleet:default"
)

// GroupOwner and WorkspaceOwner name a group's or a workspace's revisions.
func GroupOwner(id string) string     { return "group:" + id }
func WorkspaceOwner(id string) string { return "workspace:" + id }

// Revision is one stored version of an owner's content.
type Revision struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Owner      string          `json:"owner"`
	Content    json.RawMessage `json:"content"`
	Hash       string          `json:"content_hash"`
	CreatedAt  time.Time       `json:"created_at"`
	Actor      string          `json:"actor,omitempty"`
	ActorKeyID string          `json:"actor_key_id,omitempty"`
	Via        string          `json:"via,omitempty"`
	Note       string          `json:"note,omitempty"`
}

// Meta is who wrote a revision and how.
type Meta struct {
	Actor, ActorKeyID, Via, Note string
	// IfHead, when set, is the revision the writer read ("" = none): the
	// write happens only while that is still the owner's current one, and
	// answers ErrMoved otherwise. Compared inside the write's own
	// transaction, so two writers that read the same revision cannot both
	// pass.
	IfHead *string
}

// ErrMoved: the owner's current revision is not the one Meta.IfHead names.
var ErrMoved = errors.New("the current revision is another one")

// Store reads and writes revisions.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Hash is the content hash a revision records: sha256 of the bytes as
// stored. Callers store canonical JSON (json.Marshal of a normalized
// value), so equal content hashes equally.
func Hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

const revColumns = `id, kind, owner, content, content_hash, created_ms, actor, actor_key_id, via, note`

func scanRevision(row interface{ Scan(...any) error }) (*Revision, error) {
	var r Revision
	var content string
	var ms int64
	if err := row.Scan(&r.ID, &r.Kind, &r.Owner, &content, &r.Hash, &ms, &r.Actor, &r.ActorKeyID, &r.Via, &r.Note); err != nil {
		return nil, err
	}
	r.Content = json.RawMessage(content)
	r.CreatedAt = time.UnixMilli(ms).UTC()
	return &r, nil
}

// Put makes content the owner's current revision. When it equals the
// current one (same hash) nothing is written and changed is false: saving
// the same thing twice is not a change.
func (s *Store) Put(ctx context.Context, kind, owner string, content []byte, meta Meta) (rev *Revision, changed bool, err error) {
	kind, owner = strings.TrimSpace(kind), strings.TrimSpace(owner)
	if kind == "" || owner == "" {
		return nil, false, fmt.Errorf("%w: kind and owner are required", ErrInvalid)
	}
	if !json.Valid(content) {
		return nil, false, fmt.Errorf("%w: content is not JSON", ErrInvalid)
	}
	hash := Hash(content)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := headTx(ctx, tx, kind, owner)
	if err != nil {
		return nil, false, err
	}
	if meta.IfHead != nil {
		held := ""
		if cur != nil {
			held = cur.ID
		}
		if held != strings.TrimSpace(*meta.IfHead) {
			return cur, false, ErrMoved
		}
	}
	if cur != nil && cur.Hash == hash {
		return cur, false, nil
	}
	now := s.now().UTC()
	r := &Revision{ID: ids.New(), Kind: kind, Owner: owner, Content: json.RawMessage(content), Hash: hash, CreatedAt: now,
		Actor: meta.Actor, ActorKeyID: meta.ActorKeyID, Via: meta.Via, Note: meta.Note}
	if _, err := tx.ExecContext(ctx, `INSERT INTO config_revisions (`+revColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Kind, r.Owner, string(content), r.Hash, now.UnixMilli(), r.Actor, r.ActorKeyID, r.Via, r.Note); err != nil {
		return nil, false, fmt.Errorf("writing revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO config_heads (kind, owner, revision_id, updated_ms) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, owner) DO UPDATE SET revision_id = excluded.revision_id, updated_ms = excluded.updated_ms`,
		kind, owner, r.ID, now.UnixMilli()); err != nil {
		return nil, false, fmt.Errorf("moving the head: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return r, true, nil
}

func headTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, kind, owner string) (*Revision, error) {
	r, err := scanRevision(q.QueryRowContext(ctx, `
		SELECT `+prefixed("r", revColumns)+` FROM config_heads h JOIN config_revisions r ON r.id = h.revision_id
		WHERE h.kind = ? AND h.owner = ?`, kind, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the head of %s %s: %w", kind, owner, err)
	}
	return r, nil
}

func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// Head returns the owner's current revision, or nil when it has none (never
// set, or cleared).
func (s *Store) Head(ctx context.Context, kind, owner string) (*Revision, error) {
	return headTx(ctx, s.db, kind, owner)
}

// Heads returns every owner's current revision of a kind, by owner.
func (s *Store) Heads(ctx context.Context, kind string) (map[string]*Revision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed("r", revColumns)+` FROM config_heads h JOIN config_revisions r ON r.id = h.revision_id
		WHERE h.kind = ?`, kind)
	if err != nil {
		return nil, fmt.Errorf("reading %s heads: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out[r.Owner] = r
	}
	return out, rows.Err()
}

// Clear removes the owner's current revision: the head stays, pointing at
// nothing, and the revisions stay. It returns what was current (nil when
// there was nothing to clear).
func (s *Store) Clear(ctx context.Context, kind, owner string) (*Revision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := headTx(ctx, tx, kind, owner)
	if err != nil || cur == nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE config_heads SET revision_id = NULL, updated_ms = ? WHERE kind = ? AND owner = ?`,
		s.now().UTC().UnixMilli(), kind, owner); err != nil {
		return nil, fmt.Errorf("clearing the head: %w", err)
	}
	return cur, tx.Commit()
}

// Get returns one revision by id.
func (s *Store) Get(ctx context.Context, id string) (*Revision, error) {
	r, err := scanRevision(s.db.QueryRowContext(ctx, `SELECT `+revColumns+` FROM config_revisions WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("reading revision: %w", err)
	}
	return r, nil
}

// History lists an owner's revisions, newest first.
func (s *Store) History(ctx context.Context, kind, owner string, limit int) ([]Revision, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+revColumns+` FROM config_revisions WHERE kind = ? AND owner = ? ORDER BY created_ms DESC, id DESC LIMIT ?`,
		kind, owner, limit)
	if err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
