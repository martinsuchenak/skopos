package runs

import (
	"context"
	"strings"
	"testing"
)

// A review-only item's branch_review run goes to the workspace's review
// runner, moves the item into implementing on claim, and hands it to
// The approver at in_review whatever the verdict; a run that couldn't review
// blocks it.
func TestBranchReviewRun(t *testing.T) {
	s := testService(t)
	insertItem(t, s, "item-1")
	insertItem(t, s, "item-2")
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Stages: map[string]string{StageReview: "codex"}}})
	var moves []string
	s.SetItemTransition(func(_ context.Context, item, to, _, _ string) error { moves = append(moves, item+":"+to); return nil })
	ctx := rootCtx()

	r, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageBranchReview, Branch: "JOB9-x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Runner != "codex" || r.Branch != "JOB9-x" {
		t.Fatalf("queued: runner=%q branch=%q", r.Runner, r.Branch)
	}
	c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if c == nil || c.ID != r.ID {
		t.Fatalf("claim: %+v", c)
	}
	if err := s.Heartbeat(ctx, c.ID, "w1", c.LeaseToken, PhaseReviewing); err != nil {
		t.Fatalf("reviewing phase: %v", err)
	}
	moves = nil
	// A failing verdict still succeeds the run: the findings are the verdict.
	if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, HeadSHA: "abc1234def", Branch: "JOB9-x",
		Findings: []Finding{{Severity: SeverityBlocking, Text: "no header row"}}}, c.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if len(moves) != 2 || moves[0] != "item-1:implementing" || moves[1] != "item-1:in_review" {
		t.Fatalf("moves: %v", moves)
	}
	if got := s.latestHead(ctx, "item-1"); got != "abc1234def" {
		t.Fatalf("the reviewed head is the head sign-off approves: %q", got)
	}

	r2, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-2", Stage: StageBranchReview, Branch: "JOB9-y"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if c == nil || c.ID != r2.ID {
		t.Fatalf("claim 2: %+v", c)
	}
	moves = nil
	if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "branch JOB9-y is not on origin"}, c.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0] != "item-2:blocked" {
		t.Fatalf("failed run: %v", moves)
	}
}

// A review-only item follows the review stage's routing: a panel opens a
// group (each member with its focus, the review policy), the summary
// carries each member's verdict, and the item reaches in_review whatever it
// says; solo runs the first member without its focus; a single focused
// reviewer keeps its focus; a panel where nobody managed to review blocks.
func TestBranchReviewPanel(t *testing.T) {
	s := testService(t)
	for _, id := range []string{"item-1", "item-2", "item-3", "item-4"} {
		insertItem(t, s, id)
	}
	panel := []Member{{Runner: "claude", Focus: "quality"}, {Runner: "codex", Focus: "security"}}
	s.SetPrefs(map[string]*WorkspacePrefs{
		"ws-a": {Groups: map[string][]Member{StageReview: panel}},
		// One focused reviewer, as pipeline.ToPrefs stores it.
		"ws-b": {Stages: map[string]string{StageReview: "codex"}, Groups: map[string][]Member{StageReview: {{Runner: "codex", Focus: "security"}}}},
	})
	agents := map[string]string{"item-2": AgentsSolo}
	s.SetItemAgents(func(_ context.Context, id string) string { return agents[id] })
	var moves, sections []string
	s.SetItemTransition(func(_ context.Context, item, to, _, _ string) error { moves = append(moves, item+":"+to); return nil })
	s.SetItemAppend(func(_ context.Context, _, heading, body string) error {
		sections = append(sections, heading+"\n"+body)
		return nil
	})
	ctx := rootCtx()

	first, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageBranchReview, Branch: "JOB9-x"})
	if err != nil {
		t.Fatal(err)
	}
	if first.GroupID == "" {
		t.Fatalf("a panel must open a group: %+v", first)
	}
	members, _ := s.storage.GroupMembers(ctx, first.GroupID)
	if len(members) != 2 || members[0].Focus != "quality" || members[1].Focus != "security" || members[1].Runner != "codex" {
		t.Fatalf("members: %+v", members)
	}
	findings := map[string][]Finding{"claude": nil, "codex": {{Severity: SeverityBlocking, Text: "SQL by concatenation"}}}
	for _, runner := range []string{"claude", "codex"} {
		c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-" + runner, Runners: []string{runner}, Workspaces: []string{"ws-a"}})
		if c == nil || c.Stage != StageBranchReview {
			t.Fatalf("claim %s: %+v", runner, c)
		}
		if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w-" + runner, Status: StatusSucceeded, HeadSHA: "abc1234def", Findings: findings[runner]}, c.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	if len(sections) != 1 || !strings.HasPrefix(sections[0], "Review summary — round 1\n**Outcome:** fail") ||
		!strings.Contains(sections[0], "**quality · claude**: pass") || !strings.Contains(sections[0], "**security · codex**: fail") {
		t.Fatalf("summary: %q", sections)
	}
	if len(moves) == 0 || moves[len(moves)-1] != "item-1:in_review" {
		t.Fatalf("a failing panel verdict still goes to in_review: %v", moves)
	}

	solo, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-2", Stage: StageBranchReview, Branch: "JOB9-y"})
	if err != nil {
		t.Fatal(err)
	}
	if solo.GroupID != "" || solo.Runner != "claude" || solo.Focus != "" {
		t.Fatalf("solo: %+v", solo)
	}
	_ = s.CancelRun(ctx, solo.ID, "checked")
	focused, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-b", ItemID: "item-3", Stage: StageBranchReview, Branch: "JOB9-z"})
	if err != nil {
		t.Fatal(err)
	}
	if focused.GroupID != "" || focused.Runner != "codex" || focused.Focus != "security" {
		t.Fatalf("single focused reviewer: %+v", focused)
	}
	_ = s.CancelRun(ctx, focused.ID, "checked")

	moves = nil
	r4, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-4", Stage: StageBranchReview, Branch: "JOB9-gone"})
	if err != nil {
		t.Fatal(err)
	}
	for _, runner := range []string{"claude", "codex"} {
		c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-" + runner, Runners: []string{runner}, Workspaces: []string{"ws-a"}})
		if c == nil || c.GroupID != r4.GroupID {
			t.Fatalf("claim %s: %+v", runner, c)
		}
		if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w-" + runner, Status: StatusFailed, Reason: "branch JOB9-gone is not on origin"}, c.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	if len(moves) == 0 || moves[len(moves)-1] != "item-4:blocked" {
		t.Fatalf("nobody reviewed: %v", moves)
	}
}
