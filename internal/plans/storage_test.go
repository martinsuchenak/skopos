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
