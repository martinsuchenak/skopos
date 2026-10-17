package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stage groups (tmp/multi-agent-stages — design.md §1). The table is
// STRICT with CHECKs and a partial unique index ("one open group per item
// and stage"), so the database refuses what the Go code must never write.
// Timestamps are Unix milliseconds.

const groupColumns = `id, workspace_id, item_id, stage, round, size, policy, early_exit, state,
	outcome, base_sha, deadline_ms, decided_ms, created_ms`

func scanGroup(row interface{ Scan(dest ...any) error }) (*Group, error) {
	var g Group
	var earlyExit int
	var outcome, baseSHA sql.NullString
	var deadline, created int64
	var decided sql.NullInt64
	if err := row.Scan(&g.ID, &g.WorkspaceID, &g.ItemID, &g.Stage, &g.Round, &g.Size, &g.Policy, &earlyExit, &g.State,
		&outcome, &baseSHA, &deadline, &decided, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning run group: %w", err)
	}
	g.EarlyExit = earlyExit == 1
	g.Outcome, g.BaseSHA = outcome.String, baseSHA.String
	g.Deadline = time.UnixMilli(deadline).UTC()
	g.CreatedAt = time.UnixMilli(created).UTC()
	if decided.Valid {
		g.DecidedAt = time.UnixMilli(decided.Int64).UTC()
	}
	return &g, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateGroup inserts a group and all its member runs in one transaction.
// A second open group for the same item and stage violates the partial
// unique index and surfaces as ErrDuplicateActive.
func (s *Storage) CreateGroup(ctx context.Context, g Group, members []Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_groups (`+groupColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, NULL, ?)`,
		g.ID, g.WorkspaceID, g.ItemID, g.Stage, g.Round, g.Size, g.Policy, boolInt(g.EarlyExit), GroupOpen,
		nullIfEmpty(g.BaseSHA), g.Deadline.UnixMilli(), g.CreatedAt.UnixMilli()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("%w: %s group already open", ErrDuplicateActive, g.Stage)
		}
		return fmt.Errorf("inserting run group: %w", err)
	}
	for _, r := range members {
		if err := insertRun(ctx, tx, r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextRound is the round number a new group for item+stage gets.
func (s *Storage) NextRound(ctx context.Context, itemID, stage string) (int, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(round) FROM run_groups WHERE item_id = ? AND stage = ?`, itemID, stage).Scan(&n); err != nil {
		return 0, fmt.Errorf("reading group round: %w", err)
	}
	return int(n.Int64) + 1, nil
}

// GetGroup returns one group by id.
func (s *Storage) GetGroup(ctx context.Context, id string) (*Group, error) {
	return scanGroup(s.db.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM run_groups WHERE id = ?`, id))
}

// GroupMembers returns a group's runs, oldest first.
func (s *Storage) GroupMembers(ctx context.Context, groupID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE group_id = ? ORDER BY id ASC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListGroups returns an item's groups, newest first (dashboard, tests).
func (s *Storage) ListGroups(ctx context.Context, itemID string) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+groupColumns+` FROM run_groups WHERE item_id = ? ORDER BY created_ms DESC, round DESC`, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing run groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// DecideGroup closes an open group with its outcome and cancels its still
// active members, in one transaction. It reports false when the group was
// no longer open — another member's report (or the deadline sweep) decided
// it first, and the caller must not act on the item.
func (s *Storage) DecideGroup(ctx context.Context, id, outcome, cancelReason string, now time.Time) (bool, error) {
	return s.closeGroup(ctx, id, GroupDecided, outcome, cancelReason, now)
}

// CancelGroup closes an open group without an outcome (item rejected/done).
func (s *Storage) CancelGroup(ctx context.Context, id, cancelReason string, now time.Time) (bool, error) {
	return s.closeGroup(ctx, id, GroupCancelled, "", cancelReason, now)
}

func (s *Storage) closeGroup(ctx context.Context, id, state, outcome, cancelReason string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE run_groups SET state = ?, outcome = ?, decided_ms = ?
		WHERE id = ? AND state = ?`,
		state, nullIfEmpty(outcome), now.UnixMilli(), id, GroupOpen)
	if err != nil {
		return false, fmt.Errorf("closing run group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, fail_reason = CASE WHEN fail_reason = '' THEN ? ELSE fail_reason END,
			lease_expires_at = '', resume_after = '', updated_at = ?
		WHERE group_id = ? AND status IN (?, ?, ?, ?, ?)`,
		string(StatusCancelled), cancelReason, formatTime(now), id,
		string(StatusQueued), string(StatusClaimed), string(StatusRunning), string(StatusPaused), string(StatusInterrupted)); err != nil {
		return false, fmt.Errorf("cancelling group members: %w", err)
	}
	return true, tx.Commit()
}

// SetGroupBase pins a plan group's base commit once (the first member to
// start records it; later calls return the pinned value).
func (s *Storage) SetGroupBase(ctx context.Context, id, baseSHA string) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_groups SET base_sha = ? WHERE id = ? AND base_sha IS NULL`, baseSHA, id); err != nil {
		return "", fmt.Errorf("pinning group base: %w", err)
	}
	g, err := s.GetGroup(ctx, id)
	if err != nil {
		return "", err
	}
	return g.BaseSHA, nil
}

// OpenGroupsForItem returns an item's open groups.
func (s *Storage) OpenGroupsForItem(ctx context.Context, itemID string) ([]Group, error) {
	return s.groupsWhere(ctx, `item_id = ? AND state = ?`, itemID, GroupOpen)
}

// ExpiredGroups returns open groups past their deadline.
func (s *Storage) ExpiredGroups(ctx context.Context, now time.Time) ([]Group, error) {
	return s.groupsWhere(ctx, `state = ? AND deadline_ms < ?`, GroupOpen, now.UnixMilli())
}

func (s *Storage) groupsWhere(ctx context.Context, where string, args ...any) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+groupColumns+` FROM run_groups WHERE `+where+` ORDER BY created_ms ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing run groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}
