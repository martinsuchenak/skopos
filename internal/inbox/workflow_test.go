package inbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/plans"
	_ "modernc.org/sqlite"
)

func testServiceWithAudit(t *testing.T) (*Service, *audit.Service) {
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
	svc := NewService(NewStorage(sqlDB))
	svc.SetAuditRecorder(auditSvc)
	return svc, auditSvc
}

func approverCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k-approver", Name: "relay", Approver: true,
		Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

// The full happy path from the design's end-to-end flow: queue → plan →
// approve → implement → review → done, with every transition audited.
// mustReadyForApproval walks an item to awaiting_approval with a linked
// plan and a snapshot revision — the precondition Approve now enforces.
func mustReadyForApproval(t *testing.T, svc *Service, plansSvc *plans.Service, title string) *Item {
	t.Helper()
	root := context.Background()
	item := mustCreate(t, svc, root, title)
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	plan, err := plansSvc.CreatePlan(root, plans.CreatePlanInput{WorkspaceID: "ws-a", Name: "p-" + title, AuthorAgentID: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(root, plan.ID, plans.CreateItemInput{Title: "step 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LinkPlan(root, item.ID, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.CreateRevision(root, plan.ID, "d2ce73dfdd1"); err != nil {
		t.Fatal(err)
	}
	gated, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return gated
}

func TestWorkflowHappyPath(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "workflow item")

	steps := []struct {
		name string
		run  func() error
		want Status
	}{
		{"approve", func() error {
			_, err := svc.Approve(approverCtx(), item.ID, ApproveInput{Notes: "wr=108900", Via: audit.ViaSlack})
			return err
		}, StatusApproved},
		{"implement", func() error {
			_, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "agent-1", audit.ViaWorker)
			return err
		}, StatusImplementing},
		{"in review", func() error {
			_, err := svc.SystemTransition(root, item.ID, StatusInReview, "", "agent-1", audit.ViaWorker)
			return err
		}, StatusInReview},
		{"done", func() error { _, err := svc.MarkDone(approverCtx(), item.ID, "abc123", audit.ViaSlack); return err }, StatusDone},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got, err := svc.GetItem(root, item.ID)
		if err != nil || got.Status != step.want {
			t.Fatalf("%s: status %s (want %s), err %v", step.name, got.Status, step.want, err)
		}
	}

	// The timeline carries every transition, newest-first, with actors
	// (helper-walk transitions included).
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < len(steps) {
		t.Fatalf("timeline must have one entry per transition: %d", len(entries))
	}
	if entries[0].Action != "inbox.mark_done" || entries[0].Actor != "key relay" || entries[0].Via != "slack" {
		t.Fatalf("newest entry wrong: %+v", entries[0])
	}
	if entries[1].Actor != "agent agent-1" || entries[1].Via != "worker" {
		t.Fatalf("system entries must record the agent and via: %+v", entries[1])
	}
	// Approve notes (overrides) are part of the record.
	found := false
	for _, e := range entries {
		if e.Action == "inbox.approve" && e.Notes == "wr=108900" {
			found = true
		}
	}
	if !found {
		t.Fatal("approve entry must carry the override notes")
	}
}

// The five human actions (plus retry) are approver-gated: a plain scoped key
// is rejected; an approver key passes. Root passes implicitly.
func TestWorkflowApproverGating(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	item := mustReadyForApproval(t, svc, plansSvc, "gated")

	calls := map[string]func(context.Context, string) error{
		"queue": func(ctx context.Context, id string) error { _, err := svc.Queue(ctx, id, ""); return err },
		"approve": func(ctx context.Context, id string) error {
			_, err := svc.Approve(ctx, id, ApproveInput{Notes: "", Via: ""})
			return err
		},
		"request_changes": func(ctx context.Context, id string) error {
			_, err := svc.RequestChanges(ctx, id, "notes", "")
			return err
		},
		"reject":    func(ctx context.Context, id string) error { _, err := svc.Reject(ctx, id, "", ""); return err },
		"mark_done": func(ctx context.Context, id string) error { _, err := svc.MarkDone(ctx, id, "abc", ""); return err },
		"retry":     func(ctx context.Context, id string) error { _, err := svc.Retry(ctx, id, "answer", ""); return err },
	}
	for name, call := range calls {
		if err := call(scopedCtx(), item.ID); !errors.Is(err, auth.ErrApproverRequired) {
			t.Errorf("%s by plain scoped key: expected ErrApproverRequired, got %v", name, err)
		}
	}
	// The approver key performs a legal action successfully.
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatalf("approve by approver key: %v", err)
	}
}

// Illegal moves are rejected with the matrix's verdict: wrong actor class or
// no edge at all.
func TestWorkflowIllegalTransitions(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "matrix")

	// Human actions on non-workflow states have no edge.
	if _, err := svc.Approve(root, item.ID, ApproveInput{Notes: "", Via: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("approve from open: %v", err)
	}
	// System cannot perform human edges.
	if _, err := svc.SystemTransition(root, item.ID, StatusQueued, "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("system queue: %v", err)
	}
	// Human cannot perform system edges.
	if _, err := svc.Queue(root, item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(root, item.ID, ApproveInput{Notes: "", Via: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("human planning: %v", err)
	}
	// Skips are illegal: queued → implementing has no edge.
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("skip to implementing: %v", err)
	}

	// Amendment loop: link a plan + revision, approve, then amend.
	plan2, err := plansSvc.CreatePlan(root, plans.CreatePlanInput{WorkspaceID: "ws-a", Name: "p-matrix", AuthorAgentID: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.AddItem(root, plan2.ID, plans.CreateItemInput{Title: "step 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LinkPlan(root, item.ID, plan2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := plansSvc.CreateRevision(root, plan2.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); err != nil {
		t.Fatalf("walk to planning: %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", ""); err != nil {
		t.Fatalf("walk to awaiting_approval: %v", err)
	}
	if _, err := svc.Approve(root, item.ID, ApproveInput{}); err != nil {
		t.Fatalf("approve (human): %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatalf("walk to implementing: %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "amendment", "", ""); err != nil {
		t.Fatalf("amendment: %v", err)
	}
	if _, err := svc.RequestChanges(root, item.ID, "tighten step 2", ""); err != nil {
		t.Fatalf("request changes after amendment: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Status != StatusPlanning {
		t.Fatalf("after request changes: %s", got.Status)
	}

	// Blocked → retry (human, with answer) → back to implementing.
	if _, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", ""); err != nil { // planning → awaiting_approval
		t.Fatalf("walk to awaiting_approval: %v", err)
	}
	if _, err := svc.Approve(root, item.ID, ApproveInput{Notes: "", Via: ""}); err != nil { // human
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatalf("walk to implementing: %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusBlocked, "one question", "", ""); err != nil {
		t.Fatalf("walk to blocked: %v", err)
	}
	if _, err := svc.Retry(approverCtx(), item.ID, "the answer", audit.ViaSlack); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// In-review request-changes returns the item to implementing.
	for _, to := range []Status{StatusInReview} {
		if _, err := svc.SystemTransition(root, item.ID, to, "", "", ""); err != nil {
			t.Fatalf("walk to %s: %v", to, err)
		}
	}
	if _, err := svc.RequestChanges(approverCtx(), item.ID, "rename the label", ""); err != nil {
		t.Fatalf("in-review request changes: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Status != StatusImplementing {
		t.Fatalf("after in-review request changes: %s", got.Status)
	}
}

// Foreign-workspace keys get the uniform not-found page — no existence
// oracle on workflow actions or the timeline.
func TestWorkflowScope(t *testing.T) {
	svc, _ := testServiceWithAudit(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "scoped")

	foreign := auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k2", Name: "foreign", Approver: true,
		Workspaces: map[string]struct{}{"ws-b": {}},
	})
	if _, err := svc.Queue(foreign, item.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign queue: %v", err)
	}
	if _, err := svc.Timeline(foreign, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign timeline: %v", err)
	}
	if _, err := svc.SystemTransition(foreign, item.ID, StatusPlanning, "", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign system transition: %v", err)
	}
	// In-scope non-approver still passes system transitions (the executor's
	// key is exactly this shape) but never the human actions.
	if _, err := svc.Queue(scopedCtx(), item.ID, ""); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("in-scope queue without approver: %v", err)
	}
}

// The list filter accepts the workflow phases and the aggregate chip — the
// executor polls status=queued every tick (found live: the 400 starved the
// worker of work while HasWork swallowed the error).
func TestListWorkflowStatusFilters(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "filter probe")
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"queued", "planning", "awaiting_approval", "approved",
		"implementing", "in_review", "failed", "blocked", "workflow"} {
		items, err := svc.ListItems(root, "ws-a", status, "", "")
		if err != nil {
			t.Fatalf("status=%s: %v", status, err)
		}
		if status == "queued" && len(items) != 1 {
			t.Fatalf("status=queued should list the item, got %d", len(items))
		}
	}
	if _, err := svc.ListItems(root, "ws-a", "bogus", "", ""); !errorsIsInvalidInput(err) {
		t.Fatalf("bogus status still refused: %v", err)
	}
}

func errorsIsInvalidInput(err error) bool {
	if err == nil {
		return false
	}
	type unwrapper interface{ Unwrap() error }
	return strings.Contains(err.Error(), "invalid inbox input")
}
