package inbox

import (
	"context"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
)

// Answers to the planner's questions get their own "## Answers" section,
// ahead of the notes, on approve and on request-changes: later prompts read
// them with the questions (worker questionsAndAnswers).
func TestAnswersAppendedBeforeNotes(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	item := mustReadyForApproval(t, svc, plansSvc, "approve with answers")
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{Answers: "1. Keep it?\n   Answer: (b) no", Notes: "ship it", Via: audit.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if !strings.HasSuffix(got.Content, "## Answers\n\n1. Keep it?\n   Answer: (b) no\n\n\n## Approval notes\n\nship it\n") {
		t.Fatalf("approve: %q", got.Content)
	}

	item2 := mustReadyForApproval(t, svc, plansSvc, "changes with answers only")
	if _, err := svc.RequestChangesWith(approverCtx(), item2.ID, ChangesInput{Answers: "1. Which cache?\n   Answer: Redis", Via: audit.ViaDashboard}); err != nil {
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
