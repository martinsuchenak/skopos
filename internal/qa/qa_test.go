package qa

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	_ "modernc.org/sqlite"
)

type fakeAudit struct{ events []audit.RecordInput }

func (f *fakeAudit) Record(_ context.Context, in audit.RecordInput) error {
	f.events = append(f.events, in)
	return nil
}

func testService(t *testing.T) (*Service, *fakeAudit) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	s := NewService(NewStorage(sqlDB))
	a := &fakeAudit{}
	s.SetAuditRecorder(a)
	return s, a
}

func martin() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Name: "martin", KeyID: "k1", Approver: true})
}

func ask(t *testing.T, s *Service, asker, label string, qs ...itemtext.Question) *Set {
	t.Helper()
	set, err := s.Ask(context.Background(), AskInput{WorkspaceID: "ws", ItemID: "it", RunID: "run-1", Asker: asker, Label: label,
		Stage: "plan", SubjectKind: SubjectPlanRevision, Subject: "rev+base", Questions: qs})
	if err != nil || set == nil {
		t.Fatalf("ask: %v", err)
	}
	return set
}

func stateOf(t *testing.T, s *Service) map[string]string {
	t.Helper()
	sets, err := s.Item(context.Background(), "it")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, st := range sets {
		for _, q := range st.Questions {
			out[q.Text] = q.State
		}
	}
	return out
}

func TestAskRecordsAndAudits(t *testing.T) {
	s, a := testService(t)
	set := ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}}, itemtext.Question{Text: " "}, itemtext.Question{Text: "Keep the flag?"})
	if len(set.Questions) != 3 || set.Questions[1].Text != "(no question)" || set.Questions[2].N != 3 {
		t.Fatalf("numbers hold: %+v", set.Questions)
	}
	if got := Render(*set); got != "1. Which backend?\n   - (a) Redis\n   - (b) in-process\n2. (no question)\n3. Keep the flag?\n" {
		t.Fatalf("render: %q", got)
	}
	if len(a.events) != 1 || a.events[0].Action != "question.asked" || a.events[0].AgentID != "planner (run run-1)" {
		t.Fatalf("audit: %+v", a.events)
	}
	if set, err := s.Ask(context.Background(), AskInput{WorkspaceID: "ws", ItemID: "it", Asker: AskerPlanner}); err != nil || set != nil {
		t.Fatalf("no questions, no set: %+v %v", set, err)
	}
}

// Free text: matched lines link to questions (options by letter, the
// dashboard's "Answer:" rendering, a label prefix with several sets); the
// rest is a general answer; a gate skips what stays open.
func TestPrepareFreeText(t *testing.T) {
	s, a := testService(t)
	ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}},
		itemtext.Question{Text: "Keep the flag?"}, itemtext.Question{Text: "Which scope?", Options: []string{"sales", "quotes"}})
	text := "1) b — fingerprint check is enough\n2. Keep the flag?\n   Answer: yes, for one release\nalso: drop the docs"
	p, err := s.Prepare(martin(), "it", "ws", ActionApprove, AnswersInput{Text: text}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(martin(), p, "appr-1", "slack"); err != nil {
		t.Fatal(err)
	}
	got := stateOf(t, s)
	// Question 3 has no linked answer, but the set got a general answer.
	if got["Which backend?"] != StateAnswered || got["Keep the flag?"] != StateAnswered || got["Which scope?"] != StateAnsweredFreeText {
		t.Fatalf("states: %v", got)
	}
	if !strings.Contains(p.Markdown, "1. Which backend?\n   Answer: (b) in-process — fingerprint check is enough") ||
		!strings.Contains(p.Markdown, "2. Keep the flag?\n   Answer: yes, for one release") || !strings.Contains(p.Markdown, "also: drop the docs") {
		t.Fatalf("markdown:\n%s", p.Markdown)
	}
	sets, _ := s.Item(context.Background(), "it")
	q1 := sets[0].Questions[0].Answers[0]
	if q1.Option != "b" || q1.Actor != "key martin" || q1.ActorKeyID != "k1" || q1.Via != "slack" || q1.ApprovalID != "appr-1" || q1.Action != ActionApprove {
		t.Fatalf("answer row: %+v", q1)
	}
	if len(sets[0].General) != 1 || sets[0].General[0].Text != "also: drop the docs" {
		t.Fatalf("general: %+v", sets[0].General)
	}
	if n := countActions(a, "question.answered"); n != 3 {
		t.Fatalf("one event per answer row: %d (%+v)", n, a.events)
	}
}

func countActions(a *fakeAudit, action string) int {
	n := 0
	for _, e := range a.events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// Structured answers by id; a gate records what it leaves open as skipped;
// answering a closed question is refused.
func TestPrepareStructuredAndSkipped(t *testing.T) {
	s, a := testService(t)
	set := ask(t, s, AskerImplementer, "", itemtext.Question{Text: "New scope?", Options: []string{"sales", "quotes"}}, itemtext.Question{Text: "Delete permission?"})
	items := []AnswerItem{{QuestionID: set.Questions[0].ID, Option: "a"}}
	if _, err := s.Prepare(martin(), "it", "ws", ActionRetry, AnswersInput{Items: []AnswerItem{{QuestionID: set.Questions[0].ID, Option: "z"}}}, true); err == nil {
		t.Fatal("an option the question doesn't have")
	}
	p, err := s.Prepare(martin(), "it", "ws", ActionRetry, AnswersInput{Items: items}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(martin(), p, "", "dashboard"); err != nil {
		t.Fatal(err)
	}
	got := stateOf(t, s)
	if got["New scope?"] != StateAnswered || got["Delete permission?"] != StateSkipped {
		t.Fatalf("states: %v", got)
	}
	if countActions(a, "question.skipped") != 1 {
		t.Fatalf("skipped audited: %+v", a.events)
	}
	if _, err := s.Prepare(martin(), "it", "ws", ActionAnswer, AnswersInput{Items: items}, false); err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("an answered question can't be answered again: %v", err)
	}
	// JSON: a string or a list.
	var in AnswersInput
	if err := json.Unmarshal([]byte(`[{"question_id":"q","option":"a"}]`), &in); err != nil || len(in.Items) != 1 {
		t.Fatalf("list: %+v %v", in, err)
	}
	if err := json.Unmarshal([]byte(`"1) a"`), &in); err != nil || in.Text != "1) a" {
		t.Fatalf("string: %+v %v", in, err)
	}
}

// A later set from the same asker and label supersedes; another label
// (a plan group's other candidate) doesn't. With several open sets a bare
// number links nothing; a label picks the set.
func TestSupersedeAndLabels(t *testing.T) {
	s, _ := testService(t)
	ask(t, s, AskerPlanner, "a@claude", itemtext.Question{Text: "A1?"})
	ask(t, s, AskerPlanner, "b@codex", itemtext.Question{Text: "B1?", Options: []string{"x", "y"}})
	p, err := s.Prepare(martin(), "it", "ws", ActionApprove, AnswersInput{Text: "1) bare\nb@codex 1) y"}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(martin(), p, "", "cli")
	got := stateOf(t, s)
	if got["B1?"] != StateAnswered || got["A1?"] != StateAnsweredFreeText {
		t.Fatalf("label picks the set, the bare line is general to both: %v", got)
	}
	ask(t, s, AskerPlanner, "a@claude", itemtext.Question{Text: "A2?"})
	ask(t, s, AskerPlanner, "a@claude", itemtext.Question{Text: "A3?"})
	got = stateOf(t, s)
	if got["A2?"] != StateSuperseded || got["A3?"] != StateOpen {
		t.Fatalf("supersede by the same label: %v", got)
	}
}

// The import reads an item's history once: questions, Answers (linked and
// general), notes lines that match, and skipped questions of passed rounds.
func TestImportItem(t *testing.T) {
	s, _ := testService(t)
	content := "Brief.\n\n## Enrichment — skopos worker\n\nP1\n\n## Questions — planner\n\n1. Pilot?\n   - (a) Finance\n   - (b) Setup\n2. Sync on deploy?\n\n" +
		"## Review notes\n\n1) a\nwhy sync at all?\n\n## Enrichment — skopos worker\n\nP2\n\n## Questions — planner\n\n1. Cache?\n2. Deps?\n\n" +
		"## Answers\n\n1. Cache?\n   Answer: Redis\n\n## Approval notes\n\ngo\n\n## Amendment proposed — worker\n\n- r\n\n## Questions — implementer\n\n1. Scope?\n"
	created := time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC)
	ok, err := s.ImportItem(context.Background(), "it", "ws", content, created)
	if err != nil || !ok {
		t.Fatalf("import: %v %v", ok, err)
	}
	got := stateOf(t, s)
	want := map[string]string{"Pilot?": StateAnswered, "Sync on deploy?": StateSkipped, "Cache?": StateAnswered, "Deps?": StateSkipped, "Scope?": StateOpen}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	sets, _ := s.Item(context.Background(), "it")
	if sets[2].Asker != AskerImplementer || sets[0].Source != SourceImported || sets[0].Questions[0].Answers[0].Actor != "migration" {
		t.Fatalf("sets: %+v", sets)
	}
	if again, err := s.ImportItem(context.Background(), "it", "ws", content, created); err != nil || again {
		t.Fatalf("once per item: %v %v", again, err)
	}
}

// Notes lines that answer open questions are answers, not skipped; a
// restated question isn't an answer; an answer by id wins over the same
// question in the text.
func TestNotesRestatedAndDuplicates(t *testing.T) {
	s, _ := testService(t)
	set := ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}},
		itemtext.Question{Text: "Keep the flag?"}, itemtext.Question{Text: "Which scope?"})
	p, err := s.Prepare(martin(), "it", "ws", ActionApprove, AnswersInput{
		Items: []AnswerItem{{QuestionID: set.Questions[0].ID, Option: "a"}},
		Text:  "1) b\n3. Which scope?",
		Notes: "2) yes, for one release\nship it",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(martin(), p, "", "slack"); err != nil {
		t.Fatal(err)
	}
	sets, _ := s.Item(context.Background(), "it")
	qs := sets[0].Questions
	if len(qs[0].Answers) != 1 || qs[0].Answers[0].Option != "a" {
		t.Fatalf("the answer by id wins: %+v", qs[0].Answers)
	}
	if qs[1].State != StateAnswered || qs[1].Answers[0].Text != "yes, for one release" {
		t.Fatalf("a notes line answers: %+v", qs[1])
	}
	// "3. Which scope?" restates the question: a general answer, not an
	// answer to 3; "ship it" in the notes stays notes.
	if qs[2].State != StateAnsweredFreeText || len(sets[0].General) != 1 || strings.Contains(sets[0].General[0].Text, "ship it") {
		t.Fatalf("restated / notes: %+v general %+v", qs[2], sets[0].General)
	}
}
