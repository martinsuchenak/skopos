package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// The plan-ready message lists the newest round's questions one per line,
// options lettered on their own lines; an earlier round's questions (already
// answered) are not repeated.
func TestPlanReadyListsCurrentQuestions(t *testing.T) {
	svc, _, plansSvc, _, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "cache")
	for _, s := range [][2]string{
		{"Enrichment — skopos worker", "OLD plan"},
		{"Questions — planner", "1. Old question?\n"},
		{"Answers", "1. Old question?\n   Answer: yes"},
		{"Enrichment — skopos worker", "NEW plan"},
		{"Questions — planner", "1. Which backend?\n   - (a) Redis\n   - (b) in-process\n2. Keep the flag?\n"},
	} {
		if err := svc.AppendSection(root, item.ID, s[0], s[1], audit.ViaWorker); err != nil {
			t.Fatal(err)
		}
	}
	msg, _ := relay.planReadyMessage(item.ID)
	for _, want := range []string{"*Questions for you*", "*1.* Which backend?\n      a) Redis\n      b) in-process\n*2.* Keep the flag?\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("plan-ready message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Old question") {
		t.Errorf("an answered round's questions must not be repeated:\n%s", msg)
	}
}

// A planner that needs answers blocks the item: Slack lists its questions
// instead of the run's one-line reason.
func TestRelayBlockedListsQuestions(t *testing.T) {
	svc, runsSvc, _, _, relay := relayStack(t)
	ctx := context.Background()
	item, err := svc.CreateItem(ctx, inbox.CreateInput{WorkspaceID: "ws-a", Title: "vague", AuthorAgentID: "martin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runsSvc.Queue(ctx, runs.QueueInput{WorkspaceID: "ws-a", ItemID: item.ID, Stage: runs.StagePlan}); err != nil {
		t.Fatal(err)
	}
	r, err := runsSvc.ClaimNext(ctx, runs.ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err != nil || r == nil {
		t.Fatalf("claim: %v", err)
	}
	questions := "1. Which repository?\n   - (a) api\n   - (b) web\n"
	if err := svc.AppendSection(ctx, item.ID, "Questions — planner", questions, audit.ViaWorker); err != nil {
		t.Fatal(err)
	}
	if _, err := runsSvc.Report(ctx, r.ID, runs.ReportInput{WorkerID: "w1", Status: runs.StatusFailed, Reason: "needs clarification: " + questions}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	msg := relay.formatGate(item.ID, inbox.StatusPlanning, inbox.StatusBlocked)
	for _, want := range []string{"❓ *Questions for you* · vague", "*1.* Which repository?\n      a) api\n      b) web\n", "`retry: 1) a, 2) …`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("blocked message missing %q:\n%s", want, msg)
		}
	}
}

// Blocked by the implementer with questions: the reason, then its
// questions to answer with the retry.
func TestRelayBlockedByImplementerListsQuestions(t *testing.T) {
	svc, runsSvc, _, _, relay := relayStack(t)
	ctx := context.Background()
	item, err := svc.CreateItem(ctx, inbox.CreateInput{WorkspaceID: "ws-a", Title: "stock api", AuthorAgentID: "martin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runsSvc.Queue(ctx, runs.QueueInput{WorkspaceID: "ws-a", ItemID: item.ID, Stage: runs.StageImplement}); err != nil {
		t.Fatal(err)
	}
	r, err := runsSvc.ClaimNext(ctx, runs.ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if err != nil || r == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.AppendSection(ctx, item.ID, "Questions — implementer", "1. Which scope?\n   - (a) sales\n   - (b) quotes\n", audit.ViaWorker); err != nil {
		t.Fatal(err)
	}
	if _, err := runsSvc.Report(ctx, r.ID, runs.ReportInput{WorkerID: "w1", Status: runs.StatusFailed, Reason: "the API scope is undecided"}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	msg := relay.formatGate(item.ID, inbox.StatusImplementing, inbox.StatusBlocked)
	for _, want := range []string{"*Blocked* · stock api", "the API scope is undecided", "*the implementer asks:*", "*1.* Which scope?\n      a) sales\n      b) quotes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("blocked message missing %q:\n%s", want, msg)
		}
	}
}

// `answer:` records answers without moving the item and says what it
// matched; an approve whose notes answer a question says so too.
func TestRelayAnswerCommand(t *testing.T) {
	svc, _, plansSvc, fake, relay := relayStack(t)
	root := context.Background()
	item := mustReachGate(t, svc, plansSvc, "answers")
	qaSvc := relayQA(t, svc)
	if _, err := qaSvc.Ask(root, qa.AskInput{WorkspaceID: item.WorkspaceID, ItemID: item.ID, Asker: qa.AskerPlanner,
		Questions: []itemtext.Question{{Text: "Which backend?", Options: []string{"Redis", "in-process"}}, {Text: "Keep the flag?"}}}); err != nil {
		t.Fatal(err)
	}
	if !relay.Apply(root, item.ID, "answer: 1) b") {
		t.Fatal("answer must be handled")
	}
	if got, _ := svc.GetItem(root, item.ID); got.Status != inbox.StatusAwaitingApproval {
		t.Fatalf("answer doesn't move the item: %s", got.Status)
	}
	if msg := fake.last().text; !strings.Contains(msg, "Recorded answers to 1) b") {
		t.Fatalf("reply: %q", msg)
	}
	if !relay.Apply(root, item.ID, "approve: 2) yes, keep it") {
		t.Fatal("approve must be handled")
	}
	if msg := fake.last().text; !strings.Contains(msg, "Recorded answers to 2") {
		t.Fatalf("approve reply: %q", msg)
	}
	rec, _ := svc.Questions(root, item.ID)
	sets := rec.Sets
	if sets[0].Questions[1].State != qa.StateAnswered || sets[0].Questions[1].Answers[0].Via != "slack" {
		t.Fatalf("notes answer recorded via slack: %+v", sets[0].Questions[1])
	}
}
