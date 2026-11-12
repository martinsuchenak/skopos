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
	"github.com/martinsuchenak/skopos/internal/events"
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

func alex() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Name: "alex", KeyID: "k1", Approver: true})
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
	p, err := s.Prepare(alex(), "it", "ws", ActionApprove, AnswersInput{Text: text}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(alex(), p, "appr-1", "slack"); err != nil {
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
	if q1.Option != "b" || q1.Actor != "key alex" || q1.ActorKeyID != "k1" || q1.Via != "slack" || q1.ApprovalID != "appr-1" || q1.Action != ActionApprove {
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
	if _, err := s.Prepare(alex(), "it", "ws", ActionRetry, AnswersInput{Items: []AnswerItem{{QuestionID: set.Questions[0].ID, Option: "z"}}}, true); err == nil {
		t.Fatal("an option the question doesn't have")
	}
	p, err := s.Prepare(alex(), "it", "ws", ActionRetry, AnswersInput{Items: items}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(alex(), p, "", "dashboard"); err != nil {
		t.Fatal(err)
	}
	got := stateOf(t, s)
	if got["New scope?"] != StateAnswered || got["Delete permission?"] != StateSkipped {
		t.Fatalf("states: %v", got)
	}
	if countActions(a, "question.skipped") != 1 {
		t.Fatalf("skipped audited: %+v", a.events)
	}
	if _, err := s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: items}, false); err == nil || !strings.Contains(err.Error(), "answered") {
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
	p, err := s.Prepare(alex(), "it", "ws", ActionApprove, AnswersInput{Text: "1) bare\nb@codex 1) y"}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(alex(), p, "", "cli")
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
	p, err := s.Prepare(alex(), "it", "ws", ActionApprove, AnswersInput{
		Items: []AnswerItem{{QuestionID: set.Questions[0].ID, Option: "a"}},
		Text:  "1) b\n3. Which scope?",
		Notes: "2) yes, for one release\nship it",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(alex(), p, "", "slack"); err != nil {
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

// A run's read marks the answers as used; a correction names the answer it
// replaces, becomes the current one, and is flagged when the replaced one
// was used. The prompt text carries the current answers, the skipped
// questions and free text, not open or superseded ones.
func TestCorrectionsAndPrompt(t *testing.T) {
	s, a := testService(t)
	set := ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Which backend?", Options: []string{"Redis", "in-process"}}, itemtext.Question{Text: "Keep the flag?"})
	q1 := set.Questions[0].ID
	p, _ := s.Prepare(alex(), "it", "ws", ActionApprove, AnswersInput{Items: []AnswerItem{{QuestionID: q1, Option: "a"}}}, true)
	_ = s.Commit(alex(), p, "", "dashboard")
	rec, err := s.ForRun(context.Background(), "it", "run-7")
	if err != nil {
		t.Fatal(err)
	}
	sets := rec.Sets
	first := sets[0].Questions[0].Answers[0]
	if first.UsedByRun != "run-7" {
		t.Fatalf("marked: %+v", first)
	}
	prompt := PromptText(rec)
	if !strings.Contains(prompt, "1. Which backend?\n   Answer: (a) Redis") || !strings.Contains(prompt, "2. Keep the flag?\n   (not answered") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if _, err := s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: []AnswerItem{{QuestionID: q1, Option: "b"}}}, false); err == nil || !strings.Contains(err.Error(), "correction") {
		t.Fatalf("answering an answered question needs replaces: %v", err)
	}
	if _, err := s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: []AnswerItem{{QuestionID: q1, Option: "b", Replaces: "nope"}}}, false); err == nil {
		t.Fatal("replaces must name the current answer")
	}
	p, err = s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: []AnswerItem{{QuestionID: q1, Option: "b", Replaces: first.ID}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Markdown, "Corrected answer: (b) in-process") {
		t.Fatalf("markdown: %s", p.Markdown)
	}
	_ = s.Commit(alex(), p, "", "cli")
	sets, _ = s.Item(context.Background(), "it")
	as := sets[0].Questions[0].Answers
	if len(as) != 2 || !as[0].Replaced || as[1].Replaces != first.ID || !as[1].AfterUse || current(sets[0].Questions[0]).Option != "b" {
		t.Fatalf("correction: %+v", as)
	}
	if countActions(a, "question.corrected") != 1 {
		t.Fatalf("audited: %+v", a.events)
	}
	// A skipped question can still be answered later, without replaces.
	p, err = s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Text: "2) yes"}, false)
	if err != nil || len(p.Matches) != 0 || p.Markdown != "2) yes" {
		t.Fatalf("skipped questions aren't open: free text links nothing and is kept as written: %+v %v", p, err)
	}
	q2 := set.Questions[1].ID
	p, err = s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: []AnswerItem{{QuestionID: q2, Text: "yes"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(alex(), p, "", "cli")
	if got := stateOf(t, s); got["Keep the flag?"] != StateAnswered {
		t.Fatalf("a late answer: %v", got)
	}
}

// An answer given when no question is open is a free answer: recorded,
// and carried by the prompt; a set passed entirely unanswered still tells
// the agent so.
func TestFreeAnswersAndSkippedSets(t *testing.T) {
	s, _ := testService(t)
	p, err := s.Prepare(alex(), "it", "ws", ActionRetry, AnswersInput{Text: "use the staging DB for the integration test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(alex(), p, "", "dashboard"); err != nil {
		t.Fatal(err)
	}
	ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Which backend?"})
	p, _ = s.Prepare(alex(), "it", "ws", ActionApprove, AnswersInput{}, true)
	_ = s.Commit(alex(), p, "a1", "dashboard")
	rec, err := s.Record(context.Background(), "it")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Free) != 1 || rec.Free[0].Action != ActionRetry {
		t.Fatalf("free: %+v", rec.Free)
	}
	prompt := PromptText(rec)
	if !strings.Contains(prompt, "The approver's answer with retry (no question open): use the staging DB") || !strings.Contains(prompt, "1. Which backend?\n   (not answered") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}

// The import runs once per item (version 2): an item only with Answers
// sections gets its free answers; an item a version 1 import left without
// them gets them added; an item recorded live is never replayed.
func TestImportVersions(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if ok, err := s.ImportItem(ctx, "only-answers", "ws", "Brief.\n\n## Answers\n\nuse the staging DB\n", created); err != nil || !ok {
		t.Fatalf("import: %v %v", ok, err)
	}
	if rec, _ := s.Record(ctx, "only-answers"); len(rec.Free) != 1 || rec.Free[0].Text != "use the staging DB" {
		t.Fatalf("free: %+v", rec.Free)
	}
	// A version 1 import: the questions recorded, the free answer not, no
	// version mark.
	content := "Brief.\n\n## Questions — planner\n\n1. Cache?\n\n## Answers\n\n1) Redis\n\n## Approval notes\n\ngo\n\n## Answers\n\nretry with more memory\n"
	if _, err := s.Ask(ctx, AskInput{WorkspaceID: "ws", ItemID: "v1", Asker: AskerPlanner, Source: SourceImported, Questions: []itemtext.Question{{Text: "Cache?"}}, At: created}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ImportItem(ctx, "v1", "ws", content, created); err != nil || !ok {
		t.Fatalf("catch-up: %v %v", ok, err)
	}
	rec, _ := s.Record(ctx, "v1")
	if len(rec.Free) != 1 || rec.Free[0].Text != "retry with more memory" {
		t.Fatalf("catch-up free: %+v", rec.Free)
	}
	if ok, _ := s.ImportItem(ctx, "v1", "ws", content, created); ok {
		t.Fatal("once")
	}
	// Recorded live, then a restart: never replayed.
	p, _ := s.Prepare(alex(), "live", "ws", ActionRetry, AnswersInput{Text: "a live answer"}, true)
	_ = s.Commit(alex(), p, "", "dashboard")
	if ok, _ := s.ImportItem(ctx, "live", "ws", "Brief.\n\n## Answers\n\na live answer\n", created); ok {
		t.Fatal("a live item is not replayed")
	}
	if rec, _ := s.Record(ctx, "live"); len(rec.Free) != 1 {
		t.Fatalf("no duplicate: %+v", rec.Free)
	}
}

type fakeHub struct{ events []events.Event }

func (f *fakeHub) Publish(e events.Event) { f.events = append(f.events, e) }

// The decision log across items: a row per question with its state and
// current answer, free answers as rows, filters, the summary per workspace
// and per person, the wait, and the CSV row shape. A live asking publishes
// questions.waiting.
func TestSearchAndSummary(t *testing.T) {
	s, _ := testService(t)
	hub := &fakeHub{}
	s.SetPublisher(hub)
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	now := t0
	s.now = func() time.Time { return now }
	askIn := func(ws, item, asker string, qs ...itemtext.Question) *Set {
		t.Helper()
		set, err := s.Ask(context.Background(), AskInput{WorkspaceID: ws, ItemID: item, Asker: asker, Blocking: true, Questions: qs})
		if err != nil || set == nil {
			t.Fatal(err)
		}
		return set
	}
	a := askIn("ws-a", "it-1", AskerPlanner, itemtext.Question{Text: "Backend?", Options: []string{"Redis", "SQLite"}}, itemtext.Question{Text: "Flag?"})
	if len(hub.events) != 1 || hub.events[0].Type != events.TypeQuestionsWaiting || hub.events[0].Item != "it-1" || hub.events[0].Data["count"] != 2 {
		t.Fatalf("event: %+v", hub.events)
	}
	now = t0.Add(10 * time.Minute)
	p, _ := s.Prepare(alex(), "it-1", "ws-a", ActionApprove, AnswersInput{Items: []AnswerItem{{QuestionID: a.Questions[0].ID, Option: "a"}}}, true)

	_ = s.Commit(alex(), p, "appr-1", "dashboard")
	now = t0.Add(time.Hour)
	askIn("ws-b", "it-2", AskerImplementer, itemtext.Question{Text: "Scope?"})
	p, _ = s.Prepare(auth.WithPrincipal(context.Background(), &auth.Principal{Root: true}), "it-3", "ws-b", ActionRetry, AnswersInput{Text: "more memory"}, true)
	_ = s.Commit(auth.WithPrincipal(context.Background(), &auth.Principal{Root: true}), p, "", "slack")

	rows, err := s.Search(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows: %+v", rows)
	}
	r0 := rows[0]
	if r0.Question != "Backend?" || r0.State != StateAnswered || r0.Option != "a" || r0.OptionText != "Redis" || r0.AnsweredBy != "key alex" ||
		r0.ApprovalID != "appr-1" || r0.WaitedSeconds != 600 {
		t.Fatalf("answered row: %+v", r0)
	}
	if rows[1].State != StateSkipped || rows[1].AnsweredBy != "key alex" {
		t.Fatalf("skipped row: %+v", rows[1])
	}
	if rows[3].Kind != "free" || rows[3].Answer != "more memory" || rows[3].AnsweredBy != "root" {
		t.Fatalf("free row: %+v", rows[3])
	}
	for _, c := range []struct {
		f    Filter
		want int
	}{
		{Filter{WorkspaceID: "ws-b"}, 2},
		{Filter{State: StateOpen}, 1},
		{Filter{State: "free"}, 1},
		{Filter{Asker: AskerImplementer}, 1},
		{Filter{AnsweredBy: "key alex"}, 2},
		{Filter{From: t0.Add(30 * time.Minute)}, 2},
		{Filter{To: t0.Add(30 * time.Minute)}, 2},
		{Filter{ItemID: "it-1"}, 2},
	} {
		got, _ := s.Search(context.Background(), c.f)
		if len(got) != c.want {
			t.Errorf("%+v: %d rows, want %d", c.f, len(got), c.want)
		}
	}
	sum := Summarize(rows)
	if sum.Total.Asked != 3 || sum.Total.Answered != 1 || sum.Total.Skipped != 1 || sum.Total.Open != 1 || sum.Total.FreeAnswers != 1 {
		t.Fatalf("total: %+v", sum.Total)
	}
	if m := sum.ByPerson["key alex"]; m.Answered != 1 || m.Skipped != 1 || m.Asked != 0 || m.MedianWaitSeconds != 600 {
		t.Fatalf("per person: %+v", m)
	}
	if b := sum.ByWorkspace["ws-b"]; b.Asked != 1 || b.Open != 1 || b.FreeAnswers != 1 {
		t.Fatalf("per workspace: %+v", b)
	}
	if len(r0.CSV()) != len(CSVHeader) {
		t.Fatalf("CSV row has %d fields, header %d", len(r0.CSV()), len(CSVHeader))
	}
}

// A question answered in words only (an option list, none picked) reads in
// the decision log and the prompt.
func TestAnsweredInWordsOnly(t *testing.T) {
	s, _ := testService(t)
	set := ask(t, s, AskerPlanner, "", itemtext.Question{Text: "Why?", Options: []string{"x", "y"}})
	p, err := s.Prepare(alex(), "it", "ws", ActionAnswer, AnswersInput{Items: []AnswerItem{{QuestionID: set.Questions[0].ID, Text: "because"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Commit(alex(), p, "", "cli")
	rows, err := s.Search(context.Background(), Filter{})
	if err != nil || len(rows) != 1 || rows[0].Answer != "because" || rows[0].Option != "" || rows[0].OptionText != "" {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	rec, _ := s.Record(context.Background(), "it")
	if got := PromptText(rec); !strings.Contains(got, "1. Why?\n   Answer: because") {
		t.Fatalf("prompt: %q", got)
	}
}

// The CSV export never hands a spreadsheet a formula.
func TestCSVCells(t *testing.T) {
	r := Row{Kind: "question", Question: "=HYPERLINK(\"x\")", Answer: "@sum", OptionText: "-1", ItemTitle: "plain"}
	got := r.CSV()
	for i, h := range CSVHeader {
		switch h {
		case "question":
			if got[i] != "'=HYPERLINK(\"x\")" {
				t.Errorf("question: %q", got[i])
			}
		case "answer":
			if got[i] != "'@sum" {
				t.Errorf("answer: %q", got[i])
			}
		case "option_text":
			if got[i] != "'-1" {
				t.Errorf("option_text: %q", got[i])
			}
		case "item_title":
			if got[i] != "plain" {
				t.Errorf("item_title: %q", got[i])
			}
		}
	}
}
