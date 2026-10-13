package runs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
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
	return NewStorage(sqlDB)
}

func testService(t *testing.T) *Service {
	t.Helper()
	s := NewService(testStorage(t))
	s.SetLease(50 * time.Millisecond) // lease expiry tests run fast
	return s
}

func rootCtx() context.Context { return context.Background() }

func scopedCtx(ws ...string) context.Context {
	set := map[string]struct{}{}
	for _, w := range ws {
		set[w] = struct{}{}
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Name: "worker-key", Workspaces: set})
}

// The claim matrix: scope, stage preference, and the honest 204.
func TestClaimMatching(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{
		"ws-a": {Stages: map[string]string{"implement": "claude", "review": "gemini"}},
	})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	mustQueue(t, s, ctx, "ws-b", "item-2", StagePlan)

	// Scope filter: ws-b only sees item-2.
	run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Runners: []string{"claude"}, Workspaces: []string{"ws-b"}})
	if err != nil || run == nil || run.ItemID != "item-2" {
		t.Fatalf("ws-b claim: %+v err %v", run, err)
	}
	// Stage preference: ws-a's implement wants claude; a gemini-only worker
	// gets nothing, a claude worker gets it.
	if run, err = s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{"gemini"}, Workspaces: []string{"ws-a"}}); err != nil || run != nil {
		t.Fatalf("gemini worker must not claim claude-preferred run: %+v err %v", run, err)
	}
	run, err = s.ClaimNext(ctx, ClaimInput{WorkerID: "w3", Runners: []string{"claude"}, Workspaces: []string{"ws-a"}})
	if err != nil || run == nil || run.ItemID != "item-1" {
		t.Fatalf("claude claim: %+v err %v", run, err)
	}
	if run.Status != StatusClaimed || run.WorkerID != "w3" || run.Attempt != 1 {
		t.Fatalf("claim state wrong: %+v", run)
	}
	// Queue drained: honest nil.
	if run, err = s.ClaimNext(ctx, ClaimInput{WorkerID: "w3", Runners: []string{"claude"}, Workspaces: []string{"ws-a"}}); err != nil || run != nil {
		t.Fatalf("drained queue must be nil: %+v err %v", run, err)
	}
}

// Affinity: an interrupted run re-claims to its owner; another worker takes
// it over only when branch-resumable.
func TestAffinity(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	r := mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Runners: []string{}, Workspaces: []string{"ws-a"}})
	if err != nil || run == nil || run.ID != r.ID {
		t.Fatalf("first claim: %+v err %v", run, err)
	}
	// Lease expires (no heartbeats): the next claim from ANOTHER worker
	// expires it to interrupted, but cannot take it (session affinity).
	time.Sleep(80 * time.Millisecond)
	run2, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{}, Workspaces: []string{"ws-a"}})
	if err != nil || run2 != nil {
		t.Fatalf("foreign worker must not take a session run: %+v err %v", run2, err)
	}
	// The owner re-claims it; attempt increments.
	run3, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Runners: []string{}, Workspaces: []string{"ws-a"}})
	if err != nil || run3 == nil || run3.ID != r.ID {
		t.Fatalf("owner re-claim: %+v err %v", run3, err)
	}
	if run3.Attempt != 2 {
		t.Fatalf("re-claim must increment attempt: %+v", run3)
	}
	// A branch-resumable interrupted run is claimable by anyone.
	if err := s.storage.Pause(ctx, r.ID, "w1", "quota", time.Time{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// flip to interrupted+branch directly (the takeover marking)
	if _, err := s.storage.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, resume_kind = ? WHERE id = ?`, string(StatusInterrupted), ResumeBranch, r.ID); err != nil {
		t.Fatal(err)
	}
	run4, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{}, Workspaces: []string{"ws-a"}})
	if err != nil || run4 == nil || run4.ID != r.ID {
		t.Fatalf("branch takeover: %+v err %v", run4, err)
	}
}

// Pause keeps affinity; resume is owner-only; heartbeats flip claimed ->
// running and extend the lease.
func TestPauseHeartbeatResume(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	run := mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)

	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err := s.Heartbeat(ctx, run.ID, "w1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Status != StatusRunning {
		t.Fatalf("heartbeat must flip to running: %s", got.Status)
	}
	// Wrong worker cannot heartbeat or resume.
	if err := s.Heartbeat(ctx, run.ID, "w2"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("foreign heartbeat: %v", err)
	}
	if err := s.Pause(ctx, run.ID, "w1", "quota", time.Time{}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.Resume(ctx, run.ID, "w2"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("foreign resume must refuse: %v", err)
	}
	if err := s.Resume(ctx, run.ID, "w1"); err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	got, _ = s.Get(ctx, run.ID)
	// N7: resume makes the run owner-claimable (interrupted+session), not
	// straight-claimed — a claimed run was invisible to Claim until its
	// lease expired.
	if got.Status != StatusInterrupted || got.ResumeKind != ResumeSession {
		t.Fatalf("resume must re-arm as interrupted/session: %s/%s", got.Status, got.ResumeKind)
	}
	// The owner's next claim picks it up immediately.
	again, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err != nil || again == nil || again.ID != run.ID {
		t.Fatalf("owner must re-claim after resume: %+v err %v", again, err)
	}
}

// Chaining: plan success moves the item to the gate; implement success queues
// review when the workspace prefers a reviewer, else moves to in_review;
// failure blocks the item with the reason. Item moves go through the
// transition callback — the workflow matrix stays the authority.
func TestChaining(t *testing.T) {
	var moved []string // "item:to:reason"
	s := testService(t)
	s.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		moved = append(moved, itemID+":"+to+":"+reason)
		return nil
	})
	s.SetPrefs(map[string]*WorkspacePrefs{
		"ws-a": {Stages: map[string]string{"review": "gemini"}}, // reviewer configured
		"ws-b": {},                                              // no reviewer: implement ends the sequence
	})
	ctx := rootCtx()

	// Plan success -> awaiting_approval.
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "plan ready"}); err != nil {
		t.Fatal(err)
	}
	assertMoved(t, moved, "item-1:awaiting_approval:")

	// Implement success with a reviewer configured -> review run queued (no
	// item move yet).
	_ = mustQueue(t, s, ctx, "ws-a", "item-2", StageImplement)
	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if run.Stage != StageImplement {
		t.Fatalf("expected implement run, got %s", run.Stage)
	}
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "done", HeadSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	// The review run exists, gemini-preferrred.
	rruns, _ := s.List(ctx, StatusQueued, "item-2")
	if len(rruns) != 1 || rruns[0].Stage != StageReview || rruns[0].Runner != "gemini" {
		t.Fatalf("review run not queued: %+v", rruns)
	}
	// Review success -> in_review (no qa preference configured).
	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w-gemini", Runners: []string{"gemini"}, Workspaces: []string{"ws-a"}})
	if run == nil || run.Stage != StageReview {
		t.Fatalf("gemini worker should claim review: %+v", run)
	}
	_, _ = s.Report(ctx, run.ID, ReportInput{WorkerID: "w-gemini", Status: StatusSucceeded, Summary: "looks good"})

	// Implement success with NO reviewer -> straight to in_review.
	_ = mustQueue(t, s, ctx, "ws-b", "item-3", StageImplement)
	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-b"}})
	_, _ = s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded})

	// Failure blocks the item with the reason.
	_ = mustQueue(t, s, ctx, "ws-b", "item-4", StageImplement)
	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-b"}})
	_, _ = s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "tests exploded\nand more"})

	assertMoved(t, moved, "item-2:in_review:", "item-3:in_review:", "item-4:blocked:run failed: tests exploded")
}

// H1: the item enters its phase on CLAIM, so a failure blocks from a valid
// edge even when the run was claimed from queued/approved.
func TestClaimMovesItemPhase(t *testing.T) {
	var moves []string
	s := testService(t)
	s.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		moves = append(moves, to)
		return nil
	})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if run == nil {
		t.Fatal("claim failed")
	}
	assertMoved(t, moves, "planning")
	// Failure now blocks from planning — a valid edge.
	moves = nil
	_, _ = s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "boom"})
	assertMoved(t, moves, "blocked")
}

// H4: paused runs are NEVER auto-claimed (the amendment gate); interrupted
// ones still return to their owner.
func TestPausedNotReclaimed(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err := s.Pause(ctx, run.ID, "w1", "amendment", time.Time{}); err != nil {
		t.Fatal(err)
	}
	// The same worker claims: nothing (paused is not a candidate).
	if r, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}}); r != nil {
		t.Fatalf("paused run must not be reclaimed: %+v", r)
	}
	// Explicit resume re-arms it for the owner (N7: as an immediately
	// claimable interrupted run).
	if err := s.ResumeItem(ctx, "item-1"); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if again == nil || again.ID != run.ID {
		t.Fatalf("owner must re-claim after ResumeItem: %+v", again)
	}
}

// M2: CancelItem retires interrupted runs too.
func TestCancelInterrupted(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	// Force the lease to expire, then let another claim sweep it to interrupted.
	time.Sleep(80 * time.Millisecond)
	if _, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws-a"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Status != StatusInterrupted {
		t.Fatalf("expected interrupted after lease expiry, got %s", got.Status)
	}
	if err := s.CancelItem(ctx, "item-1", "rejected"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, run.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("interrupted run must cancel with the item: %s", got.Status)
	}
}

// N1: a run queued by an action whose transition then fails (the
// non-approver path) is cancelled BY ID — and only that run.
func TestTargetedCancelOnFailedTransition(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	// An unrelated active run on another item — must survive.
	mustQueue(t, s, ctx, "ws-a", "item-other", StagePlan)
	var queued string
	r, err := s.QueueStage(ctx, "item-1", "ws-a", StagePlan, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	queued = r
	if queued == "" {
		t.Fatal("QueueStage must return the run id")
	}
	if err := s.CancelRun(ctx, queued, "transition failed"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, queued)
	if got.Status != StatusCancelled {
		t.Fatalf("targeted run must be cancelled: %s", got.Status)
	}
	other, _ := s.List(ctx, StatusQueued, "item-other")
	if len(other) != 1 {
		t.Fatalf("unrelated run must survive: %+v", other)
	}
}

// N4: interrupted runs age into branch-resumable once past the grace —
// re-evaluated by every sweep, not only at expiry time.
func TestGraceFlipOnSweep(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	// Expire the lease with a time far past the grace.
	longGone := time.Now().UTC().Add(-2 * takeoverGrace)
	if _, err := s.storage.db.ExecContext(ctx,
		`UPDATE runs SET lease_expires_at = ? WHERE id = ?`, formatTime(longGone), run.ID); err != nil {
		t.Fatal(err)
	}
	// Any worker's claim sweeps: the run flips to branch, and the foreign
	// worker CAN now take it over.
	r2, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws-a"}})
	if err != nil || r2 == nil || r2.ID != run.ID {
		t.Fatalf("grace flip must allow takeover: %+v err %v", r2, err)
	}
	if r2.ResumeKind != ResumeBranch {
		t.Fatalf("takeover must be branch-kind: %s", r2.ResumeKind)
	}
}

// A2: a quota pause with resume_after is reclaimable by its owner once the
// time passes (restart-safe); an amendment pause (no resume_after) is not.
func TestQuotaPauseReclaim(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	// Quota pause until 50ms from now.
	if err := s.Pause(ctx, run.ID, "w1", "quota: limit", time.Now().UTC().Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Before the reset: the owner gets nothing (paused, not yet due).
	if r, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}}); r != nil {
		t.Fatalf("quota run must not be reclaimed before the reset: %+v", r)
	}
	time.Sleep(80 * time.Millisecond)
	// After the reset: the owner reclaims it — no Resume call needed.
	r, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if r == nil || r.ID != run.ID {
		t.Fatalf("quota run must be reclaimable after the reset: %+v", r)
	}
}

// Queue guards: duplicates refused, unknown stages refused, scope enforced.
func TestQueueGuards(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	if _, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StagePlan}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate active run must refuse: %v", err)
	}
	if _, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: "deploy"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown stage: %v", err)
	}
	if _, err := s.Queue(scopedCtx("ws-b"), QueueInput{WorkspaceID: "ws-a", ItemID: "item-9", Stage: StagePlan}); err == nil {
		t.Fatal("foreign workspace queue must refuse")
	}
	// QueueItem (the inbox hook) swallows duplicates.
	if err := s.QueueItem(ctx, "item-1", "ws-a", StagePlan); err != nil {
		t.Fatalf("QueueItem duplicate should be a no-op: %v", err)
	}
}

// CancelItem retires active runs (item rejected or done).
func TestCancelItem(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StagePlan)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err := s.CancelItem(ctx, "item-1", "item rejected"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Status != StatusCancelled || got.FailReason != "item rejected" {
		t.Fatalf("cancel state wrong: %+v", got)
	}
}

func mustQueue(t *testing.T, s *Service, ctx context.Context, ws, item, stage string) *Run {
	t.Helper()
	r, err := s.Queue(ctx, QueueInput{WorkspaceID: ws, ItemID: item, Stage: stage})
	if err != nil {
		t.Fatalf("queue %s/%s/%s: %v", ws, item, stage, err)
	}
	return r
}

func assertMoved(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if w == "" {
			continue
		}
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("expected transition %q; got %v", w, got)
		}
	}
}
