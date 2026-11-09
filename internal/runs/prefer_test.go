package runs

import (
	"testing"
	"time"
)

// A run queued again for an item (Retry after a failure) waits for the
// worker that ran its stage before, while that worker is online, for up to
// preferGrace; then, or once it goes offline, any worker takes it.
func TestRequeuedRunPrefersItsLastWorker(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	claim := func(worker string) *Run {
		t.Helper()
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	fail := func(r *Run, worker string) {
		t.Helper()
		if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: worker, Status: StatusFailed, Reason: "no result after 1h30m0s"}, r.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}

	first := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if first.PreferWorker != "" {
		t.Fatalf("a first run prefers nobody: %+v", first)
	}
	r := claim("w1")
	fail(r, "w1")
	if claim("w2") != nil { // w2 polls once, so it is a known worker
		t.Fatal("nothing queued yet")
	}

	again := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if again.PreferWorker != "w1" || !again.PreferUntil.Equal(now.Add(preferGrace)) {
		t.Fatalf("the retry prefers w1: %+v", again)
	}
	if r := claim("w2"); r != nil {
		t.Fatalf("w1 is online: w2 must wait, got %+v", r)
	}
	r = claim("w1")
	if r == nil || r.ID != again.ID {
		t.Fatalf("w1 takes its retry: %+v", r)
	}
	fail(r, "w1")

	// w1 still online (polling for other work) but past the grace: anyone.
	third := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	now = now.Add(preferGrace + time.Second)
	if r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"other"}}); err != nil || r != nil {
		t.Fatalf("w1 polls another workspace: %+v %v", r, err)
	}
	r = claim("w2")
	if r == nil || r.ID != third.ID {
		t.Fatalf("past the grace any worker takes it: %+v", r)
	}
	fail(r, "w2")

	// The preferred worker said goodbye: no waiting.
	fourth := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if fourth.PreferWorker != "w2" {
		t.Fatalf("prefers the newest run's worker: %+v", fourth)
	}
	if err := s.WorkerOffline(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if r := claim("w1"); r == nil || r.ID != fourth.ID {
		t.Fatalf("w2 is offline: w1 takes it: %+v", r)
	}
}

// Only implement and branch_review prefer a worker, and only the newest
// run's when it was a single run on the same runner.
func TestPreferenceScope(t *testing.T) {
	existing := []Run{{Stage: StageImplement, Runner: "claude", WorkerID: "w1"}, {Stage: StageReview, Runner: "claude", WorkerID: "w2"}}
	if got := lastWorkerFor(existing, StageImplement, "codex"); got != "" {
		t.Fatalf("another runner: %q", got)
	}
	if got := lastWorkerFor(existing, StageImplement, "claude"); got != "w1" {
		t.Fatalf("same runner: %q", got)
	}
	if got := lastWorkerFor(existing, StageReview, "claude"); got != "" {
		t.Fatalf("a review gains nothing from the clone: %q", got)
	}
	panel := []Run{{Stage: StageBranchReview, Runner: "claude", WorkerID: "w3", GroupID: "g"}, {Stage: StageBranchReview, Runner: "claude", WorkerID: "w4"}}
	if got := lastWorkerFor(panel, StageBranchReview, "claude"); got != "" {
		t.Fatalf("the newest run was a panel member: %q", got)
	}
}

// The preferred worker reports the workspace unhealthy (its claims skip
// it): nobody waits for it.
func TestNoWaitForUnhealthyPreferredWorker(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}})
	if err != nil || r == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "x"}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	again := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if again.PreferWorker != "w1" {
		t.Fatalf("prefers w1: %+v", again)
	}
	if r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Unhealthy: map[string]string{"ws": "preflight failed"}}); err != nil || r != nil {
		t.Fatalf("w1 is unhealthy for ws: %+v %v", r, err)
	}
	if r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws"}}); err != nil || r == nil || r.ID != again.ID {
		t.Fatalf("w2 takes it: %+v %v", r, err)
	}
}
