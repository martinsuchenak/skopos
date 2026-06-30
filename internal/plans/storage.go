package plans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Store interface {
	CreatePlan(ctx context.Context, plan Plan) error
	GetPlan(ctx context.Context, id string) (*Plan, error)
	ListPlans(ctx context.Context, workspaceID, branchName string) ([]Plan, error)
	UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error
	DeletePlan(ctx context.Context, id string) error
	AddItem(ctx context.Context, item Item) error
	UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) error
	GetItem(ctx context.Context, planID, itemID string) (*Item, error)
	DeleteItem(ctx context.Context, planID, itemID string) error
	ShiftPositions(ctx context.Context, planID string, fromPosition int) error
	AddDependency(ctx context.Context, itemID, dependsOnItemID string) error
	RemoveDependency(ctx context.Context, itemID, dependsOnItemID string) error
	ListDependencies(ctx context.Context, itemID string) ([]string, error)
	ListDependents(ctx context.Context, itemID string) ([]string, error)
	ItemExistsInPlan(ctx context.Context, planID, itemID string) (bool, error)
	ItemStatus(ctx context.Context, itemID string) (ItemStatus, error)
	SetItemStatus(ctx context.Context, itemID string, status ItemStatus) error
	AddPlanDependency(ctx context.Context, planID, dependsOnPlanID string) error
	RemovePlanDependency(ctx context.Context, planID, dependsOnPlanID string) error
	ListPlanDependencies(ctx context.Context, planID string) ([]string, error)
	ListPlanDependents(ctx context.Context, planID string) ([]string, error)
	PlanStatus(ctx context.Context, planID string) (PlanStatus, error)
	SetPlanStatus(ctx context.Context, planID string, status PlanStatus) error
	PlanExists(ctx context.Context, planID string) (bool, error)
	AllItemsDone(ctx context.Context, planID string) (bool, error)
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

func (s *Storage) CreatePlan(ctx context.Context, plan Plan) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plans (id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ID, plan.Name, nullableString(plan.BranchName), nullableString(plan.WorkspaceID), plan.Description,
		string(plan.Status), plan.AuthorAgentID,
		formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt))
	if err != nil {
		return fmt.Errorf("inserting plan: %w", err)
	}
	return nil
}

func (s *Storage) GetPlan(ctx context.Context, id string) (*Plan, error) {
	var p Plan
	var branchName, workspaceID, description sql.NullString
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at
		FROM plans WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &branchName, &workspaceID, &description, &p.Status,
		&p.AuthorAgentID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting plan: %w", err)
	}
	if branchName.Valid {
		p.BranchName = branchName.String
	}
	if workspaceID.Valid {
		p.WorkspaceID = workspaceID.String
	}
	if description.Valid {
		p.Description = description.String
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)

	items, err := s.listItems(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Items = items
	planDeps, err := s.ListPlanDependencies(ctx, id)
	if err != nil {
		return nil, err
	}
	p.DependsOn = planDeps
	return &p, nil
}

func (s *Storage) listItems(ctx context.Context, planID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_id, title, description, phase, status, position, claimed_by_agent_id, created_at, updated_at
		FROM plan_items WHERE plan_id = ? ORDER BY position ASC, created_at ASC
	`, planID)
	if err != nil {
		return nil, fmt.Errorf("listing items: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating items: %w", err)
	}
	for i := range items {
		deps, err := s.ListDependencies(ctx, items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].DependsOn = deps
	}
	return items, nil
}

func (s *Storage) ListPlans(ctx context.Context, workspaceID, branchName string) ([]Plan, error) {
	var (
		query string
		args  []any
	)
	if workspaceID != "" && branchName != "" {
		query = `SELECT id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at
		         FROM plans WHERE workspace_id = ? AND (branch_name = ? OR branch_name IS NULL) ORDER BY created_at DESC`
		args = []any{workspaceID, branchName}
	} else if workspaceID != "" {
		query = `SELECT id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at
		         FROM plans WHERE workspace_id = ? ORDER BY created_at DESC`
		args = []any{workspaceID}
	} else if branchName != "" {
		query = `SELECT id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at
		         FROM plans WHERE branch_name = ? OR branch_name IS NULL ORDER BY created_at DESC`
		args = []any{branchName}
	} else {
		query = `SELECT id, name, branch_name, workspace_id, description, status, author_agent_id, created_at, updated_at
		         FROM plans ORDER BY created_at DESC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	defer rows.Close()

	var result []Plan
	for rows.Next() {
		var p Plan
		var bn, ws, desc sql.NullString
		var ca, ua string
		if err := rows.Scan(&p.ID, &p.Name, &bn, &ws, &desc, &p.Status,
			&p.AuthorAgentID, &ca, &ua); err != nil {
			return nil, fmt.Errorf("scanning plan: %w", err)
		}
		if bn.Valid {
			p.BranchName = bn.String
		}
		if ws.Valid {
			p.WorkspaceID = ws.String
		}
		if desc.Valid {
			p.Description = desc.String
		}
		p.CreatedAt = parseTime(ca)
		p.UpdatedAt = parseTime(ua)
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Storage) UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error {
	var sets []string
	var args []any
	if input.Name != "" {
		sets = append(sets, "name = ?")
		args = append(args, input.Name)
	}
	if input.Description != "" {
		sets = append(sets, "description = ?")
		args = append(args, input.Description)
	}
	if input.Status != "" {
		sets = append(sets, "status = ?")
		args = append(args, string(input.Status))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(time.Now().UTC()))
	args = append(args, id)
	result, err := s.db.ExecContext(ctx,
		"UPDATE plans SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("updating plan: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) DeletePlan(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting plan: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	return nil
}

func (s *Storage) AddItem(ctx context.Context, item Item) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plan_items
		    (id, plan_id, title, description, phase, status, position, claimed_by_agent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, item.ID, item.PlanID, item.Title, item.Description, nullableString(item.Phase), string(item.Status),
		item.Position, nullableString(item.ClaimedByAgentID),
		formatTime(item.CreatedAt), formatTime(item.UpdatedAt))
	if err != nil {
		return fmt.Errorf("inserting item: %w", err)
	}
	return nil
}

func (s *Storage) UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) error {
	var sets []string
	var args []any
	if input.Status != "" {
		sets = append(sets, "status = ?")
		args = append(args, string(input.Status))
	}
	if input.ClaimedByAgentID != nil {
		sets = append(sets, "claimed_by_agent_id = ?")
		if *input.ClaimedByAgentID == "" {
			args = append(args, nil)
		} else {
			args = append(args, *input.ClaimedByAgentID)
		}
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(time.Now().UTC()))
	args = append(args, itemID, planID)
	result, err := s.db.ExecContext(ctx,
		"UPDATE plan_items SET "+strings.Join(sets, ", ")+" WHERE id = ? AND plan_id = ?", args...)
	if err != nil {
		return fmt.Errorf("updating item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s in plan %s", ErrNotFound, itemID, planID)
	}
	return nil
}

func (s *Storage) GetItem(ctx context.Context, planID, itemID string) (*Item, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, plan_id, title, description, phase, status, position, claimed_by_agent_id, created_at, updated_at
		FROM plan_items WHERE id = ? AND plan_id = ?
	`, itemID, planID)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	if err != nil {
		return nil, err
	}
	deps, err := s.ListDependencies(ctx, itemID)
	if err != nil {
		return nil, err
	}
	item.DependsOn = deps
	return &item, nil
}

func (s *Storage) DeleteItem(ctx context.Context, planID, itemID string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM plan_items WHERE id = ? AND plan_id = ?`, itemID, planID)
	if err != nil {
		return fmt.Errorf("deleting item: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	return nil
}

func (s *Storage) ShiftPositions(ctx context.Context, planID string, fromPosition int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE plan_items SET position = position + 1 WHERE plan_id = ? AND position >= ?`,
		planID, fromPosition)
	if err != nil {
		return fmt.Errorf("shifting positions: %w", err)
	}
	return nil
}

func (s *Storage) AddDependency(ctx context.Context, itemID, dependsOnItemID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO plan_item_dependencies (item_id, depends_on_item_id) VALUES (?, ?)`,
		itemID, dependsOnItemID)
	if err != nil {
		return fmt.Errorf("adding dependency: %w", err)
	}
	return nil
}

func (s *Storage) RemoveDependency(ctx context.Context, itemID, dependsOnItemID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plan_item_dependencies WHERE item_id = ? AND depends_on_item_id = ?`,
		itemID, dependsOnItemID)
	if err != nil {
		return fmt.Errorf("removing dependency: %w", err)
	}
	return nil
}

func (s *Storage) ListDependencies(ctx context.Context, itemID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT depends_on_item_id FROM plan_item_dependencies WHERE item_id = ?`, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing dependencies: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Storage) ListDependents(ctx context.Context, itemID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT item_id FROM plan_item_dependencies WHERE depends_on_item_id = ?`, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing dependents: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Storage) ItemExistsInPlan(ctx context.Context, planID, itemID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT true FROM plan_items WHERE id = ? AND plan_id = ?`, itemID, planID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return exists, err
}

func (s *Storage) ItemStatus(ctx context.Context, itemID string) (ItemStatus, error) {
	var status ItemStatus
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM plan_items WHERE id = ?`, itemID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	return status, err
}

func (s *Storage) SetItemStatus(ctx context.Context, itemID string, status ItemStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE plan_items SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), formatTime(time.Now().UTC()), itemID)
	return err
}

func (s *Storage) AddPlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO plan_dependencies (plan_id, depends_on_plan_id) VALUES (?, ?)`,
		planID, dependsOnPlanID)
	if err != nil {
		return fmt.Errorf("adding plan dependency: %w", err)
	}
	return nil
}

func (s *Storage) RemovePlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plan_dependencies WHERE plan_id = ? AND depends_on_plan_id = ?`,
		planID, dependsOnPlanID)
	if err != nil {
		return fmt.Errorf("removing plan dependency: %w", err)
	}
	return nil
}

func (s *Storage) ListPlanDependencies(ctx context.Context, planID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT depends_on_plan_id FROM plan_dependencies WHERE plan_id = ?`, planID)
	if err != nil {
		return nil, fmt.Errorf("listing plan dependencies: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Storage) ListPlanDependents(ctx context.Context, planID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT plan_id FROM plan_dependencies WHERE depends_on_plan_id = ?`, planID)
	if err != nil {
		return nil, fmt.Errorf("listing plan dependents: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Storage) PlanStatus(ctx context.Context, planID string) (PlanStatus, error) {
	var status PlanStatus
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM plans WHERE id = ?`, planID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	return status, err
}

func (s *Storage) SetPlanStatus(ctx context.Context, planID string, status PlanStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE plans SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), formatTime(time.Now().UTC()), planID)
	return err
}

func (s *Storage) PlanExists(ctx context.Context, planID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT true FROM plans WHERE id = ?`, planID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return exists, err
}

func (s *Storage) AllItemsDone(ctx context.Context, planID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plan_items WHERE plan_id = ? AND status != 'done'`, planID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking all items done: %w", err)
	}
	var hasItems int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plan_items WHERE plan_id = ?`, planID).Scan(&hasItems)
	if err != nil {
		return false, fmt.Errorf("checking plan has items: %w", err)
	}
	return hasItems > 0 && count == 0, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (Item, error) {
	var item Item
	var description, phase, claimedBy sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &item.PlanID, &item.Title, &description,
		&phase, &item.Status, &item.Position, &claimedBy, &createdAt, &updatedAt); err != nil {
		return item, fmt.Errorf("scanning item: %w", err)
	}
	if description.Valid {
		item.Description = description.String
	}
	if phase.Valid {
		item.Phase = phase.String
	}
	if claimedBy.Valid {
		item.ClaimedByAgentID = claimedBy.String
	}
	item.CreatedAt = parseTime(createdAt)
	item.UpdatedAt = parseTime(updatedAt)
	return item, nil
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
