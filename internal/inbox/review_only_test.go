package inbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// stageQueuer records the stages and branches queued.
type stageQueuer struct {
	stubQueuer
	mu     sync.Mutex
	queued []string // "stage@branch"
}

func (q *stageQueuer) QueueStage(ctx context.Context, itemID, workspaceID, stage, planRevID, baseSHA, branch string) (string, error) {
	q.mu.Lock()
	q.queued = append(q.queued, stage+"@"+branch)
	q.mu.Unlock()
	return q.stubQueuer.QueueStage(ctx, itemID, workspaceID, stage, planRevID, baseSHA, branch)
}

func (q *stageQueuer) last() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queued) == 0 {
		return ""
	}
	return q.queued[len(q.queued)-1]
}

func mustCreateOnBranch(t *testing.T, svc *Service, title, branch, ref string) *Item {
	t.Helper()
	item, err := svc.CreateItem(context.Background(), CreateInput{
		WorkspaceID: "ws-a", Title: title, Content: "what the branch is for", AuthorAgentID: "author",
		Branch: branch, Ref: ref,
	})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return item
}

func TestBranchAndRefValidation(t *testing.T) {
	for _, ok := range []string{"", "WR203345-some-feature", "feature/x.y_z", "agent/7614a51fea45-csv"} {
		if _, err := NormalizeBranch(ok); err != nil {
			t.Errorf("NormalizeBranch(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"-x", "a..b", "a//b", "a/", "a.", "x.lock", "a/.hidden", "has space", "a~b", "a^b", "a:b", "@{x}", strings.Repeat("a", 121)} {
		if _, err := NormalizeBranch(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("NormalizeBranch(%q) = %v, want invalid", bad, err)
		}
	}
	for _, ok := range []string{"", "WR203345", "JIRA-12", "v1.2"} {
		if _, err := NormalizeRef(ok); err != nil {
			t.Errorf("NormalizeRef(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"WR/1", "-x", "a..b", "a b", strings.Repeat("a", 41)} {
		if _, err := NormalizeRef(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("NormalizeRef(%q) = %v, want invalid", bad, err)
		}
	}
}

// Branch and reference are editable on an open item; once the item is in
// the agent workflow, only an approver changes which branch agents push to.
func TestBranchEditRules(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "edit branch")
	b, r := "WR1-x", "WR1"
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Branch: &b, Ref: &r}); err != nil {
		t.Fatalf("open item: %v", err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if got.Branch != b || got.Ref != r {
		t.Fatalf("stored: %q %q", got.Branch, got.Ref)
	}
	bad := "a..b"
	if err := svc.UpdateItem(root, item.ID, UpdateInput{Branch: &bad}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid branch: %v", err)
	}
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	other := "WR1-y"
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Branch: &other}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("agent key on a workflow item: %v", err)
	}
	if err := svc.UpdateItem(approverCtx(), item.ID, UpdateInput{Branch: &other}); err != nil {
		t.Fatalf("approver: %v", err)
	}
	clear := ""
	if err := svc.UpdateItem(approverCtx(), item.ID, UpdateInput{Ref: &clear}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item.ID)
	if got.Branch != other || got.Ref != "" {
		t.Fatalf("after edits: %q %q", got.Branch, got.Ref)
	}
}

// The review-only path: Review queues a branch_review run; the claim moves
// the item into implementing (an edge only review-only items have); at
// in_review, request-changes plans a handover; a blocked review retries
// the review.
func TestReviewOnlyFlow(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	q := &stageQueuer{}
	svc.SetRunQueuer(q)

	none := mustCreate(t, svc, root, "no branch")
	if _, err := svc.ReviewBranch(approverCtx(), none.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("review without branch or ref: %v", err)
	}
	item := mustCreateOnBranch(t, svc, "review me", "WR9-feature", "")
	if _, err := svc.ReviewBranch(agentCtx(), item.ID, ""); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("agent key: %v", err)
	}
	got, err := svc.ReviewBranch(approverCtx(), item.ID, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.Flow != FlowReview || q.last() != StageBranchReview+"@WR9-feature" {
		t.Fatalf("after review: %s flow=%q queued=%q", got.Status, got.Flow, q.last())
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "run claimed", "worker:w", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A failed review run blocks; retry runs the review again.
	if _, err := svc.SystemTransition(root, item.ID, StatusBlocked, "branch gone", "worker:w", ""); err != nil {
		t.Fatal(err)
	}
	if got, err = svc.Retry(approverCtx(), item.ID, "pushed it again", ""); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusImplementing || q.last() != StageBranchReview+"@WR9-feature" {
		t.Fatalf("retry: %s queued=%q", got.Status, q.last())
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusInReview, "", "worker:w", ""); err != nil {
		t.Fatal(err)
	}
	// Changes on an unplanned branch: a handover, planned first.
	if got, err = svc.RequestChanges(approverCtx(), item.ID, "add the header row", ""); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPlanning || got.Flow != "" || q.last() != "plan@" {
		t.Fatalf("request changes: %s flow=%q queued=%q", got.Status, got.Flow, q.last())
	}
	full, _ := svc.GetItem(root, item.ID)
	if !strings.Contains(full.Content, "## Change requests\n\nadd the header row") {
		t.Fatalf("change requests not appended: %q", full.Content)
	}
}

// The review-only edges stay closed to every other item: an executor can't
// skip an ordinary item's plan gate, and an implemented item isn't
// re-planned from in_review.
func TestReviewOnlyEdgesClosedToOtherItems(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "ordinary")
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("queued → implementing on an ordinary item: %v", err)
	}
	// An implemented item at in_review goes back to implementing, never
	// to planning.
	ready := mustReadyForApproval(t, svc, plansSvc, "implemented")
	rev, _ := plansSvc.LatestRevision(root, ready.PlanID)
	if _, err := svc.Approve(approverCtx(), ready.ID, ApproveInput{RevisionID: rev.ID}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Status{StatusImplementing, StatusInReview} {
		if _, err := svc.SystemTransition(root, ready.ID, to, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.RequestChanges(approverCtx(), ready.ID, "fix it", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusImplementing {
		t.Fatalf("implemented item went to %s", got.Status)
	}
	// Queue starts a restored review-only item over as an ordinary one.
	ro := mustCreateOnBranch(t, svc, "was review only", "WR3-x", "")
	if _, err := svc.ReviewBranch(approverCtx(), ro.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reject(approverCtx(), ro.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Restore(root, ro.ID); err != nil {
		t.Fatal(err)
	}
	if restored, _ := svc.GetItem(root, ro.ID); restored.Flow != "" {
		t.Fatalf("restore kept flow %q", restored.Flow)
	}
	if got, err = svc.Queue(approverCtx(), ro.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got.Flow != "" {
		t.Fatalf("queue kept flow %q", got.Flow)
	}
}
