package runs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func lookupToken(t *testing.T, s *Service, token string) *auth.RunIdentity {
	t.Helper()
	run, err := s.storage.LookupRunToken(context.Background(), auth.HashKey(token))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func claimOne(t *testing.T, s *Service, worker string) *Run {
	t.Helper()
	run, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: worker, Workspaces: []string{"ws-a"}})
	if err != nil || run == nil {
		t.Fatalf("claim by %s: %+v err %v", worker, run, err)
	}
	return run
}

// A claim mints the attempt's agent token: it resolves to the run while the
// run is claimed or running, and names the run's workspace, item, plan,
// stage and worker.
func TestAgentTokenResolvesToItsRun(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	if _, err := s.storage.db.Exec(`INSERT INTO inbox_items (id, workspace_id, title, author_agent_id, plan_id, created_at, updated_at)
		VALUES ('item-1', 'ws-a', 't', 'a', 'plan-1', ?, ?)`, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run := claimOne(t, s, "w1")
	if !strings.HasPrefix(run.AgentToken, auth.RunTokenPrefix) || len(run.AgentToken) < 20 {
		t.Fatalf("the claim must return an agent token: %q", run.AgentToken)
	}
	got := lookupToken(t, s, run.AgentToken)
	if got == nil || got.ID != run.ID || got.WorkspaceID != "ws-a" || got.ItemID != "item-1" ||
		got.PlanID != "plan-1" || got.Stage != StageImplement || got.WorkerID != "w1" {
		t.Fatalf("token identity: %+v", got)
	}
	// Still valid once the first heartbeat makes the run "running".
	if err := s.Heartbeat(ctx, run.ID, "w1", run.LeaseToken, ""); err != nil {
		t.Fatal(err)
	}
	if lookupToken(t, s, run.AgentToken) == nil {
		t.Fatal("the token must stay valid while the run runs")
	}
	// The stored form is a hash; the token itself is in no row, and no
	// read of the run carries it.
	var n int
	if err := s.storage.db.QueryRow(`SELECT COUNT(*) FROM run_tokens WHERE token_hash = ?`, run.AgentToken).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the token must not be stored in clear: %d %v", n, err)
	}
	if again, err := s.Get(ctx, run.ID); err != nil || again.AgentToken != "" {
		t.Fatalf("only the claim answer carries the token: %+v %v", again, err)
	}
	if lookupToken(t, s, auth.RunTokenPrefix+"unknown") != nil {
		t.Fatal("an unknown token resolves to nothing")
	}
}

// Everything that takes a run out of execution ends its token at once: a
// report, a pause, a retry requeue, a cancel and an expired lease.
func TestAgentTokenEndsWithExecution(t *testing.T) {
	ends := map[string]func(t *testing.T, s *Service, run *Run){
		"report": func(t *testing.T, s *Service, run *Run) {
			if _, err := s.Report(rootCtx(), run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded}, run.LeaseToken); err != nil {
				t.Fatal(err)
			}
		},
		"pause": func(t *testing.T, s *Service, run *Run) {
			if err := s.Pause(rootCtx(), run.ID, "w1", "quota", time.Now().Add(time.Hour), run.LeaseToken); err != nil {
				t.Fatal(err)
			}
		},
		"retry requeue": func(t *testing.T, s *Service, run *Run) {
			if _, err := s.Report(rootCtx(), run.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "setup failed", Retryable: true}, run.LeaseToken); err != nil {
				t.Fatal(err)
			}
		},
		"cancel": func(t *testing.T, s *Service, run *Run) {
			if err := s.CancelItem(rootCtx(), run.ItemID, "rejected"); err != nil {
				t.Fatal(err)
			}
		},
		"expired lease": func(t *testing.T, s *Service, run *Run) {
			// No sweep has run: the run still reads "claimed".
			if _, err := s.storage.db.Exec(`UPDATE runs SET lease_expires_at = ? WHERE id = ?`,
				formatTime(time.Now().Add(-time.Minute)), run.ID); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			s := testService(t)
			mustQueue(t, s, rootCtx(), "ws-a", "item-1", StageImplement)
			run := claimOne(t, s, "w1")
			if lookupToken(t, s, run.AgentToken) == nil {
				t.Fatal("valid before")
			}
			end(t, s, run)
			if got := lookupToken(t, s, run.AgentToken); got != nil {
				t.Fatalf("the token must be dead after %s: %+v", name, got)
			}
		})
	}
}

// A paused run's token is dead while it waits, and its resume — a new
// claim — mints a new one. The old token never comes back, and its end is
// on record.
func TestAgentTokenPerAttempt(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	first := claimOne(t, s, "w1")
	if err := s.Pause(ctx, first.ID, "w1", "quota", time.Now().Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if lookupToken(t, s, first.AgentToken) != nil {
		t.Fatal("a paused run's token is dead")
	}
	second := claimOne(t, s, "w1") // the quota pause is past its resume time
	if second.ID != first.ID || second.AgentToken == "" || second.AgentToken == first.AgentToken {
		t.Fatalf("the resume must mint a new token: %q then %q", first.AgentToken, second.AgentToken)
	}
	if lookupToken(t, s, second.AgentToken) == nil || lookupToken(t, s, first.AgentToken) != nil {
		t.Fatal("only the newest attempt's token works")
	}
	var open, ended int
	if err := s.storage.db.QueryRow(`SELECT
		COALESCE(SUM(ended_ms IS NULL), 0), COALESCE(SUM(ended_ms IS NOT NULL), 0) FROM run_tokens WHERE run_id = ?`, first.ID).
		Scan(&open, &ended); err != nil {
		t.Fatal(err)
	}
	if open != 1 || ended != 1 {
		t.Fatalf("one open token and one ended: open=%d ended=%d", open, ended)
	}

	// After the report, the next claim poll (any worker's) records the end
	// of the last one too.
	if _, err := s.Report(ctx, second.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded}, second.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws-a"}}); err != nil || run != nil {
		t.Fatalf("idle poll: %+v %v", run, err)
	}
	if err := s.storage.db.QueryRow(`SELECT COUNT(*) FROM run_tokens WHERE run_id = ? AND ended_ms IS NULL`, first.ID).Scan(&open); err != nil || open != 0 {
		t.Fatalf("every token of a finished run is ended: open=%d %v", open, err)
	}
}

// A takeover by another worker ends the first worker's token.
func TestAgentTokenTakeover(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	first := claimOne(t, s, "w1")
	longGone := time.Now().Add(-2 * takeoverGrace)
	if _, err := s.storage.db.Exec(`UPDATE runs SET lease_expires_at = ? WHERE id = ?`, formatTime(longGone), first.ID); err != nil {
		t.Fatal(err)
	}
	second := claimOne(t, s, "w2")
	if second.ID != first.ID || second.WorkerID != "w2" {
		t.Fatalf("takeover: %+v", second)
	}
	if lookupToken(t, s, first.AgentToken) != nil {
		t.Fatal("the first worker's token must be dead after the takeover")
	}
	if got := lookupToken(t, s, second.AgentToken); got == nil || got.WorkerID != "w2" {
		t.Fatalf("the new token belongs to the new worker: %+v", got)
	}
}
