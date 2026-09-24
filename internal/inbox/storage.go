package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Store interface {
	CreateItem(ctx context.Context, item Item) error
	GetItem(ctx context.Context, id string) (*Item, error)
	ListItems(ctx context.Context, workspaceID, status, tag, query string) ([]Item, error)
	// ItemsByPriority returns the open/in_progress items holding an exact
	// priority in one workspace — the by-number item reference for agents.
	// Normally exactly one row; more means duplicate numbers (defensive).
	ItemsByPriority(ctx context.Context, workspaceID string, priority int) ([]Item, error)
	ItemWorkspace(ctx context.Context, id string) (string, error)
	PlanWorkspace(ctx context.Context, planID string) (string, error)
	UpdateItem(ctx context.Context, id, workspace, title, content, tagsJSON string, priority *int, updatedAt time.Time) error
	// ReorderItems assigns priorities 1..N to ids, in order, atomically.
	ReorderItems(ctx context.Context, ids []string, updatedAt time.Time) error
	// RestoreItem moves a discarded item back to open (claim cleared).
	RestoreItem(ctx context.Context, id string, updatedAt time.Time) error
	// ReopenItem moves a done item back to open — fresh cycle: claim, plan
	// link, and priority are cleared.
	ReopenItem(ctx context.Context, id string, updatedAt time.Time) error
	ClaimItem(ctx context.Context, id, agentID string, updatedAt time.Time) error
	ReleaseItem(ctx context.Context, id string, updatedAt time.Time) error
	ConvertItem(ctx context.Context, id, planID string, updatedAt time.Time) error
	SetStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error
	CompleteForPlan(ctx context.Context, planID string, updatedAt time.Time) (int64, error)
	DeleteItem(ctx context.Context, id string) error
	// DeleteByFilter bulk-deletes items in one workspace — every status, or
	// one status when non-empty. Unfiled items (NULL workspace) never match.
	DeleteByFilter(ctx context.Context, workspaceID string, status Status) (int64, error)
	// RunInTx executes fn inside a single SQL transaction. The Store passed to
	// fn is bound to the transaction, so all operations are atomic. If fn is
	// called on a store already inside a transaction, fn runs inline (no nesting).
	RunInTx(ctx context.Context, fn func(Store) error) error
}

// DBTX is the minimal subset of *sql.DB / *sql.Tx used by Storage queries.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Storage struct {
	db DBTX
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

// RunInTx implements Store.RunInTx.
func (s *Storage) RunInTx(ctx context.Context, fn func(Store) error) error {
	// Already inside a transaction: run inline without nesting.
	if _, ok := s.db.(*sql.Tx); ok {
		return fn(s)
	}
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("RunInTx: cannot begin transaction from %T", s.db)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	txStore := &Storage{db: tx}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// itemColumns joins the linked plan's summary: converted items show the plan
// they became, and a deleted plan leaves the summary NULL (dangling plan_id).
const itemColumns = `
	i.id, i.workspace_id, i.title, i.content, i.tags, i.status, i.priority,
	i.claimed_by_agent_id, i.author_agent_id, i.plan_id, i.created_at, i.updated_at,
	p.name, p.status
`

// itemOrder is the partial-order contract: prioritized items first (ascending
// priority), the unprioritized tail after them (newest first) — the board and
// list share it. Recency is ordered by id, not created_at: ids are UUIDv7
// (fixed-width, time-sortable), while RFC3339Nano TEXT timestamps are not
// safely lexicographic (truncated trailing fraction zeros invert the order
// of timestamps within the same second).
const itemOrder = ` ORDER BY (i.priority IS NULL) ASC, i.priority ASC, i.id DESC`

func (s *Storage) CreateItem(ctx context.Context, item Item) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inbox_items
		    (id, workspace_id, title, content, tags, status, priority, claimed_by_agent_id, author_agent_id, plan_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, ?, ?)
	`, item.ID, item.WorkspaceID, item.Title, item.Content, tagsJSON(item.Tags), string(item.Status),
		nullableInt(item.Priority), nullableString(item.AuthorAgentID), formatTime(item.CreatedAt), formatTime(item.UpdatedAt))
	if err != nil {
		return fmt.Errorf("inserting inbox item: %w", err)
	}
	return nil
}

func (s *Storage) GetItem(ctx context.Context, id string) (*Item, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+itemColumns+`
		FROM inbox_items i LEFT JOIN plans p ON p.id = i.plan_id
		WHERE i.id = ?
	`, id)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Storage) ListItems(ctx context.Context, workspaceID, status, tag, query string) ([]Item, error) {
	// The tag filter matches the normalized JSON array with quoted delimiters
	// (`%"tag"%`): the tag charset excludes the quote, so a match can never
	// straddle two tags. LIKE wildcards inside the tag are escaped explicitly.
	var conds []string
	var args []any
	if workspaceID != "" {
		conds = append(conds, "i.workspace_id = ?")
		args = append(args, workspaceID)
	}
	if status != "" {
		conds = append(conds, "i.status = ?")
		args = append(args, status)
	}
	if tag != "" {
		conds = append(conds, `i.tags LIKE ? ESCAPE '\'`)
		args = append(args, `%"`+likeEscape(tag)+`"%`)
	}
	if query != "" {
		conds = append(conds, "(i.title LIKE ? OR i.content LIKE ?)")
		pattern := "%" + query + "%"
		args = append(args, pattern, pattern)
	}
	q := `SELECT ` + itemColumns + `
		FROM inbox_items i LEFT JOIN plans p ON p.id = i.plan_id`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += itemOrder
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing inbox items: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ItemsByPriority implements Store.ItemsByPriority. The status restriction is
// what makes the number a stable reference: reorder renumbers exactly the
// open/in_progress set, while converted/done/discarded rows keep stale
// priorities that may collide with live ones.
func (s *Storage) ItemsByPriority(ctx context.Context, workspaceID string, priority int) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+itemColumns+`
		FROM inbox_items i LEFT JOIN plans p ON p.id = i.plan_id
		WHERE i.workspace_id = ? AND i.priority = ? AND i.status IN ('open', 'in_progress')
	`, workspaceID, priority)
	if err != nil {
		return nil, fmt.Errorf("resolving inbox item by priority: %w", err)
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ItemWorkspace returns the item's workspace for authorization ("" when the
// item is unfiled).
func (s *Storage) ItemWorkspace(ctx context.Context, id string) (string, error) {
	var ws sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM inbox_items WHERE id = ?`, id).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	if err != nil {
		return "", err
	}
	if ws.Valid {
		return ws.String, nil
	}
	return "", nil
}

// PlanWorkspace returns the linked plan's workspace (convert validation and
// completion-event attribution). It reads the plans table deliberately: the
// join keeps hydration and validation in one query surface.
func (s *Storage) PlanWorkspace(ctx context.Context, planID string) (string, error) {
	var ws sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM plans WHERE id = ?`, planID).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	if err != nil {
		return "", err
	}
	if ws.Valid {
		return ws.String, nil
	}
	return "", nil
}

func (s *Storage) UpdateItem(ctx context.Context, id, workspace, title, content, tagsJSON string, priority *int, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET workspace_id = ?, title = ?, content = ?, tags = ?, priority = ?, updated_at = ? WHERE id = ?
	`, nullableString(workspace), title, content, tagsJSON, nullableInt(priority), formatTime(updatedAt), id)
	if err != nil {
		return fmt.Errorf("updating inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) ReorderItems(ctx context.Context, ids []string, updatedAt time.Time) error {
	// Sequential independent UPDATEs; the service wraps this in RunInTx so
	// the whole renumbering is atomic for callers that need it.
	for i, id := range ids {
		result, err := s.db.ExecContext(ctx, `
			UPDATE inbox_items SET priority = ?, updated_at = ? WHERE id = ?
		`, i+1, formatTime(updatedAt), id)
		if err != nil {
			return fmt.Errorf("reordering inbox items: %w", err)
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return fmt.Errorf("%w: item %s", ErrNotFound, id)
		}
	}
	return nil
}

func (s *Storage) ClaimItem(ctx context.Context, id, agentID string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, claimed_by_agent_id = ?, updated_at = ?
		WHERE id = ? AND status = ? AND (claimed_by_agent_id IS NULL OR claimed_by_agent_id = '')
	`, string(StatusInProgress), agentID, formatTime(updatedAt), id, string(StatusOpen))
	if err != nil {
		return fmt.Errorf("claiming inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrClaimConflict
	}
	return nil
}

func (s *Storage) ReleaseItem(ctx context.Context, id string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, claimed_by_agent_id = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, string(StatusOpen), formatTime(updatedAt), id, string(StatusInProgress))
	if err != nil {
		return fmt.Errorf("releasing inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) ConvertItem(ctx context.Context, id, planID string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, plan_id = ?, updated_at = ?
		WHERE id = ?
	`, string(StatusConverted), planID, formatTime(updatedAt), id)
	if err != nil {
		return fmt.Errorf("converting inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

// RestoreItem moves a discarded item back to open, clearing any stale claim.
func (s *Storage) RestoreItem(ctx context.Context, id string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, claimed_by_agent_id = NULL, updated_at = ? WHERE id = ?
	`, string(StatusOpen), formatTime(updatedAt), id)
	if err != nil {
		return fmt.Errorf("restoring inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

// ReopenItem moves a done item back to open for a fresh cycle. The claim,
// plan link, and priority are cleared: the old rank would resurface the
// item mid-board, and the completed plan link would block re-converting.
func (s *Storage) ReopenItem(ctx context.Context, id string, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, claimed_by_agent_id = NULL, plan_id = NULL, priority = NULL, updated_at = ?
		WHERE id = ? AND status = ?
	`, string(StatusOpen), formatTime(updatedAt), id, string(StatusDone))
	if err != nil {
		return fmt.Errorf("reopening inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) SetStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, updated_at = ? WHERE id = ?
	`, string(status), formatTime(updatedAt), id)
	if err != nil {
		return fmt.Errorf("setting inbox item status: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

// CompleteForPlan flips every converted item linked to planID to done. It is
// driven by the plans completion hook (background context, system principal)
// and returns how many items moved.
func (s *Storage) CompleteForPlan(ctx context.Context, planID string, updatedAt time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE inbox_items SET status = ?, updated_at = ? WHERE plan_id = ? AND status = ?
	`, string(StatusDone), formatTime(updatedAt), planID, string(StatusConverted))
	if err != nil {
		return 0, fmt.Errorf("completing inbox items for plan: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

func (s *Storage) DeleteItem(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM inbox_items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting inbox item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, id)
	}
	return nil
}

// DeleteByFilter implements Store.DeleteByFilter: one statement over the
// workspace (optionally narrowed to a status); RowsAffected is the count.
func (s *Storage) DeleteByFilter(ctx context.Context, workspaceID string, status Status) (int64, error) {
	query := `DELETE FROM inbox_items WHERE workspace_id = ?`
	args := []any{workspaceID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("purging inbox items: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("checking purge result: %w", err)
	}
	return n, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (Item, error) {
	var item Item
	var workspace sql.NullString
	var priority sql.NullInt64
	var claimedBy, planID sql.NullString
	var planName, planStatus sql.NullString
	var tags string
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &workspace, &item.Title, &item.Content, &tags, &item.Status, &priority,
		&claimedBy, &item.AuthorAgentID, &planID, &createdAt, &updatedAt, &planName, &planStatus); err != nil {
		return item, fmt.Errorf("scanning inbox item: %w", err)
	}
	if workspace.Valid {
		item.WorkspaceID = workspace.String
	}
	if priority.Valid {
		p := int(priority.Int64)
		item.Priority = &p
	}
	if claimedBy.Valid {
		item.ClaimedByAgentID = claimedBy.String
	}
	if planID.Valid {
		item.PlanID = planID.String
	}
	if planName.Valid {
		item.Plan = &PlanSummary{ID: item.PlanID, Name: planName.String}
		if planStatus.Valid {
			item.Plan.Status = planStatus.String
		}
	}
	item.Tags = parseTags(tags)
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	return item, nil
}

// parseTags decodes the normalized JSON array; malformed rows degrade to no
// tags rather than failing the read.
func parseTags(raw string) []string {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}

func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// likeEscape neutralizes LIKE wildcards in literal matching (tag filter).
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
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
