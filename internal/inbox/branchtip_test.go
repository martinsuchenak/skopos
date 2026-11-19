package inbox

import (
	"context"
	"errors"
	"testing"
)

// adoptQueuer is a run queue that also adopts commits.
type adoptQueuer struct {
	stubQueuer
	head    string
	adopted []string // "from|prefer"
}

func (q *adoptQueuer) QueueAdopt(_ context.Context, _, _, _, _, _, from, prefer string) (string, error) {
	q.adopted = append(q.adopted, from+"|"+prefer)
	return "run-adopt", nil
}

func (q *adoptQueuer) ImplementedHead(context.Context, string) (string, error) { return q.head, nil }

// The worker's report of commits after the tested head rides on the item;
// the approver's check queues an adopt run, moves the item back to
// implementing and clears the report.
func TestBranchTipAndCheckCommits(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "tip")
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Status{StatusImplementing, StatusInReview} {
		if _, err := svc.SystemTransition(root, item.ID, to, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	q := &adoptQueuer{head: "aaa"}
	svc.SetRunQueuer(q)
	if _, err := svc.CheckCommits(approverCtx(), item.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nothing reported, nothing to check: %v", err)
	}
	if _, err := svc.RecordBranchTip(root, item.ID, BranchTipInput{Head: "aaa", SHA: "bbb", Local: true, Worker: "w1",
		Commits: []TipCommit{{SHA: "bbb", Author: "Dev", Subject: "fix the copy"}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if tip := got.BranchTip; tip == nil || tip.SHA != "bbb" || tip.Head != "aaa" || !tip.Local || len(tip.Commits) != 1 || tip.Commits[0].Author != "Dev" {
		t.Fatalf("the item carries the tip: %+v", got.BranchTip)
	}
	if _, err := svc.CheckCommits(scopedCtx(), item.ID, ""); err == nil {
		t.Fatal("only an approver checks the new commits")
	}
	out, err := svc.CheckCommits(approverCtx(), item.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusImplementing || len(q.adopted) != 1 || q.adopted[0] != "aaa|w1" {
		t.Fatalf("an adopt run from the tested head, on the clone's worker: %s %v", out.Status, q.adopted)
	}
	if got, _ := svc.GetItem(root, item.ID); got.BranchTip != nil {
		t.Fatalf("the report is cleared: %+v", got.BranchTip)
	}
	// A tip equal to the head clears it; a stale report (the head moved
	// since) is refused.
	for _, to := range []Status{StatusInReview} {
		if _, err := svc.SystemTransition(root, item.ID, to, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = svc.RecordBranchTip(root, item.ID, BranchTipInput{Head: "aaa", SHA: "ccc"})
	q.head = "ccc"
	if _, err := svc.CheckCommits(approverCtx(), item.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a report about an older head is refused: %v", err)
	}
	if _, err := svc.RecordBranchTip(root, item.ID, BranchTipInput{Head: "ccc", SHA: "ccc"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.BranchTip != nil {
		t.Fatalf("a tip at the head clears the report: %+v", got.BranchTip)
	}
}
