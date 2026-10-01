package inbox

import (
	"context"
	"errors"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
)

// The frozen tag→status table, applied once, idempotently, with audit
// backfill (docs/design/agent-pipeline.md §1 "Moving the trial over").
func TestMigrateWorkflow(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()

	mustTagged := func(title string, tags ...string) *Item {
		t.Helper()
		item, err := svc.CreateItem(root, CreateInput{WorkspaceID: "ws-a", Title: title, AuthorAgentID: "trial"})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.UpdateItem(root, item.ID, UpdateInput{Tags: &tags}); err != nil {
			t.Fatal(err)
		}
		return item
	}

	ready := mustTagged("a", "ready")
	review := mustTagged("b", "plan-review")
	waiting := mustTagged("c", "approved", "agent-waiting-quota")
	plain := mustCreate(t, svc, root, "no tags")

	// Unscoped migration is refused (review fix 7).
	if _, err := svc.MigrateWorkflow(root, MigrateInput{DryRun: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unscoped migration must refuse: %v", err)
	}
	// Dry run: reports, writes nothing.
	report, err := svc.MigrateWorkflow(root, MigrateInput{WorkspaceID: "ws-a", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Migrated) != 3 {
		t.Fatalf("dry run should report 3 migrations, got %d", len(report.Migrated))
	}
	if got, _ := svc.GetItem(root, ready.ID); got.Status != StatusOpen {
		t.Fatal("dry run must not write")
	}

	// Scoped keys cannot migrate.
	if _, err := svc.MigrateWorkflow(scopedCtx(), MigrateInput{WorkspaceID: "ws-a"}); err == nil {
		t.Fatal("scoped key must not migrate")
	}

	// Apply.
	report, err = svc.MigrateWorkflow(root, MigrateInput{WorkspaceID: "ws-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Migrated) != 3 || len(report.WaitingQuota) != 1 {
		t.Fatalf("report wrong: %+v", report)
	}
	checks := map[string]Status{
		ready.ID:   StatusQueued,
		review.ID:  StatusAwaitingApproval,
		waiting.ID: StatusApproved,
	}
	for id, want := range checks {
		got, err := svc.GetItem(root, id)
		if err != nil || got.Status != want {
			t.Fatalf("item %s: %s (want %s), err %v", id, got.Status, want, err)
		}
	}
	if got, _ := svc.GetItem(root, plain.ID); got.Status != StatusOpen {
		t.Fatal("untagged item must be untouched")
	}

	// Audit backfill: one migration entry per moved item.
	timeline, err := svc.Timeline(root, ready.ID)
	if err != nil || len(timeline) != 1 {
		t.Fatalf("timeline after migration: %+v err %v", timeline, err)
	}
	if timeline[0].Action != "inbox.migrate" || timeline[0].Via != audit.ViaMigration || timeline[0].Actor != "migration" {
		t.Fatalf("migration entry wrong: %+v", timeline[0])
	}

	// Second run is a no-op: trial tags were stripped, so nothing matches
	// (not even the discarded backfill — the original report had one).
	report, err = svc.MigrateWorkflow(root, MigrateInput{WorkspaceID: "ws-a"})
	if err != nil || len(report.Migrated) != 0 {
		t.Fatalf("second run must be a no-op: %+v err %v", report, err)
	}
}

// Discarded trial items keep their status and get audit history backfilled.
func TestMigrateWorkflowDiscarded(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "rejected")
	tags := []string{"plan-review"}
	if err := svc.UpdateItem(root, item.ID, UpdateInput{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Discard(root, item.ID); err != nil {
		t.Fatal(err)
	}
	report, err := svc.MigrateWorkflow(root, MigrateInput{WorkspaceID: "ws-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Migrated) != 1 || report.Migrated[0].To != StatusDiscarded {
		t.Fatalf("discarded mapping wrong: %+v", report)
	}
	timeline, err := svc.Timeline(root, item.ID)
	if err != nil || len(timeline) != 1 || timeline[0].Via != audit.ViaMigration {
		t.Fatalf("discarded backfill: %+v err %v", timeline, err)
	}
}
