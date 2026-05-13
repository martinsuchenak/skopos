# Plans & Todo Lists Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add named, branch-scoped plan lists so agents can create and track work items across sessions, exposed via REST, MCP, CLI, and web UI.

**Architecture:** A new `internal/plans` package follows the same `handler → service → storage` layering as `internal/blackboard`. Two SQLite tables (`plans` + `plan_items`) with ON DELETE CASCADE. Routes and MCP tools use the same `init()` registration pattern. No new infrastructure.

**Tech Stack:** Go, SQLite (`modernc.org/sqlite`), Alpine.js, Tailwind CSS (web), `github.com/paularlott/cli` (CLI), `github.com/paularlott/mcp` (MCP).

---

## File Map

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/db/schema.sql` | Modify | Add `plans` + `plan_items` tables |
| `internal/plans/models.go` | Create | Types, constants, error vars, input structs |
| `internal/plans/storage.go` | Create | `Store` interface + `Storage` implementation |
| `internal/plans/storage_test.go` | Create | Real `:memory:` SQLite storage tests |
| `internal/plans/service.go` | Create | Business logic, validation, ID generation |
| `internal/plans/service_test.go` | Create | Fake-store service unit tests |
| `internal/plans/handler.go` | Create | HTTP handler for 8 REST endpoints |
| `internal/plans/handler_test.go` | Create | Real SQLite HTTP integration tests |
| `cmd/routes/plans_routes.go` | Create | Route registration via `init()` |
| `cmd/routes/plans_routes_test.go` | Create | Registration smoke test |
| `cmd/routes/api_routes.go` | Modify | Add `plansRegistrations`, `RegisterPlans`, update `RegisterRoutes` |
| `cmd/mcp/plan_create_tool.go` | Create | `plan_create` MCP tool |
| `cmd/mcp/plan_create_tool_test.go` | Create | Registration check |
| `cmd/mcp/plan_read_tool.go` | Create | `plan_read` MCP tool |
| `cmd/mcp/plan_read_tool_test.go` | Create | Registration check |
| `cmd/mcp/plan_add_item_tool.go` | Create | `plan_add_item` MCP tool |
| `cmd/mcp/plan_add_item_tool_test.go` | Create | Registration check |
| `cmd/mcp/plan_update_item_tool.go` | Create | `plan_update_item` MCP tool |
| `cmd/mcp/plan_update_item_tool_test.go` | Create | Registration check |
| `cmd/mcp/mcp.go` | Modify | Add `plansToolRegistrations`, `RegisterPlansTool`, update `StartMCPServer` |
| `cmd/plans.go` | Create | CLI `plan` command with subcommands |
| `cmd/plans_test.go` | Create | CLI smoke test |
| `cmd/serve.go` | Modify | Wire plans storage/service/handler |
| `web/src/main.ts` | Modify | Add Plans tab state and data fetching |
| `web/templates/base.html` | Modify | Add Plans tab UI |

---

## Task 1: Schema + Models

**Files:**
- Modify: `internal/db/schema.sql`
- Create: `internal/plans/models.go`

- [ ] **Step 1: Add plans tables to schema.sql**

Append to the end of `internal/db/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS plans (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    branch_name     TEXT,
    description     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active',
    author_agent_id TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plan_items (
    id                  TEXT PRIMARY KEY,
    plan_id             TEXT NOT NULL,
    title               TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'pending',
    position            INTEGER NOT NULL DEFAULT 0,
    claimed_by_agent_id TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_plans_branch    ON plans(branch_name);
CREATE INDEX IF NOT EXISTS idx_plan_items_plan ON plan_items(plan_id, position);
```

- [ ] **Step 2: Create `internal/plans/models.go`**

```go
package plans

import (
	"errors"
	"time"
)

type PlanStatus string
type ItemStatus string

const (
	PlanActive    PlanStatus = "active"
	PlanCompleted PlanStatus = "completed"
	PlanArchived  PlanStatus = "archived"
)

const (
	ItemPending    ItemStatus = "pending"
	ItemInProgress ItemStatus = "in_progress"
	ItemDone       ItemStatus = "done"
	ItemBlocked    ItemStatus = "blocked"
)

type Plan struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	BranchName    string     `json:"branch_name,omitempty"`
	Description   string     `json:"description,omitempty"`
	Status        PlanStatus `json:"status"`
	AuthorAgentID string     `json:"author_agent_id"`
	Items         []Item     `json:"items,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Item struct {
	ID               string     `json:"id"`
	PlanID           string     `json:"plan_id"`
	Title            string     `json:"title"`
	Description      string     `json:"description,omitempty"`
	Status           ItemStatus `json:"status"`
	Position         int        `json:"position"`
	ClaimedByAgentID string     `json:"claimed_by_agent_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

var (
	ErrInvalidInput = errors.New("invalid plans input")
	ErrNotFound     = errors.New("not found")
)

type CreatePlanInput struct {
	Name          string `json:"name"`
	BranchName    string `json:"branch_name,omitempty"`
	Description   string `json:"description,omitempty"`
	AuthorAgentID string `json:"author_agent_id"`
}

type UpdatePlanInput struct {
	Name        string     `json:"name,omitempty"`
	Description string     `json:"description,omitempty"`
	Status      PlanStatus `json:"status,omitempty"`
}

type CreateItemInput struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Position    *int   `json:"position,omitempty"`
}

// UpdateItemInput: ClaimedByAgentID nil = don't change, *"" = release claim, *"id" = claim.
type UpdateItemInput struct {
	Status           ItemStatus `json:"status,omitempty"`
	ClaimedByAgentID *string    `json:"claimed_by_agent_id"`
}
```

- [ ] **Step 3: Verify it compiles**

Run:
```bash
go build ./internal/plans/...
```
Expected: no output (success), or compilation error if typo.

- [ ] **Step 4: Commit**

```bash
git add internal/db/schema.sql internal/plans/models.go
git commit -m "feat: add plans schema and models"
```

---

## Task 2: Storage

**Files:**
- Create: `internal/plans/storage_test.go`
- Create: `internal/plans/storage.go`

- [ ] **Step 1: Write `internal/plans/storage_test.go`**

```go
package plans

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func TestStorageCreateAndGetPlan(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	plan := Plan{
		ID:            "plan-1",
		Name:          "Auth refactor",
		BranchName:    "feat-auth",
		Description:   "Refactor JWT handling",
		Status:        PlanActive,
		AuthorAgentID: "agent-1",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	got, err := s.GetPlan(ctx, "plan-1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.Name != "Auth refactor" {
		t.Errorf("name: got %q", got.Name)
	}
	if got.BranchName != "feat-auth" {
		t.Errorf("branch_name: got %q", got.BranchName)
	}
	if got.Items != nil {
		t.Errorf("expected nil items, got %v", got.Items)
	}
}

func TestStorageGetPlanWithItems(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "Plan", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	for i, title := range []string{"First", "Second", "Third"} {
		pos := i
		if err := s.AddItem(ctx, Item{
			ID: "item-" + title, PlanID: "p1", Title: title,
			Status: ItemPending, Position: pos,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("add item %s: %v", title, err)
		}
	}

	plan, err := s.GetPlan(ctx, "p1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if len(plan.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(plan.Items))
	}
	if plan.Items[0].Title != "First" {
		t.Errorf("expected first item First, got %q", plan.Items[0].Title)
	}
}

func TestStorageListPlansBranchFilter(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// project-wide (no branch)
	if err := s.CreatePlan(ctx, Plan{
		ID: "global", Name: "Global", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create global plan: %v", err)
	}
	// feat-auth branch
	if err := s.CreatePlan(ctx, Plan{
		ID: "auth", Name: "Auth", BranchName: "feat-auth", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create auth plan: %v", err)
	}
	// main branch
	if err := s.CreatePlan(ctx, Plan{
		ID: "main-plan", Name: "Main", BranchName: "main", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create main plan: %v", err)
	}

	// Filter for feat-auth: should include "auth" + "global" (project-wide)
	plans, err := s.ListPlans(ctx, "feat-auth")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans for feat-auth, got %d", len(plans))
	}

	// No filter: all 3 plans
	all, err := s.ListPlans(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 plans total, got %d", len(all))
	}

	// ListPlans does not populate items
	for _, p := range all {
		if p.Items != nil {
			t.Errorf("expected nil items in list, got %v for plan %s", p.Items, p.ID)
		}
	}
}

func TestStorageCascadeDeleteRemovesItems(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p-del", Name: "To delete", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "item-del", PlanID: "p-del", Title: "Item", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	if err := s.DeletePlan(ctx, "p-del"); err != nil {
		t.Fatalf("delete plan: %v", err)
	}

	_, err := s.GetPlan(ctx, "p-del")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStorageUpdateItemStatusAndClaim(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "i1", PlanID: "p1", Title: "Item", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	// Update status + claim
	agent := "claude-macbook"
	if err := s.UpdateItem(ctx, "p1", "i1", UpdateItemInput{
		Status:           ItemInProgress,
		ClaimedByAgentID: &agent,
	}); err != nil {
		t.Fatalf("update item: %v", err)
	}

	item, err := s.GetItem(ctx, "p1", "i1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.Status != ItemInProgress {
		t.Errorf("expected in_progress, got %q", item.Status)
	}
	if item.ClaimedByAgentID != "claude-macbook" {
		t.Errorf("expected claimed by claude-macbook, got %q", item.ClaimedByAgentID)
	}
}

func TestStorageReleaseClaim(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	agent := "claude-macbook"
	if err := s.AddItem(ctx, Item{
		ID: "i1", PlanID: "p1", Title: "Item", Status: ItemPending, Position: 0,
		ClaimedByAgentID: agent, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	// Release claim with empty string pointer
	empty := ""
	if err := s.UpdateItem(ctx, "p1", "i1", UpdateItemInput{ClaimedByAgentID: &empty}); err != nil {
		t.Fatalf("release claim: %v", err)
	}

	item, err := s.GetItem(ctx, "p1", "i1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.ClaimedByAgentID != "" {
		t.Errorf("expected empty claim, got %q", item.ClaimedByAgentID)
	}
}

func TestStorageDeleteItem(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "i1", PlanID: "p1", Title: "Item", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	if err := s.DeleteItem(ctx, "p1", "i1"); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	_, err := s.GetItem(ctx, "p1", "i1")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStorageGetPlanNotFound(t *testing.T) {
	s := testStorage(t)
	_, err := s.GetPlan(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/plans/... -run TestStorage -count=1
```
Expected: compilation error (storage.go does not exist yet).

- [ ] **Step 3: Create `internal/plans/storage.go`**

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/plans/... -run TestStorage -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/internal/plans`

- [ ] **Step 5: Commit**

```bash
git add internal/plans/storage.go internal/plans/storage_test.go
git commit -m "feat: add plans storage with SQLite backend"
```

---

## Task 3: Service

**Files:**
- Create: `internal/plans/service_test.go`
- Create: `internal/plans/service.go`

- [ ] **Step 1: Write `internal/plans/service_test.go`**

```go
package plans

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type fakeStore struct {
	plans     map[string]*Plan
	createErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{plans: make(map[string]*Plan)}
}

func (f *fakeStore) CreatePlan(_ context.Context, plan Plan) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.plans[plan.ID] = &plan
	return nil
}

func (f *fakeStore) GetPlan(_ context.Context, id string) (*Plan, error) {
	p, ok := f.plans[id]
	if !ok {
		return nil, fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	return p, nil
}

func (f *fakeStore) ListPlans(_ context.Context, _ string) ([]Plan, error) {
	var result []Plan
	for _, p := range f.plans {
		result = append(result, *p)
	}
	return result, nil
}

func (f *fakeStore) UpdatePlan(_ context.Context, id string, _ UpdatePlanInput) error {
	if _, ok := f.plans[id]; !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	return nil
}

func (f *fakeStore) DeletePlan(_ context.Context, id string) error {
	if _, ok := f.plans[id]; !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	delete(f.plans, id)
	return nil
}

func (f *fakeStore) AddItem(_ context.Context, item Item) error {
	p, ok := f.plans[item.PlanID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, item.PlanID)
	}
	p.Items = append(p.Items, item)
	return nil
}

func (f *fakeStore) UpdateItem(_ context.Context, planID, itemID string, _ UpdateItemInput) error {
	p, ok := f.plans[planID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for _, item := range p.Items {
		if item.ID == itemID {
			return nil
		}
	}
	return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func (f *fakeStore) GetItem(_ context.Context, planID, itemID string) (*Item, error) {
	p, ok := f.plans[planID]
	if !ok {
		return nil, fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for i, item := range p.Items {
		if item.ID == itemID {
			return &p.Items[i], nil
		}
	}
	return nil, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func (f *fakeStore) DeleteItem(_ context.Context, planID, itemID string) error {
	p, ok := f.plans[planID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for _, item := range p.Items {
		if item.ID == itemID {
			return nil
		}
	}
	return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func TestServiceCreatePlanMissingName(t *testing.T) {
	svc := NewService(newFakeStore())
	_, err := svc.CreatePlan(context.Background(), CreatePlanInput{AuthorAgentID: "agent-1"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceCreatePlanMissingAuthorAgentID(t *testing.T) {
	svc := NewService(newFakeStore())
	_, err := svc.CreatePlan(context.Background(), CreatePlanInput{Name: "My Plan"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceCreatePlanSuccess(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	plan, err := svc.CreatePlan(context.Background(), CreatePlanInput{
		Name:          "Auth refactor",
		AuthorAgentID: "agent-1",
		BranchName:    "feat-auth",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.ID == "" {
		t.Fatal("expected generated ID")
	}
	if plan.Status != PlanActive {
		t.Errorf("expected active status, got %q", plan.Status)
	}
	if len(store.plans) != 1 {
		t.Fatalf("expected 1 stored plan, got %d", len(store.plans))
	}
}

func TestServiceUpdatePlanInvalidStatus(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a"}
	svc := NewService(store)
	err := svc.UpdatePlan(context.Background(), "p1", UpdatePlanInput{Status: "unknown"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceUpdatePlanEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.UpdatePlan(context.Background(), "", UpdatePlanInput{Status: PlanCompleted})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceDeletePlanEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.DeletePlan(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceAddItemMissingTitle(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a"}
	svc := NewService(store)
	_, err := svc.AddItem(context.Background(), "p1", CreateItemInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceAddItemPositionDefaultsToEnd(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemPending, Position: 1},
		},
	}
	svc := NewService(store)
	item, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.Position != 2 {
		t.Errorf("expected position 2 (append at end), got %d", item.Position)
	}
}

func TestServiceUpdateItemInvalidStatus(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{{ID: "i1", PlanID: "p1", Title: "T", Status: ItemPending}},
	}
	svc := NewService(store)
	_, err := svc.UpdateItem(context.Background(), "p1", "i1", UpdateItemInput{Status: "invalid"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceDeleteItemEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.DeleteItem(context.Background(), "p1", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/plans/... -run TestService -count=1
```
Expected: compilation error (service.go does not exist yet).

- [ ] **Step 3: Create `internal/plans/service.go`**

```go
package plans

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) CreatePlan(ctx context.Context, input CreatePlanInput) (*Plan, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.AuthorAgentID = strings.TrimSpace(input.AuthorAgentID)
	if input.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if input.AuthorAgentID == "" {
		return nil, fmt.Errorf("%w: author_agent_id is required", ErrInvalidInput)
	}
	now := time.Now().UTC()
	plan := Plan{
		ID:            generateID(),
		Name:          input.Name,
		BranchName:    strings.TrimSpace(input.BranchName),
		Description:   strings.TrimSpace(input.Description),
		Status:        PlanActive,
		AuthorAgentID: input.AuthorAgentID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreatePlan(ctx, plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *Service) GetPlan(ctx context.Context, id string) (*Plan, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.GetPlan(ctx, id)
}

func (s *Service) ListPlans(ctx context.Context, branchName string) ([]Plan, error) {
	return s.store.ListPlans(ctx, strings.TrimSpace(branchName))
}

func (s *Service) UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	if input.Status != "" && !validPlanStatus(input.Status) {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidInput, input.Status)
	}
	return s.store.UpdatePlan(ctx, id, input)
}

func (s *Service) DeletePlan(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.DeletePlan(ctx, id)
}

func (s *Service) AddItem(ctx context.Context, planID string, input CreateItemInput) (*Item, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}

	var pos int
	if input.Position != nil {
		pos = *input.Position
	} else {
		plan, err := s.store.GetPlan(ctx, planID)
		if err != nil {
			return nil, err
		}
		pos = len(plan.Items)
	}

	now := time.Now().UTC()
	item := Item{
		ID:          generateID(),
		PlanID:      planID,
		Title:       input.Title,
		Description: strings.TrimSpace(input.Description),
		Status:      ItemPending,
		Position:    pos,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.AddItem(ctx, item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Service) UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) (*Item, error) {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	if itemID == "" {
		return nil, fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	if input.Status != "" && !validItemStatus(input.Status) {
		return nil, fmt.Errorf("%w: invalid status %q", ErrInvalidInput, input.Status)
	}
	if err := s.store.UpdateItem(ctx, planID, itemID, input); err != nil {
		return nil, err
	}
	return s.store.GetItem(ctx, planID, itemID)
}

func (s *Service) DeleteItem(ctx context.Context, planID, itemID string) error {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	if planID == "" {
		return fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	if itemID == "" {
		return fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	return s.store.DeleteItem(ctx, planID, itemID)
}

func validPlanStatus(s PlanStatus) bool {
	switch s {
	case PlanActive, PlanCompleted, PlanArchived:
		return true
	}
	return false
}

func validItemStatus(s ItemStatus) bool {
	switch s {
	case ItemPending, ItemInProgress, ItemDone, ItemBlocked:
		return true
	}
	return false
}

func generateID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/plans/... -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/internal/plans`

- [ ] **Step 5: Commit**

```bash
git add internal/plans/service.go internal/plans/service_test.go
git commit -m "feat: add plans service with validation"
```

---

## Task 4: Handler + Routes

**Files:**
- Create: `internal/plans/handler_test.go`
- Create: `internal/plans/handler.go`
- Create: `cmd/routes/plans_routes_test.go`
- Create: `cmd/routes/plans_routes.go`
- Modify: `cmd/routes/api_routes.go`

- [ ] **Step 1: Write `internal/plans/handler_test.go`**

```go
package plans

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewHandler(NewService(NewStorage(sqlDB)), apiKey)
}

func TestHandlerCreatePlanRequiresAuth(t *testing.T) {
	h := testHandler(t, "secret")
	body := bytes.NewBufferString(`{"name":"Plan","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandlerCreateAndGetPlan(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"name":"Auth refactor","branch_name":"feat-auth","author_agent_id":"agent-1"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	var plan Plan
	if err := json.NewDecoder(w.Body).Decode(&plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.ID == "" {
		t.Fatal("expected ID")
	}

	req2 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.GetPlan(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w2.Code)
	}
	var got Plan
	if err := json.NewDecoder(w2.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Auth refactor" {
		t.Errorf("expected name Auth refactor, got %q", got.Name)
	}
}

func TestHandlerListPlans(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"name":"P1","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	req2 := httptest.NewRequest("GET", "/api/plans", nil)
	w2 := httptest.NewRecorder()
	h.ListPlans(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w2.Code)
	}
	var plans []Plan
	if err := json.NewDecoder(w2.Body).Decode(&plans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
}

func TestHandlerAddItemAndPatchStatus(t *testing.T) {
	h := testHandler(t, "")

	// Create plan
	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	// Add item
	itemBody := bytes.NewBufferString(`{"title":"Fix auth bug"}`)
	req2 := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", itemBody)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.AddItem(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("add item: expected 201, got %d body=%s", w2.Code, w2.Body.String())
	}
	var item Item
	json.NewDecoder(w2.Body).Decode(&item)

	// PATCH item status
	patchBody := bytes.NewBufferString(`{"status":"done"}`)
	req3 := httptest.NewRequest("PATCH", "/api/plans/"+plan.ID+"/items/"+item.ID, patchBody)
	req3.SetPathValue("id", plan.ID)
	req3.SetPathValue("item_id", item.ID)
	w3 := httptest.NewRecorder()
	h.UpdateItem(w3, req3)
	if w3.Code != http.StatusNoContent {
		t.Fatalf("patch item: expected 204, got %d body=%s", w3.Code, w3.Body.String())
	}

	// Verify via GetPlan
	req4 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req4.SetPathValue("id", plan.ID)
	w4 := httptest.NewRecorder()
	h.GetPlan(w4, req4)
	var updated Plan
	json.NewDecoder(w4.Body).Decode(&updated)
	if len(updated.Items) != 1 || updated.Items[0].Status != ItemDone {
		t.Errorf("expected item done, got %v", updated.Items)
	}
}

func TestHandlerDeletePlanCascades(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	// Delete plan
	req2 := httptest.NewRequest("DELETE", "/api/plans/"+plan.ID, nil)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.DeletePlan(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", w2.Code)
	}

	// Plan is gone
	req3 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req3.SetPathValue("id", plan.ID)
	w3 := httptest.NewRecorder()
	h.GetPlan(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHandlerGetPlanNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("GET", "/api/plans/missing", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.GetPlan(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/plans/... -run TestHandler -count=1
```
Expected: compilation error (handler.go does not exist yet).

- [ ] **Step 3: Create `internal/plans/handler.go`**

```go
package plans

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	apiKey  string
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.apiKey == "" {
		return true
	}
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	return key == h.apiKey
}

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input CreatePlanInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	plan, err := h.service.CreatePlan(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusCreated, plan)
}

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	plans, err := h.service.ListPlans(r.Context(), branch)
	if err != nil {
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if plans == nil {
		plans = []Plan{}
	}
	rest.RespondJSON(w, http.StatusOK, plans)
}

func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := h.service.GetPlan(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	var input UpdatePlanInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.UpdatePlan(r.Context(), id, input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) DeletePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.DeletePlan(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AddItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	var input CreateItemInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.AddItem(r.Context(), planID, input)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	rest.RespondJSON(w, http.StatusCreated, item)
}

func (h *Handler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	var input UpdateItemInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := h.service.UpdateItem(r.Context(), planID, itemID, input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) DeleteItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	if err := h.service.DeleteItem(r.Context(), planID, itemID); err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Run handler tests to verify they pass**

```bash
go test ./internal/plans/... -run TestHandler -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/internal/plans`

- [ ] **Step 5: Write `cmd/routes/plans_routes_test.go`**

```go
package routes

import (
	"context"
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/plans"
)

func TestRegisterPlansRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerPlansRoutes(mux, plans.NewHandler(
		plans.NewService(&noopPlansStore{}), "",
	))
}

type noopPlansStore struct{}

func (s *noopPlansStore) CreatePlan(_ context.Context, _ plans.Plan) error { return nil }
func (s *noopPlansStore) GetPlan(_ context.Context, _ string) (*plans.Plan, error) {
	return nil, plans.ErrNotFound
}
func (s *noopPlansStore) ListPlans(_ context.Context, _ string) ([]plans.Plan, error) {
	return nil, nil
}
func (s *noopPlansStore) UpdatePlan(_ context.Context, _ string, _ plans.UpdatePlanInput) error {
	return nil
}
func (s *noopPlansStore) DeletePlan(_ context.Context, _ string) error { return nil }
func (s *noopPlansStore) AddItem(_ context.Context, _ plans.Item) error { return nil }
func (s *noopPlansStore) UpdateItem(_ context.Context, _, _ string, _ plans.UpdateItemInput) error {
	return nil
}
func (s *noopPlansStore) GetItem(_ context.Context, _, _ string) (*plans.Item, error) {
	return nil, plans.ErrNotFound
}
func (s *noopPlansStore) DeleteItem(_ context.Context, _, _ string) error { return nil }
```

- [ ] **Step 6: Run route test to verify it fails**

```bash
go test ./cmd/routes/... -run TestRegisterPlansRoutes -count=1
```
Expected: compilation error (plans_routes.go does not exist yet).

- [ ] **Step 7: Create `cmd/routes/plans_routes.go`**

```go
package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/plans"
)

func init() {
	RegisterPlans(registerPlansRoutes)
}

func registerPlansRoutes(mux *http.ServeMux, h *plans.Handler) {
	mux.HandleFunc("POST /api/plans", h.CreatePlan)
	mux.HandleFunc("GET /api/plans", h.ListPlans)
	mux.HandleFunc("GET /api/plans/{id}", h.GetPlan)
	mux.HandleFunc("PATCH /api/plans/{id}", h.UpdatePlan)
	mux.HandleFunc("DELETE /api/plans/{id}", h.DeletePlan)
	mux.HandleFunc("POST /api/plans/{id}/items", h.AddItem)
	mux.HandleFunc("PATCH /api/plans/{id}/items/{item_id}", h.UpdateItem)
	mux.HandleFunc("DELETE /api/plans/{id}/items/{item_id}", h.DeleteItem)
}
```

- [ ] **Step 8: Update `cmd/routes/api_routes.go`**

Add `plansRegistrations` and `RegisterPlans` variables, and update `RegisterRoutes` to accept and wire the plans handler. Find the current content of the file and make these changes:

Add after the `blackboardRegistrations` line:
```go
var plansRegistrations []func(*http.ServeMux, *plans.Handler)

func RegisterPlans(fn func(*http.ServeMux, *plans.Handler)) {
	plansRegistrations = append(plansRegistrations, fn)
}
```

Update the `RegisterRoutes` signature and body:
```go
func RegisterRoutes(mux *http.ServeMux, statusHandler *status.Handler, blackboardHandler *blackboard.Handler, plansHandler *plans.Handler) {
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /metrics", metricsHandler)
	registerWebRoutes(mux)

	for _, fn := range registrations {
		fn(mux, statusHandler)
	}
	for _, fn := range blackboardRegistrations {
		fn(mux, blackboardHandler)
	}
	for _, fn := range plansRegistrations {
		fn(mux, plansHandler)
	}
}
```

Add the `plans` import to the import block:
```go
import (
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	appweb "github.com/martinsuchenak/skopos/web"
)
```

- [ ] **Step 9: Run all route tests to verify they pass**

```bash
go test ./cmd/routes/... -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/cmd/routes`

- [ ] **Step 10: Commit**

```bash
git add internal/plans/handler.go internal/plans/handler_test.go \
        cmd/routes/plans_routes.go cmd/routes/plans_routes_test.go \
        cmd/routes/api_routes.go
git commit -m "feat: add plans HTTP handler and routes"
```

---

## Task 5: MCP Tools + serve.go Wiring

**Files:**
- Create: `cmd/mcp/plan_create_tool.go` + `plan_create_tool_test.go`
- Create: `cmd/mcp/plan_read_tool.go` + `plan_read_tool_test.go`
- Create: `cmd/mcp/plan_add_item_tool.go` + `plan_add_item_tool_test.go`
- Create: `cmd/mcp/plan_update_item_tool.go` + `plan_update_item_tool_test.go`
- Modify: `cmd/mcp/mcp.go`
- Modify: `cmd/serve.go`

- [ ] **Step 1: Write the four MCP tool test files**

`cmd/mcp/plan_create_tool_test.go`:
```go
package mcp

import "testing"

func TestPlanCreateToolRegistered(t *testing.T) {
	found := false
	for range plansToolRegistrations {
		found = true
		break
	}
	if !found {
		t.Fatal("plansToolRegistrations should not be empty")
	}
}
```

`cmd/mcp/plan_read_tool_test.go`:
```go
package mcp

import "testing"

func TestPlanReadToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 2 {
		t.Fatalf("expected at least 2 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
```

`cmd/mcp/plan_add_item_tool_test.go`:
```go
package mcp

import "testing"

func TestPlanAddItemToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 3 {
		t.Fatalf("expected at least 3 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
```

`cmd/mcp/plan_update_item_tool_test.go`:
```go
package mcp

import "testing"

func TestPlanUpdateItemToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 4 {
		t.Fatalf("expected at least 4 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/mcp/... -run TestPlan -count=1
```
Expected: compilation error (`plansToolRegistrations` undefined).

- [ ] **Step 3: Update `cmd/mcp/mcp.go`**

Add `plansToolRegistrations` and `RegisterPlansTool`, and update `StartMCPServer` to accept and wire the plans service. Replace the current `mcp.go` content with:

```go
package mcp

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/paularlott/logger"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server, *status.Service)
var blackboardToolRegistrations []func(*mcplib.Server, *blackboard.Service)
var plansToolRegistrations []func(*mcplib.Server, *plans.Service)

func RegisterTool(fn func(*mcplib.Server, *status.Service)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func RegisterBlackboardTool(fn func(*mcplib.Server, *blackboard.Service)) {
	blackboardToolRegistrations = append(blackboardToolRegistrations, fn)
}

func RegisterPlansTool(fn func(*mcplib.Server, *plans.Service)) {
	plansToolRegistrations = append(plansToolRegistrations, fn)
}

func StartMCPServer(log logger.Logger, statusService *status.Service, blackboardService *blackboard.Service, plansService *plans.Service) {
	server := mcplib.NewServer("skopos-mcp", "1.0.0")

	for _, fn := range toolRegistrations {
		fn(server, statusService)
	}
	for _, fn := range blackboardToolRegistrations {
		fn(server, blackboardService)
	}
	for _, fn := range plansToolRegistrations {
		fn(server, plansService)
	}

	go func() {
		log.Info("starting MCP server on :9000")
		http.HandleFunc("/mcp", server.HandleRequest)
		if err := http.ListenAndServe(":9000", nil); err != nil {
			log.Error("MCP server error", "error", err)
		}
	}()
}
```

- [ ] **Step 4: Create `cmd/mcp/plan_create_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanCreateTool)
}

func registerPlanCreateTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_create", "Create a named plan for tracking work items",
			mcplib.String("name", "Plan name", mcplib.Required()),
			mcplib.String("author_agent_id", "Identifier of the creating agent", mcplib.Required()),
			mcplib.String("branch_name", "Branch this plan belongs to (omit for project-wide)"),
			mcplib.String("description", "Optional description of the plan"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			plan, err := service.CreatePlan(ctx, plans.CreatePlanInput{
				Name:          req.StringOr("name", ""),
				AuthorAgentID: req.StringOr("author_agent_id", ""),
				BranchName:    req.StringOr("branch_name", ""),
				Description:   req.StringOr("description", ""),
			})
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(map[string]string{"id": plan.ID, "name": plan.Name}), nil
		},
	)
}
```

- [ ] **Step 5: Create `cmd/mcp/plan_read_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanReadTool)
}

func registerPlanReadTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_read", "Get a plan with all its items",
			mcplib.String("id", "Plan ID", mcplib.Required()),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			plan, err := service.GetPlan(ctx, req.StringOr("id", ""))
			if err != nil {
				return nil, mcplib.NewToolErrorInternal(err.Error())
			}
			return mcplib.NewToolResponseJSON(plan), nil
		},
	)
}
```

- [ ] **Step 6: Create `cmd/mcp/plan_add_item_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanAddItemTool)
}

func registerPlanAddItemTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_add_item", "Add a work item to a plan",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("title", "Item title", mcplib.Required()),
			mcplib.String("description", "Optional description"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			item, err := service.AddItem(ctx, req.StringOr("plan_id", ""), plans.CreateItemInput{
				Title:       req.StringOr("title", ""),
				Description: req.StringOr("description", ""),
			})
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(item), nil
		},
	)
}
```

- [ ] **Step 7: Create `cmd/mcp/plan_update_item_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/plans"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterPlansTool(registerPlanUpdateItemTool)
}

func registerPlanUpdateItemTool(server *mcplib.Server, service *plans.Service) {
	server.RegisterTool(
		mcplib.NewTool("plan_update_item", "Update an item's status or claim it",
			mcplib.String("plan_id", "Plan ID", mcplib.Required()),
			mcplib.String("item_id", "Item ID", mcplib.Required()),
			mcplib.String("status", "New status: pending, in_progress, done, or blocked"),
			mcplib.String("claimed_by_agent_id", "Agent ID claiming the item; pass empty string to release"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := plans.UpdateItemInput{
				Status: plans.ItemStatus(req.StringOr("status", "")),
			}
			// Use sentinel to distinguish "not provided" from "provided as empty string"
			if claimed := req.StringOr("claimed_by_agent_id", "\x00"); claimed != "\x00" {
				input.ClaimedByAgentID = &claimed
			}
			item, err := service.UpdateItem(ctx,
				req.StringOr("plan_id", ""),
				req.StringOr("item_id", ""),
				input,
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(item), nil
		},
	)
}
```

- [ ] **Step 8: Update `cmd/serve.go` to wire plans**

In `serve.go`, after the `blackboardHandler` lines, add:
```go
plansStorage := plans.NewStorage(conn.SQL)
plansService := plans.NewService(plansStorage)
plansHandler := plans.NewHandler(plansService, cmd.GetString("api-key"))
```

Update the `mcpserver.StartMCPServer` call:
```go
mcpserver.StartMCPServer(log, statusService, blackboardService, plansService)
```

Update the `routes.RegisterRoutes` call:
```go
routes.RegisterRoutes(mux, statusHandler, blackboardHandler, plansHandler)
```

Add `plans` to the imports in `serve.go`:
```go
"github.com/martinsuchenak/skopos/internal/plans"
```

- [ ] **Step 9: Run all MCP tests**

```bash
go test ./cmd/mcp/... -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/cmd/mcp`

- [ ] **Step 10: Verify full build compiles**

```bash
go build ./...
```
Expected: no output (success).

- [ ] **Step 11: Commit**

```bash
git add cmd/mcp/plan_create_tool.go cmd/mcp/plan_create_tool_test.go \
        cmd/mcp/plan_read_tool.go cmd/mcp/plan_read_tool_test.go \
        cmd/mcp/plan_add_item_tool.go cmd/mcp/plan_add_item_tool_test.go \
        cmd/mcp/plan_update_item_tool.go cmd/mcp/plan_update_item_tool_test.go \
        cmd/mcp/mcp.go cmd/serve.go
git commit -m "feat: add plans MCP tools and wire serve.go"
```

---

## Task 6: CLI

**Files:**
- Create: `cmd/plans_test.go`
- Create: `cmd/plans.go`

- [ ] **Step 1: Write `cmd/plans_test.go`**

```go
package cmd

import "testing"

func TestPlanCmdExists(t *testing.T) {
	cmd := planCmd()
	if cmd == nil {
		t.Fatal("planCmd should not return nil")
	}
	if cmd.Name != "plan" {
		t.Fatalf("expected plan command, got %q", cmd.Name)
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("expected plan subcommands")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/... -run TestPlanCmdExists -count=1
```
Expected: compilation error (`planCmd` undefined).

- [ ] **Step 3: Create `cmd/plans.go`**

```go
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/paularlott/cli"
)

func init() {
	Register(planCmd())
}

func planCmd() *cli.Command {
	return &cli.Command{
		Name:  "plan",
		Usage: "Manage agent plans and todo lists",
		Commands: []*cli.Command{
			planCreateCmd(),
			planListCmd(),
			planShowCmd(),
			planDoneCmd(),
			planItemCmd(),
		},
	}
}

func planCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a new plan",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "name", Usage: "Plan name"},
			&cli.StringFlag{Name: "branch", Usage: "Branch name (omit for project-wide)"},
			&cli.StringFlag{Name: "description", Usage: "Optional description"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			plan, err := plansPost(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), plans.CreatePlanInput{
				Name:          cmd.GetString("name"),
				BranchName:    cmd.GetString("branch"),
				Description:   cmd.GetString("description"),
				AuthorAgentID: cmd.GetString("agent-id"),
			})
			if err != nil {
				return err
			}
			fmt.Printf("created id=%s name=%q\n", plan.ID, plan.Name)
			return nil
		},
	}
}

func planListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List plans",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Filter by branch name"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ps, err := plansGetList(ctx, cmd.GetString("server-url"), cmd.GetString("branch"))
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Println("no plans")
				return nil
			}
			fmt.Printf("%-36s  %-8s  %-12s  %s\n", "ID", "STATUS", "BRANCH", "NAME")
			for _, p := range ps {
				fmt.Printf("%-36s  %-8s  %-12s  %s\n", p.ID, p.Status, p.BranchName, p.Name)
			}
			return nil
		},
	}
}

func planShowCmd() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "Show a plan with its items",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.Arg(0))
			if id == "" {
				return fmt.Errorf("plan id is required")
			}
			plan, err := plansGetOne(ctx, cmd.GetString("server-url"), id)
			if err != nil {
				return err
			}
			fmt.Printf("Plan: %s (%s)\n", plan.Name, plan.Status)
			if plan.BranchName != "" {
				fmt.Printf("Branch: %s\n", plan.BranchName)
			}
			if len(plan.Items) == 0 {
				fmt.Println("No items.")
				return nil
			}
			for _, item := range plan.Items {
				claimed := ""
				if item.ClaimedByAgentID != "" {
					claimed = " [" + item.ClaimedByAgentID + "]"
				}
				fmt.Printf("  [%s] %s%s\n", item.Status, item.Title, claimed)
			}
			return nil
		},
	}
}

func planDoneCmd() *cli.Command {
	return &cli.Command{
		Name:  "done",
		Usage: "Mark a plan as completed",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := strings.TrimSpace(cmd.Arg(0))
			if id == "" {
				return fmt.Errorf("plan id is required")
			}
			return plansPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id,
				plans.UpdatePlanInput{Status: plans.PlanCompleted})
		},
	}
}

func planItemCmd() *cli.Command {
	return &cli.Command{
		Name:  "item",
		Usage: "Manage plan items",
		Commands: []*cli.Command{
			planItemAddCmd(),
			planItemDoneCmd(),
			planItemClaimCmd(),
			planItemUnclaimCmd(),
			planItemBlockCmd(),
		},
	}
}

func planItemAddCmd() *cli.Command {
	return &cli.Command{
		Name:  "add",
		Usage: "Add an item to a plan",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "title", Usage: "Item title"},
			&cli.StringFlag{Name: "description", Usage: "Optional description"},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.Arg(0))
			if planID == "" {
				return fmt.Errorf("plan-id is required")
			}
			item, err := plansItemPost(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, plans.CreateItemInput{
					Title:       cmd.GetString("title"),
					Description: cmd.GetString("description"),
				})
			if err != nil {
				return err
			}
			fmt.Printf("added item id=%s title=%q\n", item.ID, item.Title)
			return nil
		},
	}
}

func planItemDoneCmd() *cli.Command {
	return &cli.Command{
		Name:  "done",
		Usage: "Mark an item as done",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.Arg(0))
			itemID := strings.TrimSpace(cmd.Arg(1))
			if planID == "" || itemID == "" {
				return fmt.Errorf("plan-id and item-id are required")
			}
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{Status: plans.ItemDone},
				"done")
		},
	}
}

func planItemClaimCmd() *cli.Command {
	return &cli.Command{
		Name:  "claim",
		Usage: "Claim an item as being worked on",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.Arg(0))
			itemID := strings.TrimSpace(cmd.Arg(1))
			if planID == "" || itemID == "" {
				return fmt.Errorf("plan-id and item-id are required")
			}
			agentID := cmd.GetString("agent-id")
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{ClaimedByAgentID: &agentID},
				"claim")
		},
	}
}

func planItemUnclaimCmd() *cli.Command {
	return &cli.Command{
		Name:  "unclaim",
		Usage: "Release claim on an item",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.Arg(0))
			itemID := strings.TrimSpace(cmd.Arg(1))
			if planID == "" || itemID == "" {
				return fmt.Errorf("plan-id and item-id are required")
			}
			empty := ""
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{ClaimedByAgentID: &empty},
				"unclaim")
		},
	}
}

func planItemBlockCmd() *cli.Command {
	return &cli.Command{
		Name:  "block",
		Usage: "Mark an item as blocked",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
		},
		Args: true,
		Run: func(ctx context.Context, cmd *cli.Command) error {
			planID := strings.TrimSpace(cmd.Arg(0))
			itemID := strings.TrimSpace(cmd.Arg(1))
			if planID == "" || itemID == "" {
				return fmt.Errorf("plan-id and item-id are required")
			}
			return plansItemPatch(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"),
				planID, itemID, plans.UpdateItemInput{Status: plans.ItemBlocked},
				"block")
		},
	}
}

func plansPost(ctx context.Context, serverURL, apiKey string, input plans.CreatePlanInput) (*plans.Plan, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/plans",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("posting plan: unexpected status %s", resp.Status)
	}
	var plan plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &plan, nil
}

func plansGetList(ctx context.Context, serverURL, branch string) ([]plans.Plan, error) {
	base := strings.TrimRight(serverURL, "/") + "/api/plans"
	q := url.Values{}
	if branch != "" {
		q.Set("branch", branch)
	}
	u := base
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing plans: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing plans: unexpected status %s", resp.Status)
	}
	var result []plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding plans: %w", err)
	}
	return result, nil
}

func plansGetOne(ctx context.Context, serverURL, id string) (*plans.Plan, error) {
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting plan: unexpected status %s", resp.Status)
	}
	var plan plans.Plan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("decoding plan: %w", err)
	}
	return &plan, nil
}

func plansPatch(ctx context.Context, serverURL, apiKey, id string, input plans.UpdatePlanInput) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("patching plan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("patching plan: unexpected status %s", resp.Status)
	}
	return nil
}

func plansItemPost(ctx context.Context, serverURL, apiKey, planID string, input plans.CreateItemInput) (*plans.Item, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" + url.PathEscape(planID) + "/items"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adding item: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("adding item: unexpected status %s", resp.Status)
	}
	var item plans.Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, fmt.Errorf("decoding item: %w", err)
	}
	return &item, nil
}

func plansItemPatch(ctx context.Context, serverURL, apiKey, planID, itemID string, input plans.UpdateItemInput, action string) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encoding input: %w", err)
	}
	u := strings.TrimRight(serverURL, "/") + "/api/plans/" +
		url.PathEscape(planID) + "/items/" + url.PathEscape(itemID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s item: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s item: unexpected status %s", action, resp.Status)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./cmd/... -run TestPlanCmdExists -count=1 -race
```
Expected: `ok  	github.com/martinsuchenak/skopos/cmd`

- [ ] **Step 5: Run full test suite**

```bash
go test ./... -count=1 -race
```
Expected: all packages pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/plans.go cmd/plans_test.go
git commit -m "feat: add plans CLI commands"
```

---

## Task 7: Web UI — Plans Tab

**Files:**
- Modify: `web/src/main.ts`
- Modify: `web/templates/base.html`

- [ ] **Step 1: Add Plans types and state to `web/src/main.ts`**

After the `Bundle` type, add:

```typescript
type PlanItem = {
  id: string;
  plan_id: string;
  title: string;
  description?: string;
  status: string;
  position: number;
  claimed_by_agent_id?: string;
};

type Plan = {
  id: string;
  name: string;
  branch_name?: string;
  description?: string;
  status: string;
  author_agent_id: string;
  items?: PlanItem[];
  created_at: string;
};
```

In `window.app = () => ({`, change `activeTab` type and add plans state:

```typescript
// plans tab
activeTab: 'sessions' as 'sessions' | 'blackboard' | 'plans',
plans: [] as Plan[],
plansLoading: false,
plansBranch: '',
expandedPlan: null as Plan | null,
```

In the `refresh()` method, add a plans refresh after the blackboard block:

```typescript
if (this.activeTab === 'plans') {
  await this.fetchPlans();
}
```

Update `switchTab` to handle the plans tab:

```typescript
async switchTab(tab: 'sessions' | 'blackboard' | 'plans') {
  this.activeTab = tab;
  if (tab === 'blackboard') {
    await this.fetchBundle();
  }
  if (tab === 'plans') {
    await this.fetchPlans();
  }
},
```

Add the plans methods after `scopeClass`:

```typescript
async fetchPlans() {
  this.plansLoading = true;
  try {
    const params = new URLSearchParams();
    if (this.plansBranch.trim()) {
      params.set('branch', this.plansBranch.trim());
    }
    const qs = params.size > 0 ? '?' + params.toString() : '';
    const response = await fetch('/api/plans' + qs);
    this.plans = await response.json();
  } catch {
    this.plans = [];
  } finally {
    this.plansLoading = false;
  }
},

async togglePlan(plan: Plan) {
  if (this.expandedPlan?.id === plan.id) {
    this.expandedPlan = null;
    return;
  }
  const response = await fetch(`/api/plans/${encodeURIComponent(plan.id)}`);
  this.expandedPlan = await response.json();
},

planStatusClass(status: string): string {
  const map: Record<string, string> = {
    active: 'bg-cyan-500/15 text-cyan-300',
    completed: 'bg-emerald-500/15 text-emerald-300',
    archived: 'bg-zinc-700 text-zinc-400',
  };
  return map[status] ?? 'bg-zinc-700 text-zinc-200';
},

itemStatusClass(status: string): string {
  const map: Record<string, string> = {
    done: 'bg-emerald-500/15 text-emerald-300',
    in_progress: 'bg-cyan-500/15 text-cyan-300',
    blocked: 'bg-rose-500/15 text-rose-300',
    pending: 'bg-zinc-700 text-zinc-300',
  };
  return map[status] ?? 'bg-zinc-700 text-zinc-200';
},
```

- [ ] **Step 2: Verify TypeScript compiles**

```bash
go build ./...
```
Expected: no output (Go build doesn't check TypeScript; just verify Go still compiles).

- [ ] **Step 3: Add Plans tab to `web/templates/base.html`**

Add the Plans tab button in the nav alongside Sessions and Blackboard:

```html
<button type="button" @click="switchTab('plans')" class="px-3 py-1.5 transition" :class="activeTab === 'plans' ? 'bg-zinc-700 text-white' : 'text-zinc-400 hover:text-white'">Plans</button>
```

Add the Plans main section after the Blackboard `</main>` closing tag:

```html
<!-- Plans tab -->
<main x-show="activeTab === 'plans'" class="mx-auto max-w-7xl px-4 py-6">
    <div class="mb-6 flex flex-wrap items-center gap-3">
        <h2 class="text-sm font-semibold uppercase tracking-wide text-zinc-400">Plans</h2>
        <div class="flex items-center gap-2 rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5">
            <span class="text-xs text-zinc-500">branch:</span>
            <input type="text" x-model="plansBranch" @input.debounce.300ms="fetchPlans()" placeholder="all" class="w-36 bg-transparent text-sm text-zinc-200 outline-none placeholder-zinc-600" />
        </div>
        <span x-show="plansLoading" class="text-xs text-zinc-500">Loading…</span>
        <span x-show="!plansLoading" class="text-xs text-zinc-500" x-text="`${plans.length} plans`"></span>
    </div>

    <p x-show="!plansLoading && plans.length === 0" class="rounded border border-zinc-800 bg-zinc-900 p-6 text-sm text-zinc-400">
        No plans yet. Agents create plans via the <code class="text-zinc-300">plan_create</code> MCP tool or <code class="text-zinc-300">POST /api/plans</code>.
    </p>

    <div class="space-y-3">
        <template x-for="plan in plans" :key="plan.id">
            <article class="rounded border border-zinc-800 bg-zinc-900">
                <button type="button" @click="togglePlan(plan)" class="w-full p-4 text-left">
                    <div class="flex flex-wrap items-center justify-between gap-2">
                        <div class="flex flex-wrap items-center gap-2">
                            <span class="font-medium text-zinc-100" x-text="plan.name"></span>
                            <span x-show="plan.branch_name" class="rounded bg-violet-500/15 px-2 py-0.5 text-xs text-violet-300" x-text="plan.branch_name"></span>
                            <span x-show="!plan.branch_name" class="rounded bg-indigo-500/15 px-2 py-0.5 text-xs text-indigo-300">project-wide</span>
                        </div>
                        <div class="flex items-center gap-2">
                            <span class="rounded px-2 py-0.5 text-xs" :class="planStatusClass(plan.status)" x-text="plan.status"></span>
                            <span class="text-xs text-zinc-600" x-text="formatTime(plan.created_at)"></span>
                        </div>
                    </div>
                    <p x-show="plan.description" class="mt-1 text-sm text-zinc-400" x-text="plan.description"></p>
                </button>

                <!-- Expanded item list -->
                <div x-show="expandedPlan?.id === plan.id" class="border-t border-zinc-800 px-4 pb-4">
                    <p x-show="!expandedPlan?.items || expandedPlan.items.length === 0" class="pt-4 text-sm text-zinc-500">No items.</p>
                    <div class="mt-3 space-y-2">
                        <template x-for="item in (expandedPlan?.id === plan.id ? expandedPlan?.items ?? [] : [])" :key="item.id">
                            <div class="flex flex-wrap items-start gap-2 rounded border border-zinc-800 bg-zinc-950 p-3">
                                <span class="mt-0.5 rounded px-2 py-0.5 text-xs" :class="itemStatusClass(item.status)" x-text="item.status"></span>
                                <div class="min-w-0 flex-1">
                                    <p class="text-sm text-zinc-200" x-text="item.title"></p>
                                    <p x-show="item.description" class="mt-0.5 text-xs text-zinc-500" x-text="item.description"></p>
                                    <p x-show="item.claimed_by_agent_id" class="mt-0.5 text-xs text-zinc-600" x-text="`claimed by ${item.claimed_by_agent_id}`"></p>
                                </div>
                            </div>
                        </template>
                    </div>
                </div>
            </article>
        </template>
    </div>
</main>
```

- [ ] **Step 4: Run full test suite**

```bash
go test ./... -count=1 -race
```
Expected: all packages pass.

- [ ] **Step 5: Commit**

```bash
git add web/src/main.ts web/templates/base.html
git commit -m "feat: add Plans tab to web dashboard"
```

---

## Final Verification

- [ ] **Run the complete test suite**

```bash
go test ./... -count=1 -race
```
Expected: all packages pass, no races.

- [ ] **Binary smoke test**

```bash
go build -o /tmp/skopos-plans-test . && /tmp/skopos-plans-test plan --help
```
Expected: prints plan command usage with subcommands.
