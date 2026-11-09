package inbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
)

// Answers to the planner's questions get their own "## Answers" section,
// ahead of the notes, on approve and on request-changes: later prompts read
// them with the questions (worker questionsAndAnswers).
func TestAnswersAppendedBeforeNotes(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	item := mustReadyForApproval(t, svc, plansSvc, "approve with answers")
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{Answers: qa.AnswersInput{Text: "1. Keep it?\n   Answer: (b) no"}, Notes: "ship it", Via: audit.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if !strings.HasSuffix(got.Content, "## Answers\n\n1. Keep it?\n   Answer: (b) no\n\n\n## Approval notes\n\nship it\n") {
		t.Fatalf("approve: %q", got.Content)
	}

	item2 := mustReadyForApproval(t, svc, plansSvc, "changes with answers only")
	if _, err := svc.RequestChangesWith(approverCtx(), item2.ID, ChangesInput{Answers: qa.AnswersInput{Text: "1. Which cache?\n   Answer: Redis"}, Via: audit.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetItem(root, item2.ID)
	if got.Status != StatusPlanning || !strings.HasSuffix(got.Content, "## Answers\n\n1. Which cache?\n   Answer: Redis\n") || strings.Contains(got.Content, "## Review notes") {
		t.Fatalf("request changes: %s %q", got.Status, got.Content)
	}
}

// The detail view's content: Martin's text rendered alone, the sections in
// rounds, a questions section with its questions parsed.
func TestContentParts(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "parts")
	for _, s := range [][2]string{
		{"Enrichment — skopos worker", "PLAN"},
		{"Questions — planner", "1. Keep it?\n   - (a) yes\n   - (b) no\n"},
	} {
		if err := svc.AppendSection(root, item.ID, s[0], s[1], audit.ViaWorker); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.GetItem(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentParts != nil {
		t.Fatal("GetItem itself leaves the parts out")
	}
	p := ContentPartsOf(got.Content)
	if p == nil || strings.Contains(p.BriefHTML, "PLAN") || len(p.Rounds) != 1 || p.Rounds[0].Closed {
		t.Fatalf("parts: %+v", p)
	}
	r := p.Rounds[0]
	if r.Kind != "plan" || r.N != 1 || len(r.Sections) != 2 || !strings.Contains(r.Sections[0].HTML, "PLAN") {
		t.Fatalf("round: %+v", r)
	}
	q := r.Sections[1].Questions
	if len(q) != 1 || q[0].Text != "Keep it?" || len(q[0].Options) != 2 || q[0].Options[1] != "no" {
		t.Fatalf("questions: %+v", q)
	}
}

// qaStack is fullStack with the question record wired, as serve.go does.
func qaStack(t *testing.T) (*Service, *plans.Service, *qa.Service) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	auditSvc := audit.NewService(audit.NewStorage(sqlDB))
	plansSvc := plans.NewService(plans.NewStorage(sqlDB))
	svc := NewService(NewStorage(sqlDB))
	svc.SetAuditRecorder(auditSvc)
	svc.SetApprovalLog(approvals.NewService(approvals.NewStorage(sqlDB)))
	svc.SetPlanRevisions(plansSvc)
	plansSvc.SetAmendmentGuard(svc.RequireAmendable)
	plansSvc.SetPlannableGuard(svc.RequirePlannable)
	plansSvc.SetDeletionGuard(svc.PlanHasWorkflowItem)
	qaSvc := qa.NewService(qa.NewStorage(sqlDB))
	qaSvc.SetAuditRecorder(auditSvc)
	svc.SetQuestionLog(qaSvc)
	return svc, plansSvc, qaSvc
}

func askOn(t *testing.T, qaSvc *qa.Service, item *Item, asker string, qs ...itemtext.Question) *qa.Set {
	t.Helper()
	set, err := qaSvc.Ask(context.Background(), qa.AskInput{WorkspaceID: item.WorkspaceID, ItemID: item.ID, Asker: asker, Questions: qs})
	if err != nil || set == nil {
		t.Fatalf("ask: %v", err)
	}
	return set
}

// Approve with free text: matched lines are linked answers, the rest a
// general answer, the Answers section is rendered from them, the response
// says what was matched, and the record names the approval and via.
func TestApproveRecordsAnswers(t *testing.T) {
	svc, plansSvc, _ := qaStack(t)
	ctx := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "record")
	qaSvc := svc.questions.(*qa.Service)
	askOn(t, qaSvc, item, qa.AskerPlanner, itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}}, itemtext.Question{Text: "Keep the flag?"})
	got, err := svc.Approve(approverCtx(), item.ID, ApproveInput{Answers: qa.AnswersInput{Text: "1) a\nkeep it simple"}, Notes: "ship it", Via: audit.ViaDashboard})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AnswersMatched) != 2 || got.AnswersMatched[0].N != 1 || got.AnswersMatched[0].Option != "a" || !got.AnswersMatched[1].General {
		t.Fatalf("matched: %+v", got.AnswersMatched)
	}
	after, _ := svc.GetItem(ctx, item.ID)
	if !strings.Contains(after.Content, "## Answers\n\n1. Which backend?\n   Answer: (a) Redis\nkeep it simple\n\n\n## Approval notes\n\nship it") {
		t.Fatalf("content:\n%s", after.Content)
	}
	rec, err := svc.Questions(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	sets := rec.Sets
	q1 := sets[0].Questions[0]
	if q1.State != qa.StateAnswered || q1.Answers[0].ApprovalID == "" || q1.Answers[0].Via != audit.ViaDashboard || sets[0].Questions[1].State != qa.StateAnsweredFreeText {
		t.Fatalf("record: %+v", sets)
	}
}

// Request changes with answers by id; a refused answer leaves the item at
// the gate; questions the gate passes unanswered are skipped.
func TestRequestChangesStructuredAndSkipped(t *testing.T) {
	svc, plansSvc, qaSvc := qaStack(t)
	ctx := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "structured")
	set := askOn(t, qaSvc, item, qa.AskerPlanner, itemtext.Question{Text: "Scope?", Options: []string{"sales", "quotes"}}, itemtext.Question{Text: "Delete permission?"})
	if _, err := svc.RequestChangesWith(approverCtx(), item.ID, ChangesInput{Answers: qa.AnswersInput{Items: []qa.AnswerItem{{QuestionID: "nope", Option: "a"}}}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an unknown question is refused: %v", err)
	}
	if cur, _ := svc.GetItem(ctx, item.ID); cur.Status != StatusAwaitingApproval {
		t.Fatalf("a refused answer leaves the item at the gate: %s", cur.Status)
	}
	if _, err := svc.RequestChangesWith(approverCtx(), item.ID, ChangesInput{Notes: "split step 2",
		Answers: qa.AnswersInput{Items: []qa.AnswerItem{{QuestionID: set.Questions[0].ID, Option: "b", Text: "quotes only"}}}}); err != nil {
		t.Fatal(err)
	}
	rec, _ := svc.Questions(ctx, item.ID)
	sets := rec.Sets
	if sets[0].Questions[0].State != qa.StateAnswered || sets[0].Questions[1].State != qa.StateSkipped {
		t.Fatalf("states: %+v", sets[0].Questions)
	}
	after, _ := svc.GetItem(ctx, item.ID)
	if !strings.Contains(after.Content, "1. Scope?\n   Answer: (b) quotes — quotes only") {
		t.Fatalf("content:\n%s", after.Content)
	}
}

// An older worker appends a questions section by editing the item: the
// edit records it.
func TestAppendedQuestionsRecorded(t *testing.T) {
	svc, plansSvc, _ := qaStack(t)
	ctx := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "old worker")
	cur, _ := svc.GetItem(ctx, item.ID)
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{Content: cur.Content + "\n\n## Questions — implementer\n\n1. Which scope?\n   - (a) sales\n"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := svc.Questions(ctx, item.ID)
	sets := rec.Sets
	if len(sets) != 1 || sets[0].Asker != qa.AskerImplementer || sets[0].Questions[0].Options[0] != "sales" {
		t.Fatalf("recorded: %+v", sets)
	}
}

// Answers on their own: recorded while the item waits, the item stays
// where it is; refused when it doesn't wait or without approver rights.
func TestAnswerWithoutMoving(t *testing.T) {
	svc, plansSvc, qaSvc := qaStack(t)
	ctx := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "answer only")
	askOn(t, qaSvc, item, qa.AskerPlanner, itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}})
	plainKey := auth.WithPrincipal(ctx, &auth.Principal{Name: "agent", KeyID: "k-agent"})
	if _, err := svc.Answer(plainKey, item.ID, qa.AnswersInput{Text: "1) a"}, audit.ViaAPI); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("answering needs the approver permission: %v", err)
	}
	got, err := svc.Answer(approverCtx(), item.ID, qa.AnswersInput{Text: "1) b — for now"}, audit.ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAwaitingApproval || len(got.AnswersMatched) != 1 || !strings.Contains(got.Content, "## Answers\n\n1. Which backend?\n   Answer: (b) in-process — for now") {
		t.Fatalf("answered, not moved: %s %+v\n%s", got.Status, got.AnswersMatched, got.Content)
	}
	rec, _ := svc.Questions(ctx, item.ID)
	sets := rec.Sets
	if a := sets[0].Questions[0].Answers[0]; a.Action != qa.ActionAnswer || a.Via != audit.ViaAPI {
		t.Fatalf("record: %+v", a)
	}
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Answer(approverCtx(), item.ID, qa.AnswersInput{Text: "1) a"}, audit.ViaAPI); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an approved item doesn't wait for answers: %v", err)
	}
}
