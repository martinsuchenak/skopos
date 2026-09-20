package inbox

import (
	"context"
	"database/sql"
	"testing"

	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/plans"
	_ "modernc.org/sqlite"
)

// couplingTestDB builds one SQLite database shared by the plans and inbox
// domains — the same topology serve.go wires.
func couplingTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlDB
}

// TestPlanCompletionCompletesConvertedItems wires the plans completion hook
// to the inbox exactly like serve.go and proves both completion paths:
// finishing the last item (auto-complete) and an explicit status PATCH.
func TestPlanCompletionCompletesConvertedItems(t *testing.T) {
	sqlDB := couplingTestDB(t)
	plansSvc := plans.NewService(plans.NewStorage(sqlDB))
	inboxSvc := NewService(NewStorage(sqlDB))
	rec := &recorder{}
	inboxSvc.SetPublisher(rec)
	plansSvc.SetCompletionHook(func(planID string) {
		_, _ = inboxSvc.CompleteForPlan(context.Background(), planID)
	})
	ctx := context.Background()

	plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{Name: "P", AuthorAgentID: "a", WorkspaceID: "ws-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "only item"}); err != nil {
		t.Fatal(err)
	}

	item, err := inboxSvc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", Title: "idea", AuthorAgentID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inboxSvc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := inboxSvc.Convert(ctx, item.ID, ConvertInput{PlanID: plan.ID}); err != nil {
		t.Fatal(err)
	}

	// A second converted item on another plan stays converted until ITS plan
	// completes — CompleteForPlan must not leak across plans.
	otherPlan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{Name: "Other", AuthorAgentID: "a", WorkspaceID: "ws-a"})
	if err != nil {
		t.Fatal(err)
	}
	otherItem, err := inboxSvc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", Title: "other idea", AuthorAgentID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inboxSvc.Convert(ctx, otherItem.ID, ConvertInput{PlanID: otherPlan.ID}); err != nil {
		t.Fatal(err)
	}

	// Finishing the plan's only item auto-completes the plan, which must
	// complete the linked inbox item (and publish an inbox event).
	detail, err := plansSvc.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Items) != 1 {
		t.Fatalf("expected one item, got %d", len(detail.Items))
	}
	if _, err := plansSvc.UpdateItem(ctx, plan.ID, detail.Items[0].ID, plans.UpdateItemInput{Status: plans.ItemDone}); err != nil {
		t.Fatal(err)
	}

	got, err := inboxSvc.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone {
		t.Fatalf("converted item should be done after plan completion, got %s", got.Status)
	}

	other, err := inboxSvc.GetItem(ctx, otherItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != StatusConverted {
		t.Fatalf("other plan's item must stay converted, got %s", other.Status)
	}

	// The completion published an inbox event attributed to the workspace.
	found := false
	for _, e := range rec.events {
		if e.Type == events.TypeInbox && e.Workspace == "ws-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an attributed inbox event after completion, got %+v", rec.events)
	}

	// Explicit completion path: PATCH the other plan to completed.
	if err := plansSvc.UpdatePlan(ctx, otherPlan.ID, plans.UpdatePlanInput{Status: plans.PlanCompleted}); err != nil {
		t.Fatal(err)
	}
	other, err = inboxSvc.GetItem(ctx, otherItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != StatusDone {
		t.Fatalf("explicit completion should also flip the item, got %s", other.Status)
	}
}
