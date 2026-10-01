package inbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/plans"
	_ "modernc.org/sqlite"
)

// fullStack builds the wired workflow stack against one database: inbox +
// plans + approvals + audit, exactly as serve.go assembles them.
func fullStack(t *testing.T) (*Service, *plans.Service, *approvals.Service) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	auditSvc := audit.NewService(audit.NewStorage(sqlDB))
	approvalsSvc := approvals.NewService(approvals.NewStorage(sqlDB))
	plansSvc := plans.NewService(plans.NewStorage(sqlDB))
	svc := NewService(NewStorage(sqlDB))
	svc.SetAuditRecorder(auditSvc)
	svc.SetApprovalLog(approvalsSvc)
	svc.SetPlanRevisions(plansSvc)
	return svc, plansSvc, approvalsSvc
}

func mustPlan(t *testing.T, plansSvc *plans.Service, ctx context.Context, workspace string) string {
	t.Helper()
	plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{
		WorkspaceID: workspace, Name: "p", AuthorAgentID: "planner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "step 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(ctx, plan.ID, plans.CreateItemInput{Title: "step 2"}); err != nil {
		t.Fatal(err)
	}
	return plan.ID
}

// The approval gate end to end: snapshot a revision, approve, and the
// revision locks — structural plan edits are refused until an amendment
// creates a new revision.
func TestApproveLocksRevisionAndRecordsApproval(t *testing.T) {
	svc, plansSvc, approvalsSvc := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "gated plan")

	// Walk to awaiting_approval and attach the planner's plan.
	for _, fn := range []func() error{
		func() error { _, err := svc.Queue(approverCtx(), item.ID, ""); return err },
		func() error { _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); return err },
	} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	planID := mustPlan(t, plansSvc, root, "ws-a")
	if _, err := svc.LinkPlan(root, item.ID, planID); err != nil {
		t.Fatalf("link plan: %v", err)
	}
	rev, err := plansSvc.CreateRevision(root, planID, "d2ce73dfdd1")
	if err != nil {
		t.Fatal(err)
	}
	if rev.RevisionNo != 1 || len(rev.Steps) != 2 || rev.BaseSHA != "d2ce73dfdd1" {
		t.Fatalf("revision 1 wrong: %+v", rev)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", ""); err != nil {
		t.Fatal(err)
	}

	// Approve: the revision locks and the approval names it.
	if _, err := svc.Approve(approverCtx(), item.ID, "wr=108900", "slack"); err != nil {
		t.Fatal(err)
	}
	latest, err := plansSvc.LatestRevision(root, planID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.LockedAt == nil {
		t.Fatal("approve must lock the revision")
	}
	entries, err := approvalsSvc.List(root, item.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("approvals: %+v err %v", entries, err)
	}
	if entries[0].Gate != approvals.GatePlan || entries[0].Subject != rev.ID+"+d2ce73dfdd1" {
		t.Fatalf("approval subject wrong: %+v", entries[0])
	}
	if entries[0].Actor != "key relay" || entries[0].Via != "slack" || entries[0].Notes != "wr=108900" {
		t.Fatalf("approval metadata wrong: %+v", entries[0])
	}

	// Locked: structural edits refused, status updates still pass.
	if _, err := plansSvc.AddItem(root, planID, plans.CreateItemInput{Title: "sneak"}); !errors.Is(err, plans.ErrRevisionLocked) {
		t.Fatalf("add to locked: %v", err)
	}
	items := mustListPlanItems(t, plansSvc, planID)
	if _, err := plansSvc.UpdateItem(root, planID, items[0].ID, plans.UpdateItemInput{Status: plans.ItemDone}); err != nil {
		t.Fatalf("status update on locked revision must pass: %v", err)
	}

	// Amendment: a new snapshot supersedes the lock and edits flow again.
	rev2, err := plansSvc.CreateRevision(root, planID, "")
	if err != nil {
		t.Fatal(err)
	}
	if rev2.RevisionNo != 2 || rev2.LockedAt != nil {
		t.Fatalf("amendment revision wrong: %+v", rev2)
	}
	if _, err := plansSvc.AddItem(root, planID, plans.CreateItemInput{Title: "step 3"}); err != nil {
		t.Fatalf("add after amendment: %v", err)
	}
}

func mustListPlanItems(t *testing.T, plansSvc *plans.Service, planID string) []plans.Item {
	t.Helper()
	plan, err := plansSvc.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Items
}

// Hash stability: identical steps hash identically; any change differs.
func TestRevisionHash(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	planID := mustPlan(t, plansSvc, root, "ws-a")
	r1, err := plansSvc.CreateRevision(root, planID, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := plansSvc.CreateRevision(root, planID, "")
	if err != nil {
		t.Fatal(err)
	}
	if r1.ContentHash != r2.ContentHash {
		t.Fatal("identical steps must hash identically")
	}
	if _, err := plansSvc.AddItem(root, planID, plans.CreateItemInput{Title: "step 3"}); err != nil {
		t.Fatal(err)
	}
	r3, err := plansSvc.CreateRevision(root, planID, "")
	if err != nil {
		t.Fatal(err)
	}
	if r3.ContentHash == r1.ContentHash {
		t.Fatal("changed steps must hash differently")
	}
	_ = svc
}

// The review gate names the head commit; MarkDone records it.
func TestMarkDoneRecordsReviewApproval(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "review gate")

	steps := []func() error{
		func() error { _, err := svc.Queue(approverCtx(), item.ID, ""); return err },
		func() error { _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); return err },
		func() error {
			_, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", "")
			return err
		},
		func() error { _, err := svc.Approve(approverCtx(), item.ID, "", ""); return err },
		func() error {
			_, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", "")
			return err
		},
		func() error { _, err := svc.SystemTransition(root, item.ID, StatusInReview, "", "", ""); return err },
	}
	for _, fn := range steps {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.MarkDone(approverCtx(), item.ID, "abc123head", ""); err != nil {
		t.Fatal(err)
	}
	entries, err := svc.Approvals(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the review approval only (no plan linked): %+v", entries)
	}
	if entries[0].Gate != approvals.GateReview || entries[0].Subject != "abc123head" {
		t.Fatalf("review approval wrong: %+v", entries[0])
	}
}

// LinkPlan rules: workflow items only, same workspace, once.
func TestLinkPlanRules(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "link rules")

	// Open (non-workflow) items cannot link mid-flight.
	planID := mustPlan(t, plansSvc, root, "ws-a")
	if _, err := svc.LinkPlan(root, item.ID, planID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("link on open item: %v", err)
	}
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// Foreign-workspace plan is uniform not-found.
	foreignPlan := mustPlan(t, plansSvc, context.Background(), "ws-b")
	if _, err := svc.LinkPlan(scopedCtx(), item.ID, foreignPlan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign plan link: %v", err)
	}
	if _, err := svc.LinkPlan(root, item.ID, planID); err != nil {
		t.Fatal(err)
	}
	// Second link on the same item refuses.
	if _, err := svc.LinkPlan(root, item.ID, planID); !errors.Is(err, ErrAlreadyConverted) {
		t.Fatalf("second link: %v", err)
	}
}

// Old-base warning data: the revision's creation time is exposed so the
// approval surface can warn when the plan is stale (the frozen design's
// 3-day rule; the caller computes the age).
func TestRevisionExposesCreationTime(t *testing.T) {
	_, plansSvc, _ := fullStack(t)
	root := context.Background()
	planID := mustPlan(t, plansSvc, root, "ws-a")
	rev, err := plansSvc.CreateRevision(root, planID, "")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(rev.CreatedAt) > time.Minute {
		t.Fatalf("revision creation time wrong: %s", rev.CreatedAt)
	}
	revs, err := plansSvc.ListRevisions(root, planID)
	if err != nil || len(revs) != 1 || revs[0].ID != rev.ID {
		t.Fatalf("list revisions: %+v err %v", revs, err)
	}
}
