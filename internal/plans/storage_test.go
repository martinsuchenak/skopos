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
	plans, err := s.ListPlans(ctx, "", "feat-auth")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans for feat-auth, got %d", len(plans))
	}

	// No filter: all 3 plans
	all, err := s.ListPlans(ctx, "", "")
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

func TestStorageListPlansWorkspaceFilter(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "global", Name: "Global", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create global plan: %v", err)
	}
	if err := s.CreatePlan(ctx, Plan{
		ID: "ws1-plan", Name: "WS1 Plan", WorkspaceID: "ws-1", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create ws1 plan: %v", err)
	}
	if err := s.CreatePlan(ctx, Plan{
		ID: "ws2-plan", Name: "WS2 Plan", WorkspaceID: "ws-2", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create ws2 plan: %v", err)
	}

	plans, err := s.ListPlans(ctx, "ws-1", "")
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans (ws1 + global), got %d", len(plans))
	}
	ids := map[string]bool{}
	for _, p := range plans {
		ids[p.ID] = true
	}
	if !ids["global"] || !ids["ws1-plan"] {
		t.Errorf("expected global+ws1-plan, got %v", ids)
	}

	ws2Plans, err := s.ListPlans(ctx, "ws-2", "")
	if err != nil {
		t.Fatalf("list ws2 plans: %v", err)
	}
	if len(ws2Plans) != 2 {
		t.Fatalf("expected 2 plans (ws2 + global), got %d", len(ws2Plans))
	}

	all, err := s.ListPlans(ctx, "", "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 plans total, got %d", len(all))
	}
}

func TestStorageDependencyCRUD(t *testing.T) {
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
		ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item 1: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "i2", PlanID: "p1", Title: "B", Status: ItemPending, Position: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add item 2: %v", err)
	}

	if err := s.AddDependency(ctx, "i2", "i1"); err != nil {
		t.Fatalf("add dependency: %v", err)
	}

	deps, err := s.ListDependencies(ctx, "i2")
	if err != nil {
		t.Fatalf("list deps: %v", err)
	}
	if len(deps) != 1 || deps[0] != "i1" {
		t.Fatalf("expected [i1], got %v", deps)
	}

	dependents, err := s.ListDependents(ctx, "i1")
	if err != nil {
		t.Fatalf("list dependents: %v", err)
	}
	if len(dependents) != 1 || dependents[0] != "i2" {
		t.Fatalf("expected [i2], got %v", dependents)
	}

	if err := s.RemoveDependency(ctx, "i2", "i1"); err != nil {
		t.Fatalf("remove dependency: %v", err)
	}

	depsAfter, err := s.ListDependencies(ctx, "i2")
	if err != nil {
		t.Fatalf("list deps after remove: %v", err)
	}
	if len(depsAfter) != 0 {
		t.Fatalf("expected empty deps, got %v", depsAfter)
	}
}

func TestStorageDependencyAutoBlockUnblock(t *testing.T) {
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
		ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add i1: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "i2", PlanID: "p1", Title: "B", Status: ItemBlocked, Position: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add i2: %v", err)
	}
	if err := s.AddDependency(ctx, "i2", "i1"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	if err := s.SetItemStatus(ctx, "i1", ItemDone); err != nil {
		t.Fatalf("set i1 done: %v", err)
	}

	status, err := s.ItemStatus(ctx, "i2")
	if err != nil {
		t.Fatalf("get i2 status: %v", err)
	}
	if status != ItemBlocked {
		t.Errorf("expected i2 still blocked (service handles unblock), got %q", status)
	}
}

func TestStorageShiftPositions(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	for i, title := range []string{"A", "B", "C"} {
		if err := s.AddItem(ctx, Item{
			ID: "item-" + title, PlanID: "p1", Title: title,
			Status: ItemPending, Position: i,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("add %s: %v", title, err)
		}
	}

	if err := s.ShiftPositions(ctx, "p1", 1); err != nil {
		t.Fatalf("shift: %v", err)
	}

	plan, err := s.GetPlan(ctx, "p1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	positions := map[string]int{}
	for _, item := range plan.Items {
		positions[item.Title] = item.Position
	}
	if positions["A"] != 0 {
		t.Errorf("A: expected pos 0, got %d", positions["A"])
	}
	if positions["B"] != 2 {
		t.Errorf("B: expected pos 2 (shifted), got %d", positions["B"])
	}
	if positions["C"] != 3 {
		t.Errorf("C: expected pos 3 (shifted), got %d", positions["C"])
	}
}

func TestStorageDependenciesLoadedInItems(t *testing.T) {
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
		ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add i1: %v", err)
	}
	if err := s.AddItem(ctx, Item{
		ID: "i2", PlanID: "p1", Title: "B", Status: ItemBlocked, Position: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add i2: %v", err)
	}
	if err := s.AddDependency(ctx, "i2", "i1"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	plan, err := s.GetPlan(ctx, "p1")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if len(plan.Items[0].DependsOn) != 0 {
		t.Errorf("i1 should have no deps, got %v", plan.Items[0].DependsOn)
	}
	if len(plan.Items[1].DependsOn) != 1 || plan.Items[1].DependsOn[0] != "i1" {
		t.Errorf("i2 should depend on i1, got %v", plan.Items[1].DependsOn)
	}

	item, err := s.GetItem(ctx, "p1", "i2")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if len(item.DependsOn) != 1 || item.DependsOn[0] != "i1" {
		t.Errorf("getitem i2 should depend on i1, got %v", item.DependsOn)
	}
}

func TestStorageAllItemsDone(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreatePlan(ctx, Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	done, err := s.AllItemsDone(ctx, "p1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected false for plan with no items")
	}

	if err := s.AddItem(ctx, Item{
		ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("add i1: %v", err)
	}

	done, err = s.AllItemsDone(ctx, "p1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected false when not all items done")
	}

	if err := s.SetItemStatus(ctx, "i1", ItemDone); err != nil {
		t.Fatalf("set i1 done: %v", err)
	}

	done, err = s.AllItemsDone(ctx, "p1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected true when all items done")
	}
}
