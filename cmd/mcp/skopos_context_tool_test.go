package mcp

import (
	"context"
	"database/sql"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	_ "modernc.org/sqlite"
)

func testSnapshotServices(t *testing.T) (*status.Service, *blackboard.Service, *plans.Service) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB))
}

func TestBuildSnapshot(t *testing.T) {
	ctx := context.Background()
	statusSvc, bbSvc, plansSvc := testSnapshotServices(t)

	if _, err := statusSvc.Report(ctx, status.ReportInput{
		AgentID: "a1", AgentType: "codex", Workspace: "/repo", Status: status.StatusRunning,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if _, err := bbSvc.Write(ctx, blackboard.WriteInput{
		Scope: blackboard.ScopeBranch, BranchName: "feat",
		EntryType: blackboard.TypeFinding, Title: "found", AuthorAgentID: "a1",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{Name: "P", AuthorAgentID: "a1", BranchName: "feat"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	a, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "A"})
	if err != nil {
		t.Fatalf("add A: %v", err)
	}
	if _, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "B", DependsOn: []string{a.ID}}); err != nil {
		t.Fatalf("add B: %v", err)
	}

	snap := buildSnapshot(ctx, statusSvc, bbSvc, plansSvc, "feat", "", "")

	if snap["branch"] != "feat" {
		t.Errorf("branch = %v", snap["branch"])
	}

	bb, ok := snap["blackboard"].(map[string]any)
	if !ok {
		t.Fatalf("blackboard section shape: %T", snap["blackboard"])
	}
	if bb["total"].(int) < 1 {
		t.Errorf("expected blackboard total >= 1, got %v", bb["total"])
	}

	plansList, ok := snap["plans"].([]map[string]any)
	if !ok || len(plansList) != 1 {
		t.Fatalf("expected 1 active plan, got %v", snap["plans"])
	}
	p := plansList[0]
	if p["pending"].(int) != 1 {
		t.Errorf("expected pending 1, got %v", p["pending"])
	}
	if p["blocked"].(int) != 1 {
		t.Errorf("expected blocked 1, got %v", p["blocked"])
	}
	if len(p["blocked_items"].([]string)) != 1 {
		t.Errorf("expected 1 blocked item title, got %v", p["blocked_items"])
	}
	if p["next_ready"] != "A (#0)" {
		t.Errorf("expected next_ready 'A (#0)', got %v", p["next_ready"])
	}

	sessions, ok := snap["sessions"].([]map[string]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("expected 1 in-flight session, got %v", snap["sessions"])
	}
}

func TestBuildSnapshotNextReadyEmptyWhenAllClaimed(t *testing.T) {
	ctx := context.Background()
	statusSvc, bbSvc, plansSvc := testSnapshotServices(t)

	plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{Name: "P", AuthorAgentID: "a"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	claimed := "agent-x"
	_, err = plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "Taken"})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}
	// Claim the item so next_ready should skip it
	if _, err := plansSvc.UpdateItem(ctx, plan.ID, "fake-id", plans.UpdateItemInput{ClaimedByAgentID: &claimed}); err != nil {
		// UpdateItem with fake id will 404 — we need the real id. Get the plan.
	}
	gotPlan, err := plansSvc.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	itemID := gotPlan.Items[0].ID
	if _, err := plansSvc.UpdateItem(ctx, plan.ID, itemID, plans.UpdateItemInput{ClaimedByAgentID: &claimed}); err != nil {
		t.Fatalf("claim item: %v", err)
	}

	snap := buildSnapshot(ctx, statusSvc, bbSvc, plansSvc, "", "", "")
	plansList := snap["plans"].([]map[string]any)
	if plansList[0]["next_ready"] != "" {
		t.Errorf("expected empty next_ready when only item is claimed, got %v", plansList[0]["next_ready"])
	}
}

func TestContextToolRegistered(t *testing.T) {
	if len(contextToolRegistrations) == 0 {
		t.Fatal("contextToolRegistrations should not be empty")
	}
}
