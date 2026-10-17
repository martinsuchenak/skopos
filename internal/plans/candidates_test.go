package plans

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

// candidateFixture: a file-backed database (transactions need one shared
// database, not per-connection :memory:), a plan, and one plan group row
// (plan_revisions.group_id references run_groups, which references the item).
func candidateFixture(t *testing.T) (*Service, *sql.DB, string, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "c.db") + "?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	svc := NewService(NewStorage(sqlDB))
	plan, err := svc.CreatePlan(context.Background(), CreatePlanInput{WorkspaceID: "ws-a", Name: "p", AuthorAgentID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := sqlDB.Exec(`INSERT INTO inbox_items (id, workspace_id, title, author_agent_id, status, created_at, updated_at) VALUES ('item-1','ws-a','t','t','planning',?,?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ id, state string }{{"g1", "decided"}, {"g2", "open"}} {
		round := 1
		outcome, decided := any("candidates"), any(now.UnixMilli())
		if g.state == "open" {
			round, outcome, decided = 2, nil, nil
		}
		if _, err := sqlDB.Exec(`INSERT INTO run_groups (id, workspace_id, item_id, stage, round, size, policy, state, outcome, deadline_ms, decided_ms, created_ms)
			VALUES (?, 'ws-a', 'item-1', 'plan', ?, 2, 'candidates', ?, ?, ?, ?, ?)`, g.id, round, g.state, outcome, now.Add(time.Hour).UnixMilli(), decided, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	return svc, sqlDB, plan.ID, "g1"
}

func TestCandidates(t *testing.T) {
	svc, _, planID, g1 := candidateFixture(t)
	ctx := context.Background()
	c1, err := svc.CreateCandidate(ctx, planID, []RevisionStep{{Title: "small fix"}}, "base1", nil, g1, "claude")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := svc.CreateCandidate(ctx, planID, []RevisionStep{{Title: "bigger"}, {Title: "tests", Description: "table-driven"}}, "base1", []string{"go-tests", "go-tests"}, g1, "glm")
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Steps) != 3 || c2.Steps[2].Phase != "review" || c2.Steps[0].ID == "" || len(c2.Skills) != 1 {
		t.Fatalf("candidate snapshot: %+v", c2)
	}
	plan, _ := svc.GetPlan(ctx, planID)
	if len(plan.Items) != 0 {
		t.Fatal("candidates must not touch the live plan")
	}
	open, err := svc.OpenCandidates(ctx, planID)
	if err != nil || len(open) != 2 || open[0].ID != c1.ID {
		t.Fatalf("open candidates: %+v %v", open, err)
	}

	// Approving the OLDER candidate: latest must then be it, not the
	// newer (superseded) sibling, and the live plan must match it exactly.
	got, err := svc.ApproveCandidate(ctx, planID, c1.ID)
	if err != nil || got.CandidateState != CandidateApproved || got.LockedAt == nil {
		t.Fatalf("approve: %+v %v", got, err)
	}
	latest, _ := svc.LatestRevision(ctx, planID)
	if latest.ID != c1.ID {
		t.Fatalf("latest after approval = %s, want the approved candidate %s", latest.ID, c1.ID)
	}
	if _, err := svc.VerifyRevisionCurrent(ctx, planID); err != nil {
		t.Fatalf("the live plan must hash to the approved candidate: %v", err)
	}
	sib, _ := svc.GetRevision(ctx, c2.ID)
	if sib.CandidateState != CandidateSuperseded {
		t.Fatalf("sibling: %s", sib.CandidateState)
	}
	if _, err := svc.ApproveCandidate(ctx, planID, c2.ID); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("a superseded candidate must be stale: %v", err)
	}
	// The plan is locked now: no new candidates.
	if _, err := svc.CreateCandidate(ctx, planID, []RevisionStep{{Title: "late"}}, "b", nil, g1, "late"); !errors.Is(err, ErrRevisionLocked) {
		t.Fatalf("candidate on a locked plan: %v", err)
	}
}

// Only the current round's candidates are approvable: once round 2 has a
// candidate, an open round-1 candidate is stale.
func TestCandidateOldRoundIsStale(t *testing.T) {
	svc, _, planID, g1 := candidateFixture(t)
	ctx := context.Background()
	old, _ := svc.CreateCandidate(ctx, planID, []RevisionStep{{Title: "r1"}}, "b", nil, g1, "claude")
	if _, err := svc.CreateCandidate(ctx, planID, []RevisionStep{{Title: "r2"}}, "b", nil, "g2", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveCandidate(ctx, planID, old.ID); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("round-1 candidate after round 2 started: %v", err)
	}
	if err := svc.SupersedeOpenCandidates(ctx, planID); err != nil {
		t.Fatal(err)
	}
	if open, _ := svc.OpenCandidates(ctx, planID); len(open) != 0 {
		t.Fatalf("supersede must close the round: %+v", open)
	}
}
