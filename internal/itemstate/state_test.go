package itemstate

import (
	"context"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
)

type fakeItems struct{ item *inbox.Item }

func (f fakeItems) ReadItem(context.Context, string) (*inbox.Item, error) { return f.item, nil }

type fakeRuns []runs.Run

func (f fakeRuns) List(context.Context, runs.Status, string) ([]runs.Run, error) { return f, nil }

type fakeQuestions struct{ rec *qa.Record }

func (f fakeQuestions) Record(context.Context, string) (*qa.Record, error) { return f.rec, nil }

// At sign-off: the reviews of the newest reviewed head only (a panel and a
// second opinion), the newest implementation, the open questions, and the
// sign-off actions.
func TestStateAtSignOff(t *testing.T) {
	pushed := true
	all := fakeRuns{ // newest first, as the runs service lists them
		{ID: "r6", Stage: runs.StageSecondOpinion, Status: runs.StatusSucceeded, HeadSHA: "bbb", Runner: "codex",
			Findings: []runs.Finding{{Severity: runs.SeverityNit, Text: "naming"}}},
		{ID: "r5", Stage: runs.StageReview, Status: runs.StatusFailed, HeadSHA: "bbb", Runner: "claude", Focus: "security",
			Findings: []runs.Finding{{Severity: runs.SeverityBlocking, Text: "sql injection", File: "a.go"}}},
		{ID: "r4", Stage: runs.StageReview, Status: runs.StatusSucceeded, HeadSHA: "bbb", Runner: "opencode"},
		{ID: "r3", Stage: runs.StageImplement, Status: runs.StatusSucceeded, HeadSHA: "bbb", Pushed: &pushed, PRURL: "https://github.com/x/y/pull/1",
			Checks: []runs.Check{{Name: "test", Passed: true}}},
		{ID: "r2", Stage: runs.StageReview, Status: runs.StatusSucceeded, HeadSHA: "aaa"},
		{ID: "r1", Stage: runs.StageImplement, Status: runs.StatusFailed, HeadSHA: "aaa", FailReason: "checks failed"},
	}
	rec := &qa.Record{Sets: []qa.Set{
		{ID: "s1", Asker: qa.AskerImplementer, Open: true, Blocking: false, Questions: []qa.Question{
			{ID: "q1", Text: "Keep the flag?", Options: []string{"yes", "no"}, State: qa.StateOpen},
			{ID: "q2", Text: "Which name?", State: qa.StateAnswered},
		}},
		{ID: "s0", Asker: qa.AskerPlanner, Open: false, Questions: []qa.Question{{ID: "q0", Text: "old", State: qa.StateAnswered}}},
	}}
	item := &inbox.Item{ID: "i1", WorkspaceID: "ws", Title: "t", Status: inbox.StatusInReview,
		Content: "The brief.\n\n## Questions — planner\n\n1. old", Ref: "WR1"}
	svc := &Service{Items: fakeItems{item}, Runs: all, Questions: fakeQuestions{rec}}

	st, err := svc.Get(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Turn != TurnPerson || st.Gate != GateSignOff || st.Brief != "The brief." || st.Run != nil {
		t.Fatalf("state: %+v", st)
	}
	if got := strings.Join(st.Actions, ","); got != "mark-done,request-changes,second-opinion,reject" {
		t.Fatalf("actions: %s", got)
	}
	if st.Implementation == nil || st.Implementation.RunID != "r3" || st.Implementation.PRURL == "" || len(st.Implementation.Checks) != 1 {
		t.Fatalf("implementation: %+v", st.Implementation)
	}
	if len(st.Reviews) != 3 {
		t.Fatalf("reviews: %+v", st.Reviews)
	}
	byID := map[string]ReviewView{}
	for _, r := range st.Reviews {
		byID[r.RunID] = r
	}
	if r := byID["r5"]; r.Passed || r.Blocking != 1 || r.Focus != "security" {
		t.Fatalf("r5: %+v", r)
	}
	if r := byID["r6"]; !r.Passed || r.Nits != 1 {
		t.Fatalf("r6: %+v", r)
	}
	if r := byID["r4"]; !r.Passed || r.Findings == nil {
		t.Fatalf("r4: %+v", r)
	}
	if len(st.Questions) != 1 || st.Questions[0].ID != "q1" || st.Questions[0].Asker != qa.AskerImplementer {
		t.Fatalf("questions: %+v", st.Questions)
	}
}

// Blocked: the reason is the newest failure; a run in progress shows; a
// key without the approver permission gets no actions.
func TestStateBlockedAndActions(t *testing.T) {
	item := &inbox.Item{ID: "i1", WorkspaceID: "ws", Status: inbox.StatusBlocked}
	svc := &Service{Items: fakeItems{item}, Runs: fakeRuns{
		{ID: "r2", Stage: runs.StagePlan, Status: runs.StatusQueued},
		{ID: "r1", Stage: runs.StageImplement, Status: runs.StatusFailed, FailReason: "push rejected"},
	}}
	st, err := svc.Get(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Gate != GateBlocked || st.Reason != "push rejected" || st.Run == nil || st.Run.ID != "r2" || st.Implementation == nil {
		t.Fatalf("state: %+v", st)
	}
	if got := strings.Join(st.Actions, ","); got != "retry,answers,reject" {
		t.Fatalf("actions: %s", got)
	}
	viewer := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k", Name: "viewer", Workspaces: map[string]struct{}{"ws": {}}})
	if st, _ := svc.Get(viewer, "i1"); len(st.Actions) != 0 || st.Actions == nil {
		t.Fatalf("viewer actions: %#v", st.Actions)
	}

	// Not started: queue, and review when it names a branch or ref.
	open := &inbox.Item{ID: "i2", WorkspaceID: "ws", Status: inbox.StatusOpen}
	if got := strings.Join(Actions(context.Background(), open, nil), ","); got != "queue" {
		t.Fatalf("open: %s", got)
	}
	open.Ref = "WR1"
	if got := strings.Join(Actions(context.Background(), open, nil), ","); got != "queue,review" {
		t.Fatalf("open with ref: %s", got)
	}
	done := &inbox.Item{ID: "i3", WorkspaceID: "ws", Status: inbox.StatusDone}
	if tn, _ := turnAndGate(done.Status); tn != TurnNone || len(Actions(context.Background(), done, nil)) != 0 {
		t.Fatalf("done: %s", tn)
	}
}

// The review fixes: an interrupted run is still in progress; the reason
// comes from the current round only; a blocked-by-review item offers a
// second opinion, a running one hides it; a capped key without runs:read
// or plans:read sees neither.
func TestStateRoundsAndCapabilities(t *testing.T) {
	item := &inbox.Item{ID: "i1", WorkspaceID: "ws", Status: inbox.StatusImplementing, PlanID: "p1"}
	all := fakeRuns{
		{ID: "r3", Stage: runs.StageImplement, Status: runs.StatusInterrupted},
		{ID: "r2", Stage: runs.StagePlan, Status: runs.StatusSucceeded},
		{ID: "r1", Stage: runs.StageImplement, Status: runs.StatusFailed, FailReason: "checks failed"},
	}
	svc := &Service{Items: fakeItems{item}, Runs: all}
	st, err := svc.Get(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Run == nil || st.Run.ID != "r3" || st.Implementation == nil || st.Implementation.RunID != "r1" {
		t.Fatalf("interrupted: run=%+v impl=%+v", st.Run, st.Implementation)
	}
	if got := failReason(all); got != "" {
		t.Fatalf("an older round's failure: %q", got)
	}

	// Blocked by its review: second opinion offered, unless one is running.
	blocked := &inbox.Item{ID: "i2", WorkspaceID: "ws", Status: inbox.StatusBlocked}
	byReview := []runs.Run{
		{ID: "r5", Stage: runs.StageReview, Status: runs.StatusFailed, FailReason: "blocking findings", HeadSHA: "bbb"},
		{ID: "r4", Stage: runs.StageImplement, Status: runs.StatusSucceeded, HeadSHA: "bbb"},
	}
	if got := strings.Join(Actions(context.Background(), blocked, byReview), ","); got != "retry,answers,second-opinion,reject" {
		t.Fatalf("blocked by review: %s", got)
	}
	if got := failReason(byReview); got != "blocking findings" {
		t.Fatalf("review reason: %q", got)
	}
	inReview := &inbox.Item{ID: "i3", WorkspaceID: "ws", Status: inbox.StatusInReview}
	running := append([]runs.Run{{ID: "r6", Stage: runs.StageSecondOpinion, Status: runs.StatusRunning}}, byReview[1:]...)
	if got := strings.Join(Actions(context.Background(), inReview, running), ","); got != "mark-done,request-changes,reject" {
		t.Fatalf("second opinion running: %s", got)
	}

	// A capped key sees the sections its capabilities cover.
	capped := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k", Name: "inbox-only", Approver: true,
		Workspaces: map[string]struct{}{"ws": {}}, Capabilities: map[string]struct{}{"rest": {}, "inbox:read": {}}})
	svc.Plans = fakePlans{}
	st, err = svc.Get(capped, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Run != nil || st.Implementation != nil || len(st.Reviews) != 0 || st.Plan != nil {
		t.Fatalf("capped key: %+v", st)
	}
	if st, _ := svc.Get(context.Background(), "i1"); st.Plan == nil || len(st.Plan.Revisions) != 1 {
		t.Fatalf("uncapped plan: %+v", st.Plan)
	}
}

type fakePlans struct{}

func (fakePlans) LatestRevision(context.Context, string) (*plans.Revision, error) {
	return &plans.Revision{ID: "rev1", ContentHash: "h"}, nil
}
func (fakePlans) OpenCandidates(context.Context, string) ([]plans.Revision, error) { return nil, nil }
