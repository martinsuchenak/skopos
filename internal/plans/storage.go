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
	ListPlans(ctx context.Context, branchName string) ([]Plan, error)
	UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error
	DeletePlan(ctx context.Context, id string) error
	AddItem(ctx context.Context, item Item) error
	UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) error
	GetItem(ctx context.Context, planID, itemID string) (*Item, error)
	DeleteItem(ctx context.Context, planID, itemID string) error
}

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func (s *Storage) CreatePlan(ctx context.Context, plan Plan) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO plans (id, name, branch_name, description, status, author_agent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, plan.ID, plan.Name, nullableString(plan.BranchName), plan.Description,
		string(plan.Status), plan.AuthorAgentID,
		formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt))
	if err != nil {
		return fmt.Errorf("inserting plan: %w", err)
	}
	return nil
}

func (s *Storage) GetPlan(ctx context.Context, id string) (*Plan, error) {
	var p Plan
	var branchName, description sql.NullString
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, branch_name, description, status, author_agent_id, created_at, updated_at
		FROM plans WHERE id = ?
	`, id).Scan(&p.ID, &p.Name, &branchName, &description, &p.Status,
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
	return &p, nil
}

func (s *Storage) listItems(ctx context.Context, planID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_id, title, description, status, position, claimed_by_agent_id, created_at, updated_at
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
	return items, rows.Err()
}

func (s *Storage) ListPlans(ctx context.Context, branchName string) ([]Plan, error) {
	var (
		query string
		args  []any
	)
	if branchName != "" {
		query = `SELECT id, name, branch_name, description, status, author_agent_id, created_at, updated_at
		         FROM plans WHERE branch_name = ? OR branch_name IS NULL ORDER BY created_at DESC`
		args = []any{branchName}
	} else {
		query = `SELECT id, name, branch_name, description, status, author_agent_id, created_at, updated_at
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
		var bn, desc sql.NullString
		var ca, ua string
		if err := rows.Scan(&p.ID, &p.Name, &bn, &desc, &p.Status,
			&p.AuthorAgentID, &ca, &ua); err != nil {
			return nil, fmt.Errorf("scanning plan: %w", err)
		}
		if bn.Valid {
			p.BranchName = bn.String
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
		    (id, plan_id, title, description, status, position, claimed_by_agent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, item.ID, item.PlanID, item.Title, item.Description, string(item.Status),
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
		SELECT id, plan_id, title, description, status, position, claimed_by_agent_id, created_at, updated_at
		FROM plan_items WHERE id = ? AND plan_id = ?
	`, itemID, planID)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	if err != nil {
		return nil, err
	}
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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (Item, error) {
	var item Item
	var description, claimedBy sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &item.PlanID, &item.Title, &description,
		&item.Status, &item.Position, &claimedBy, &createdAt, &updatedAt); err != nil {
		return item, fmt.Errorf("scanning item: %w", err)
	}
	if description.Valid {
		item.Description = description.String
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
