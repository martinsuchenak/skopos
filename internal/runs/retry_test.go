package runs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A retryable failure (the worker's problem) goes back to the queue with a
// back-off instead of blocking the item; from the second retry on the
// failing worker is kept off it so another one gets the first go; once the
// retries are spent the failure blocks as before. A plain failure blocks
// at once.
func TestRetryableFailureRequeues(t *testing.T) {
	var moved []string
	s := testService(t)
	s.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		moved = append(moved, itemID+":"+to+":"+reason)
		return nil
	})
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.SetLease(time.Hour)
	ctx := rootCtx()
	claim := func(worker string) *Run {
		t.Helper()
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}})
		if err != nil {
			t.Fatal(err)
		}
		moved = nil // the claim's phase move is not under test
		return r
	}
	cost := 1.5
	fail := func(r *Run, worker string) *Run {
		t.Helper()
		got, err := s.Report(ctx, r.ID, ReportInput{WorkerID: worker, Status: StatusFailed, Reason: "setup failed: no output for 10m", Retryable: true,
			CostUSD: &cost, Checks: []Check{{Name: "make test", Stopped: true}}}, r.LeaseToken)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)

	r := claim("w1")
	got := fail(r, "w1")
	if got.Status != StatusQueued || got.Retries != 1 || got.WorkerID != "" || got.AvoidWorker != "" ||
		!got.RetryAfter.Equal(now.Add(time.Minute)) || !strings.Contains(got.FailReason, "attempt 1 of 3 failed on w1") {
		t.Fatalf("first retry: %+v", got)
	}
	if len(moved) != 0 {
		t.Fatalf("a retry must not move the item: %v", moved)
	}
	if r := claim("w1"); r != nil {
		t.Fatalf("nothing is claimable before the back-off: %+v", r)
	}
	now = now.Add(time.Minute)
	r = claim("w1") // after the first failure the same worker may retry
	if r == nil || r.ID != got.ID || r.Attempt != 2 {
		t.Fatalf("the back-off passed, w1 may retry: %+v", r)
	}
	got = fail(r, "w1")
	if got.Retries != 2 || got.AvoidWorker != "w1" || !got.RetryAfter.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("second retry: %+v", got)
	}
	now = now.Add(5 * time.Minute)
	if r := claim("w1"); r != nil {
		t.Fatalf("the failing worker is kept off during the avoid window: %+v", r)
	}
	if r := claim("w2"); r == nil || r.ID != got.ID {
		t.Fatalf("another worker gets the first go: %+v", r)
	} else {
		got = fail(r, "w2")
	}
	if got.Status != StatusFailed || !strings.Contains(got.FailReason, "failed after 3 attempts") {
		t.Fatalf("spent retries must fail the run: %+v", got)
	}
	if got.CostUSD == nil || *got.CostUSD != 4.5 {
		t.Fatalf("every attempt's cost adds up: %v", got.CostUSD)
	}
	if len(got.Checks) != 1 || !got.Checks[0].Stopped {
		t.Fatalf("a stopped check stays marked: %+v", got.Checks)
	}
	assertMoved(t, moved, "item-1:blocked:run failed: failed after 3 attempts: setup failed: no output for 10m")

	// The avoid window ends: a lone worker gets its run back.
	moved = nil
	mustQueue(t, s, ctx, "ws", "item-2", StageImplement)
	fail(claim("w1"), "w1")
	now = now.Add(time.Minute)
	fail(claim("w1"), "w1")
	now = now.Add(5*time.Minute + avoidWindow)
	if r := claim("w1"); r == nil || r.ItemID != "item-2" {
		t.Fatalf("after the avoid window the failing worker may claim again: %+v", r)
	}

	// Not retryable: blocks at once.
	moved = nil
	mustQueue(t, s, ctx, "ws", "item-3", StageImplement)
	s.now = func() time.Time { return now.Add(time.Hour) }
	r = claim("w3")
	moved = nil
	if got, _ := s.Report(ctx, r.ID, ReportInput{WorkerID: "w3", Status: StatusFailed, Reason: "checks failed: tests"}, r.LeaseToken); got.Status != StatusFailed {
		t.Fatalf("a plain failure fails the run: %+v", got)
	}
	assertMoved(t, moved, "item-3:blocked:run failed: checks failed: tests")
}
