package runs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// groupFixture: a service whose workspace routes review to three members,
// an inbox item row (run_groups references inbox_items), and recorders for
// the item-side callbacks.
type groupFixture struct {
	s        *Service
	mu       sync.Mutex
	moves    []string // "to|reason"
	sections []string // "heading\nbody"
}

func newGroupFixture(t *testing.T, policy StagePolicy, members ...Member) *groupFixture {
	t.Helper()
	s := testService(t)
	if len(members) == 0 {
		members = []Member{{Runner: "claude", Focus: "security"}, {Runner: "glm", Focus: "tests"}, {Runner: "qwen", Focus: "conventions"}}
	}
	s.SetPrefs(map[string]*WorkspacePrefs{
		"ws-a": {Stages: map[string]string{"implement": "claude"}, Groups: map[string][]Member{"review": members}, Policies: map[string]StagePolicy{"review": policy}},
	})
	f := &groupFixture{s: s}
	s.SetItemTransition(func(_ context.Context, _, to, reason, _ string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.moves = append(f.moves, to+"|"+reason)
		return nil
	})
	s.SetItemAppend(func(_ context.Context, _, heading, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sections = append(f.sections, heading+"\n"+body)
		return nil
	})
	insertItem(t, s, "item-1")
	return f
}

func insertItem(t *testing.T, s *Service, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.storage.db.Exec(`INSERT INTO inbox_items (id, workspace_id, title, author_agent_id, status, created_at, updated_at) VALUES (?, 'ws-a', 't', 'test', 'implementing', ?, ?)`, id, now, now); err != nil {
		t.Fatal(err)
	}
}

// claimAs claims the next run for a worker offering exactly one runner.
func (f *groupFixture) claimAs(t *testing.T, worker, runner string) *Run {
	t.Helper()
	r, err := f.s.ClaimNext(rootCtx(), ClaimInput{WorkerID: worker, Runners: []string{runner}, Workspaces: []string{"ws-a"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *groupFixture) report(t *testing.T, r *Run, worker string, pass bool, findings ...Finding) {
	t.Helper()
	in := ReportInput{WorkerID: worker, Status: StatusSucceeded, Summary: "ok", Findings: findings}
	if !pass {
		in.Status, in.Reason = StatusFailed, "review failed: issues"
	}
	if _, err := f.s.Report(rootCtx(), r.ID, in, r.LeaseToken); err != nil {
		t.Fatalf("report %s: %v", r.ID, err)
	}
}

// resetMoves forgets claim-time phase moves (claiming moves the item into
// its phase), so assertions see only what the group decided.
func (f *groupFixture) resetMoves() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = nil
}

func (f *groupFixture) lastMove() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.moves) == 0 {
		return ""
	}
	return f.moves[len(f.moves)-1]
}

// One runner per stage stays exactly today's single run (opt-in).
func TestGroupOptInOnly(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Stages: map[string]string{"review": "claude"}}})
	r := mustQueue(t, s, rootCtx(), "ws-a", "item-1", StageReview)
	if r.GroupID != "" || r.Runner != "claude" {
		t.Fatalf("single runner must not open a group: %+v", r)
	}
	// A single focused reviewer: still one run, carrying its focus.
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Stages: map[string]string{"review": "claude"}, Groups: map[string][]Member{"review": {{Runner: "claude", Focus: "security"}}}}})
	r2 := mustQueue(t, s, rootCtx(), "ws-a", "item-2", StageReview)
	if r2.GroupID != "" || r2.Focus != "security" {
		t.Fatalf("single focused reviewer: %+v", r2)
	}
}

// Members are pinned to their own runner and carry their focus; a second
// group for the same item and stage is refused by the database.
func TestGroupQueueAndPinning(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{})
	r := mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	if r.GroupID == "" {
		t.Fatal("three members must open a group")
	}
	if _, err := f.s.Queue(rootCtx(), QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageReview}); !errors.Is(err, ErrDuplicateActive) {
		t.Fatalf("second review while the group is active: %v", err)
	}
	// A worker offering only glm gets exactly the tests member.
	got := f.claimAs(t, "w-glm", "glm")
	if got == nil || got.Runner != "glm" || got.Focus != "tests" || got.GroupID != r.GroupID {
		t.Fatalf("pinned claim: %+v", got)
	}
	if again := f.claimAs(t, "w-glm2", "glm"); again != nil {
		t.Fatalf("no other glm member exists, got %+v", again)
	}
	// The open-group invariant holds even past the Go-side check.
	g, _ := f.s.storage.GetGroup(rootCtx(), r.GroupID)
	g2 := *g
	g2.ID, g2.Round = "dup", g.Round+1
	if err := f.s.storage.CreateGroup(rootCtx(), g2, nil); !errors.Is(err, ErrDuplicateActive) {
		t.Fatalf("database must refuse a second open group: %v", err)
	}
}

// any-blocks (default): waits for every member, fails on any failure,
// writes one summary with merged findings, and blocks the item.
func TestGroupAnyBlocks(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{})
	mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	sec := f.claimAs(t, "w1", "claude")
	tst := f.claimAs(t, "w2", "glm")
	cnv := f.claimAs(t, "w3", "qwen")
	f.resetMoves()
	f.report(t, sec, "w1", false, Finding{Severity: SeverityBlocking, Text: "SQL built by string concat.", File: "db.go:10"})
	if f.lastMove() != "" {
		t.Fatal("decided before every member finished")
	}
	f.report(t, tst, "w2", true, Finding{Severity: SeverityNit, Text: "name the test table-driven"})
	f.report(t, cnv, "w3", false, Finding{Severity: SeverityBlocking, Text: "sql built by string concat", File: "db.go:10"})
	if m := f.lastMove(); !strings.HasPrefix(m, "blocked|review group failed (any-blocks): ") || !strings.Contains(m, "security · claude") {
		t.Fatalf("move: %q", m)
	}
	if len(f.sections) != 1 {
		t.Fatalf("one summary, got %d", len(f.sections))
	}
	sum := f.sections[0]
	for _, want := range []string{"Review summary — round 1", "**Outcome:** fail", "### Blocking", "`db.go:10` SQL built by string concat.", "conventions · qwen, security · claude", "### Nits", "tests · glm"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
	if strings.Count(sum, "string concat") != 1 {
		t.Errorf("duplicate finding not merged:\n%s", sum)
	}
}

// All members pass → the item moves on to in_review (no qa configured).
func TestGroupPassAdvances(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{}, Member{Runner: "claude"}, Member{Runner: "glm"})
	mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	a, b := f.claimAs(t, "w1", "claude"), f.claimAs(t, "w2", "glm")
	f.report(t, a, "w1", true)
	f.report(t, b, "w2", true)
	if m := f.lastMove(); m != "in_review|" {
		t.Fatalf("pass must hand the item to Martin: %q (%v)", m, f.moves)
	}
}

// early-exit: the first blocking member decides; the rest are cancelled
// and their late reports are refused.
func TestGroupEarlyExit(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{Policy: PolicyAnyBlocks, EarlyExit: true})
	r := mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	sec, tst := f.claimAs(t, "w1", "claude"), f.claimAs(t, "w2", "glm")
	f.resetMoves()
	f.report(t, sec, "w1", false, Finding{Severity: SeverityBlocking, Text: "x"})
	if !strings.HasPrefix(f.lastMove(), "blocked|") {
		t.Fatalf("early exit must decide: %v", f.moves)
	}
	members, _ := f.s.storage.GroupMembers(rootCtx(), r.GroupID)
	for _, m := range members {
		if m.ID != sec.ID && m.Status != StatusCancelled {
			t.Fatalf("member %s (%s) not cancelled", m.Focus, m.Status)
		}
	}
	if _, err := f.s.Report(rootCtx(), tst.ID, ReportInput{WorkerID: "w2", Status: StatusSucceeded}, tst.LeaseToken); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("late report on a cancelled member: %v", err)
	}
}

// majority: one failure out of three does not fail the group.
func TestGroupMajority(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{Policy: PolicyMajority})
	mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	a, b, c := f.claimAs(t, "w1", "claude"), f.claimAs(t, "w2", "glm"), f.claimAs(t, "w3", "qwen")
	f.report(t, a, "w1", false, Finding{Severity: SeverityBlocking, Text: "style"})
	f.report(t, b, "w2", true)
	f.report(t, c, "w3", true)
	if m := f.lastMove(); m != "in_review|" {
		t.Fatalf("majority pass: %q", m)
	}
}

// The deadline decides with what exists: a member nobody claimed is
// cancelled and named; with a pass and no failure the group passes.
func TestGroupDeadlineSweep(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{Timeout: time.Minute}, Member{Runner: "claude"}, Member{Runner: "gemini"})
	r := mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	a := f.claimAs(t, "w1", "claude")
	f.resetMoves()
	f.report(t, a, "w1", true)
	f.s.SweepGroups(rootCtx()) // not expired yet
	if f.lastMove() != "" {
		t.Fatal("swept before the deadline")
	}
	f.s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	f.s.SweepGroups(rootCtx())
	if m := f.lastMove(); m != "in_review|" {
		t.Fatalf("deadline with one pass: %q", m)
	}
	members, _ := f.s.storage.GroupMembers(rootCtx(), r.GroupID)
	var cancelled bool
	for _, m := range members {
		if m.Runner == "gemini" && m.Status == StatusCancelled && m.FailReason == "group deadline passed" {
			cancelled = true
		}
	}
	if !cancelled || !strings.Contains(f.sections[0], "**gemini**: cancelled (group deadline passed)") {
		t.Fatalf("unclaimed member must be cancelled and named: %+v\n%s", members, f.sections)
	}
	// Nothing finished at all → timeout blocks.
	insertItem(t, f.s, "item-2")
	f.s.now = time.Now
	mustQueue(t, f.s, rootCtx(), "ws-a", "item-2", StageReview)
	f.s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	f.s.SweepGroups(rootCtx())
	if m := f.lastMove(); !strings.HasPrefix(m, "blocked|review group: no member finished") {
		t.Fatalf("timeout: %q", m)
	}
}

// Rejecting the item closes the group without a decision; a later round
// (after re-implementation) opens as round 2.
func TestGroupCancelAndNextRound(t *testing.T) {
	f := newGroupFixture(t, StagePolicy{})
	r := mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	if err := f.s.CancelItem(rootCtx(), "item-1", "item rejected"); err != nil {
		t.Fatal(err)
	}
	g, _ := f.s.storage.GetGroup(rootCtx(), r.GroupID)
	if g.State != GroupCancelled || g.Outcome != "" || f.lastMove() != "" {
		t.Fatalf("cancelled group: %+v moves %v", g, f.moves)
	}
	r2 := mustQueue(t, f.s, rootCtx(), "ws-a", "item-1", StageReview)
	g2, _ := f.s.storage.GetGroup(rootCtx(), r2.GroupID)
	if g2.Round != 2 {
		t.Fatalf("next round = %d, want 2", g2.Round)
	}
	views, err := f.s.ListGroups(rootCtx(), "item-1")
	if err != nil || len(views) != 2 || len(views[0].Members) != 3 {
		t.Fatalf("list groups: %v %+v", err, views)
	}
}
