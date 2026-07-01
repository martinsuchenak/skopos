package mcp

import (
	"context"
	"testing"

	"github.com/martinsuchenak/skopos/internal/plans"
)

func TestPlanReadWithItemID(t *testing.T) {
	ctx := context.Background()
	_, _, plansSvc := testSnapshotServices(t)

	plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{Name: "P", AuthorAgentID: "a"})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	itemA, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "First"})
	if err != nil {
		t.Fatalf("add A: %v", err)
	}
	_, err = plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "Second", DependsOn: []string{itemA.ID}})
	if err != nil {
		t.Fatalf("add B: %v", err)
	}

	// Simulate what the plan_read handler does with item_id:
	// GetPlan + find by ID.
	gotPlan, err := plansSvc.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}

	// Find item A — should be pending.
	var found *plans.Item
	for _, it := range gotPlan.Items {
		if it.ID == itemA.ID {
			found = &it
			break
		}
	}
	if found == nil {
		t.Fatal("item A not found in plan")
	}
	if found.Status != plans.ItemPending {
		t.Errorf("expected A pending, got %s", found.Status)
	}

	// Find item B — should be blocked (depends on A which is not done).
	for _, it := range gotPlan.Items {
		if it.Title == "Second" {
			if it.Status != plans.ItemBlocked {
				t.Errorf("expected B blocked, got %s", it.Status)
			}
			break
		}
	}

	// Complete A → B auto-unblocks → single-item check reflects it.
	if _, err := plansSvc.UpdateItem(ctx, plan.ID, itemA.ID, plans.UpdateItemInput{Status: plans.ItemDone}); err != nil {
		t.Fatalf("done A: %v", err)
	}
	gotPlan2, _ := plansSvc.GetPlan(ctx, plan.ID)
	for _, it := range gotPlan2.Items {
		if it.Title == "Second" {
			if it.Status != plans.ItemPending {
				t.Errorf("expected B pending after A done, got %s", it.Status)
			}
			break
		}
	}
}
