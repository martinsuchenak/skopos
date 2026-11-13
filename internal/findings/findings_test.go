package findings

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testService(t *testing.T) (*Service, *sql.DB) {
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
	// A clock that moves: updates apply in the order they were written.
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { at = at.Add(time.Second); return at }
	return s, sqlDB
}

const item = "item-1"

func run(id, stage, runner, focus string) Run {
	return Run{ID: id, WorkspaceID: "ws", ItemID: item, Stage: stage, Runner: runner, Focus: focus, HeadSHA: "head-" + id}
}

// report writes a run's findings and answers the way the run queue does:
// inside one transaction. The run is alone in its round and did not pass.
func report(t *testing.T, s *Service, sqlDB *sql.DB, r Run, fs []NewFinding, answers []Answer) int {
	t.Helper()
	return reportIn(t, s, sqlDB, r, Round{}, fs, answers, false)
}

// reportIn is report for a member of a round, or a review that passed.
func reportIn(t *testing.T, s *Service, sqlDB *sql.DB, r Run, round Round, fs []NewFinding, answers []Answer, passed bool) int {
	t.Helper()
	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	still, err := s.RecordRun(context.Background(), tx, r, round, fs, answers, passed, s.now())
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return still
}

func states(t *testing.T, s *Service) map[string]string {
	t.Helper()
	fs, err := s.Item(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range fs {
		out[f.ID] = f.State
	}
	return out
}

func idsOf(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return strings.Join(out, ",")
}

// A blocking finding of a gating review is followed from the start: the
// implementer is told to fix it, its claim is only a claim, and the next
// review settles it. A nit is recorded and left alone.
func TestFindingIsFollowedAcrossRounds(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	rev1 := run("rev1", "review", "claude", "")
	report(t, s, sqlDB, rev1, []NewFinding{
		{ID: "f-block", Severity: "blocking", Text: "counts per process", File: "limit.go:41"},
		{ID: "f-nit", Severity: "nit", Text: "rename lim"},
		{ID: "f-odd", Severity: "critical", Text: "an unknown severity blocks"},
		{ID: "f-empty", Severity: "blocking", Text: "   "},
	}, nil)
	if got := states(t, s); got["f-block"] != StateOpen || got["f-nit"] != StateNoted || got["f-odd"] != StateOpen || len(got) != 3 {
		t.Fatalf("after the review: %v", got)
	}

	// The implementer is given what is followed up, not the nit.
	impl := run("impl2", "implement", "claude", "")
	v, err := s.ForRun(ctx, impl, Round{})
	if err != nil {
		t.Fatal(err)
	}
	if idsOf(v.Fix) != "f-block,f-odd" || len(v.Check) != 0 {
		t.Fatalf("to fix: %q check %q", idsOf(v.Fix), idsOf(v.Check))
	}
	// Its answers: fixed, left with a reason, and things the record does
	// not take (a nit nobody sent back, an unknown id, a reviewer's word).
	report(t, s, sqlDB, impl, nil, []Answer{
		{ID: "f-block", Status: "fixed", Note: "moved the counter to the database"},
		{ID: "f-odd", Status: "not_fixed", Note: "out of scope"},
		{ID: "f-nit", Status: "fixed"},
		{ID: "nope", Status: "fixed"},
		{ID: "f-block", Status: "addressed"},
	})
	if got := states(t, s); got["f-block"] != StateFixed || got["f-odd"] != StateOpen || got["f-nit"] != StateNoted {
		t.Fatalf("after the implementer: %v", got)
	}

	// The next review checks both: the claim and the one left open. An
	// implement run started now would get only the one still to fix.
	rev2 := run("rev2", "review", "claude", "")
	v, _ = s.ForRun(ctx, rev2, Round{})
	if idsOf(v.Check) != "f-block,f-odd" {
		t.Fatalf("to check: %q", idsOf(v.Check))
	}
	if v.Check[0].Claim != KindFixed || v.Check[0].ClaimNote != "moved the counter to the database" || v.Check[1].Claim != KindNotFixed {
		t.Fatalf("the reviewer sees the implementer's word: %+v", v.Check)
	}
	if v, _ := s.ForRun(ctx, run("impl3", "implement", "claude", ""), Round{}); idsOf(v.Fix) != "f-odd" {
		t.Fatalf("claimed fixed is not sent to the implementer again: %q", idsOf(v.Fix))
	}
	still := report(t, s, sqlDB, rev2, []NewFinding{{ID: "f-new", Severity: "blocking", Text: "no retry-after header"}}, []Answer{
		{ID: "f-block", Status: "addressed", Note: "the counter is shared"},
		{ID: "f-odd", Status: "still_open", Note: "nothing changed"},
		{ID: "f-odd", Status: "addressed"},  // a second answer for one finding is left out
		{ID: "f-nit", Status: "still_open"}, // not followed up: not this review's to answer
		{ID: "f-new", Status: "addressed"},  // its own new finding
	})
	if still != 1 {
		t.Fatalf("one blocking finding found still open, got %d", still)
	}
	got := states(t, s)
	if got["f-block"] != StateAddressed || got["f-odd"] != StateStillOpen || got["f-new"] != StateOpen || got["f-nit"] != StateNoted {
		t.Fatalf("after the second review: %v", got)
	}
	// Settled findings leave the lists; what a review found still open
	// goes back to the implementer with the reviewer's note.
	v, _ = s.ForRun(ctx, run("impl3", "implement", "claude", ""), Round{})
	if idsOf(v.Fix) != "f-odd,f-new" || v.Fix[0].ReviewNote != "nothing changed" {
		t.Fatalf("to fix in round three: %+v", v.Fix)
	}
	// A late claim on a settled finding changes nothing.
	report(t, s, sqlDB, run("impl3", "implement", "claude", ""), nil, []Answer{{ID: "f-block", Status: "not_fixed"}})
	if got := states(t, s); got["f-block"] != StateAddressed {
		t.Fatalf("an implementer cannot reopen what a review settled: %v", got)
	}
}

// In a panel each reviewer checks its own earlier findings; one whose
// focus is no longer in the panel, and a second opinion's the approver
// sent back, go to the first member. The others see them as context.
func TestPanelAndSentBackFindings(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("sec1", "review", "claude", "security"), []NewFinding{{ID: "f-sec", Severity: "blocking", Text: "injection"}}, nil)
	report(t, s, sqlDB, run("tst1", "review", "glm", "tests"), []NewFinding{{ID: "f-tst", Severity: "blocking", Text: "no test"}}, nil)
	report(t, s, sqlDB, run("perf1", "review", "glm", "performance"), []NewFinding{{ID: "f-perf", Severity: "blocking", Text: "n+1 query"}}, nil)
	report(t, s, sqlDB, run("qa1", "qa", "claude", ""), []NewFinding{{ID: "f-qa", Severity: "blocking", Text: "button does nothing"}}, nil)
	so := run("so1", "second_opinion", "codex", "")
	report(t, s, sqlDB, so, []NewFinding{{ID: "f-so", Severity: "blocking", Text: "race in the counter"}, {ID: "f-so-nit", Severity: "nit", Text: "test sleeps"}}, nil)

	// A second opinion is advisory: noted until the approver sends it back.
	if got := states(t, s); got["f-so"] != StateNoted {
		t.Fatalf("a second opinion's finding is noted: %v", got)
	}
	if err := s.Check(ctx, item, []string{"f-so", "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an id the item does not have is refused: %v", err)
	}
	if _, err := s.SendBack(ctx, item, []string{"f-so", "f-so", " "}, Who{Actor: "alex", Via: "dashboard"}); err != nil {
		t.Fatal(err)
	}
	fs, _ := s.Item(ctx, item)
	for _, f := range fs {
		if f.ID == "f-so" && (f.State != StateOpen || !f.SentBack || !f.Tracked || len(f.Updates) != 1 || f.Updates[0].Actor != "alex") {
			t.Fatalf("sent back once, by the approver: %+v", f)
		}
	}

	// The implementer gets all of it, the advisory nit left out.
	v, _ := s.ForRun(ctx, run("impl2", "implement", "claude", ""), Round{})
	if idsOf(v.Fix) != "f-sec,f-tst,f-perf,f-qa,f-so" {
		t.Fatalf("to fix: %q", idsOf(v.Fix))
	}

	// Round two's panel: security and tests (performance was dropped).
	members := []Member{{RunID: "sec2", Stage: "review", Focus: "security"}, {RunID: "tst2", Stage: "review", Focus: "tests"}}
	first, _ := s.ForRun(ctx, run("sec2", "review", "claude", "security"), Round{Members: members})
	if idsOf(first.Check) != "f-sec,f-perf,f-so" || idsOf(first.Others) != "f-tst,f-qa" {
		t.Fatalf("first member: check %q others %q", idsOf(first.Check), idsOf(first.Others))
	}
	second, _ := s.ForRun(ctx, run("tst2", "review", "glm", "tests"), Round{Members: members})
	if idsOf(second.Check) != "f-tst" || idsOf(second.Others) != "f-sec,f-perf,f-qa,f-so" {
		t.Fatalf("second member: check %q others %q", idsOf(second.Check), idsOf(second.Others))
	}
	// QA checks QA's own and nothing of the review stage's.
	qa, _ := s.ForRun(ctx, run("qa2", "qa", "claude", ""), Round{})
	if idsOf(qa.Check) != "f-qa" {
		t.Fatalf("qa: check %q", idsOf(qa.Check))
	}

	// Another second opinion by the same runner answers its own earlier
	// blocking findings, sent back or not; another runner's are not its.
	again, _ := s.ForRun(ctx, run("so2", "second_opinion", "codex", ""), Round{})
	if idsOf(again.Check) != "f-so" {
		t.Fatalf("the same runner's second opinion: %q", idsOf(again.Check))
	}
	other, _ := s.ForRun(ctx, run("so3", "second_opinion", "glm", ""), Round{})
	if len(other.Check) != 0 || !strings.Contains(idsOf(other.Others), "f-so") {
		t.Fatalf("another runner's second opinion: check %q others %q", idsOf(other.Check), idsOf(other.Others))
	}
}

// The approver's word is final against an agent's: a waived finding leaves
// every list, no run's answer brings it back, and sending it back does.
func TestWaive(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("rev1", "review", "claude", ""), []NewFinding{{ID: "f1", Severity: "blocking", Text: "no rate limit on admin"}}, nil)
	f, err := s.Waive(ctx, item, "f1", "admins are trusted", Who{Actor: "alex", KeyID: "k1", Via: "cli"})
	if err != nil || f.State != StateWaived {
		t.Fatalf("waive: %+v %v", f, err)
	}
	if again, _ := s.Waive(ctx, item, "f1", "twice", Who{Actor: "alex"}); len(again.Updates) != 1 {
		t.Fatalf("waiving twice records it once: %+v", again.Updates)
	}
	if _, err := s.Waive(ctx, item, "nope", "", Who{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown finding: %v", err)
	}
	if _, err := s.Waive(ctx, "other-item", "f1", "", Who{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a finding of another item: %v", err)
	}
	v, _ := s.ForRun(ctx, run("impl2", "implement", "claude", ""), Round{})
	if len(v.Fix) != 0 || idsOf(v.Waived) != "f1" || v.Waived[0].WaiveNote != "admins are trusted" {
		t.Fatalf("a waived finding is not to fix: %+v", v)
	}
	rev2 := run("rev2", "review", "claude", "")
	v, _ = s.ForRun(ctx, rev2, Round{})
	if len(v.Check) != 0 || idsOf(v.Waived) != "f1" {
		t.Fatalf("a waived finding is not checked again: %+v", v)
	}
	if still := report(t, s, sqlDB, rev2, nil, []Answer{{ID: "f1", Status: "still_open"}}); still != 0 {
		t.Fatalf("a review cannot hold a waived finding against the item: %d", still)
	}
	if got := states(t, s); got["f1"] != StateWaived {
		t.Fatalf("still waived: %v", got)
	}
	if _, err := s.SendBack(ctx, item, []string{"f1"}, Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	if got := states(t, s); got["f1"] != StateOpen {
		t.Fatalf("sending it back undoes the waive: %v", got)
	}
}

// Runs that reported before findings were records are taken in once, and
// never followed up by themselves: nothing says whether a later round
// fixed them.
func TestImport(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	ins := func(id, stage, status, findings string) {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO runs (id, workspace_id, item_id, stage, runner, status, head_sha, findings, created_at, updated_at)
			VALUES (?, 'ws', ?, ?, 'claude', ?, 'abc', ?, '2026-10-01T10:00:00.000000000Z', '2026-10-01T10:05:00.000000000Z')`, id, item, stage, status, findings); err != nil {
			t.Fatal(err)
		}
	}
	ins("old-review", "review", "failed", `[{"severity":"blocking","text":"old bug","file":"a.go:1"},{"severity":"nit","text":"old nit"},{"severity":"nit","text":""}]`)
	ins("old-so", "second_opinion", "succeeded", `[{"severity":"blocking","text":"old doubt"}]`)
	ins("old-impl", "implement", "succeeded", `[{"severity":"blocking","text":"not a review"}]`)
	ins("running", "review", "running", `[{"severity":"blocking","text":"not finished"}]`)
	ins("none", "review", "succeeded", `[]`)
	// A run recorded live is left alone.
	ins("live", "review", "failed", `[{"id":"f-live","severity":"blocking","text":"live bug"}]`)
	report(t, s, sqlDB, run("live", "review", "claude", ""), []NewFinding{{ID: "f-live", Severity: "blocking", Text: "live bug"}}, nil)

	runs, n, err := s.Import(ctx)
	if err != nil || runs != 2 || n != 3 {
		t.Fatalf("import: %d runs, %d findings, %v", runs, n, err)
	}
	if runs, n, err := s.Import(ctx); err != nil || runs != 0 || n != 0 {
		t.Fatalf("a second import finds nothing: %d %d %v", runs, n, err)
	}
	fs, _ := s.Item(ctx, item)
	if len(fs) != 4 {
		t.Fatalf("three imported and one live: %d", len(fs))
	}
	for _, f := range fs {
		switch {
		case f.ID == "f-live":
			if f.Source != SourceRun || f.State != StateOpen {
				t.Fatalf("live: %+v", f)
			}
		case f.Source != SourceImported || f.State != StateNoted || f.Tracked:
			t.Fatalf("imported findings are noted, not followed: %+v", f)
		case f.CreatedAt.Format("2006-01-02") != "2026-10-01":
			t.Fatalf("imported findings keep their run's time: %v", f.CreatedAt)
		}
	}
	v, _ := s.ForRun(ctx, run("impl", "implement", "claude", ""), Round{})
	if idsOf(v.Fix) != "f-live" {
		t.Fatalf("an imported finding is not sent to an implementer: %q", idsOf(v.Fix))
	}
	// Imported and untouched, a finding is history (what became of it is
	// not known): not listed as left without a follow-up. Sent back by the
	// approver, it is followed like any other.
	var old string
	for _, f := range fs {
		if f.Earlier() != (f.Source == SourceImported) {
			t.Fatalf("only untouched imported findings are from earlier: %+v", f)
		}
		if f.Source == SourceImported && f.Severity == SeverityBlocking && old == "" {
			old = f.ID
		}
	}
	if _, err := s.SendBack(ctx, item, []string{old}, Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	fs, _ = s.Item(ctx, item)
	for _, f := range fs {
		if f.ID == old && (f.Earlier() || !f.Tracked || f.State != StateOpen) {
			t.Fatalf("sent back, an imported finding is followed: %+v", f)
		}
	}
}

// What an agent writes is bounded, and a stage that is no review records
// no findings.
func TestBounds(t *testing.T) {
	s, sqlDB := testService(t)
	long := strings.Repeat("é", MaxText) // two bytes each
	report(t, s, sqlDB, run("rev1", "review", "claude", ""), []NewFinding{{ID: "f1", Severity: "blocking", Text: long, File: strings.Repeat("x", MaxFile+50)}}, nil)
	report(t, s, sqlDB, run("plan1", "plan", "claude", ""), []NewFinding{{ID: "f2", Severity: "blocking", Text: "a planner has no findings"}}, nil)
	fs, _ := s.Item(context.Background(), item)
	if len(fs) != 1 || len(fs[0].Text) > MaxText || len(fs[0].File) != MaxFile || !strings.HasSuffix(fs[0].Text, "é") {
		t.Fatalf("bounded on a rune boundary: %d findings, text %d bytes, file %d", len(fs), len(fs[0].Text), len(fs[0].File))
	}
	var many []NewFinding
	for i := 0; i < MaxPerRun+20; i++ {
		many = append(many, NewFinding{ID: "m" + strings.Repeat("i", i+1), Severity: "nit", Text: "nit"})
	}
	report(t, s, sqlDB, run("rev2", "review", "claude", ""), many, nil)
	if fs, _ := s.Item(context.Background(), item); len(fs) != 1+MaxPerRun {
		t.Fatalf("one report records at most %d findings: %d", MaxPerRun, len(fs)-1)
	}
}

// What a sibling found in this same round is not an earlier finding: a
// member that reports later is neither asked to check it nor told to leave
// it out, so every member's verdict is its own. Two members that review
// alike (the same focus) do not both answer for an earlier finding: the
// first of them checks it, and the other's answer is not taken.
func TestSameRoundAndSameFocus(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	// Round one: two reviewers without a focus (review=claude,codex).
	r1 := Round{Members: []Member{{RunID: "a1", Stage: "review"}, {RunID: "b1", Stage: "review"}}}
	a1, b1 := run("a1", "review", "claude", ""), run("b1", "review", "codex", "")
	reportIn(t, s, sqlDB, a1, r1, []NewFinding{{ID: "f-x", Severity: "blocking", Text: "nil deref"}}, nil, false)
	v, _ := s.ForRun(ctx, b1, r1)
	if len(v.Check) != 0 || len(v.Others) != 0 {
		t.Fatalf("a sibling's finding of the same round is not shown: check %q others %q", idsOf(v.Check), idsOf(v.Others))
	}
	// B answering for it anyway changes nothing.
	if still := reportIn(t, s, sqlDB, b1, r1, nil, []Answer{{ID: "f-x", Status: "still_open"}}, false); still != 0 {
		t.Fatalf("a sibling's finding cannot be held against the other member: %d", still)
	}

	// Round two: the same two. Only the first checks f-x; the second sees
	// it as another reviewer's, and its answer is left out.
	r2 := Round{Members: []Member{{RunID: "a2", Stage: "review"}, {RunID: "b2", Stage: "review"}}}
	a2, b2 := run("a2", "review", "claude", ""), run("b2", "review", "codex", "")
	if v, _ := s.ForRun(ctx, a2, r2); idsOf(v.Check) != "f-x" {
		t.Fatalf("the first member checks it: %q", idsOf(v.Check))
	}
	if v, _ := s.ForRun(ctx, b2, r2); len(v.Check) != 0 || idsOf(v.Others) != "f-x" {
		t.Fatalf("the second member does not: check %q others %q", idsOf(v.Check), idsOf(v.Others))
	}
	reportIn(t, s, sqlDB, a2, r2, nil, []Answer{{ID: "f-x", Status: "still_open", Note: "still there"}}, false)
	reportIn(t, s, sqlDB, b2, r2, nil, []Answer{{ID: "f-x", Status: "addressed"}}, true)
	if got := states(t, s); got["f-x"] != StateStillOpen {
		t.Fatalf("the member it was not given to cannot settle it, by answer or by passing: %v", got)
	}
}

// A passing review settles what was its to check and it did not answer
// for: it read the change and did not raise it again. A worker from before
// the record never answers, and without this a finding fixed rounds ago
// would be handed to an implementer again.
func TestPassingReviewSettles(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("rev1", "review", "claude", ""), []NewFinding{
		{ID: "f-a", Severity: "blocking", Text: "a"}, {ID: "f-b", Severity: "blocking", Text: "b"},
	}, nil)
	report(t, s, sqlDB, run("qa1", "qa", "claude", ""), []NewFinding{{ID: "f-qa", Severity: "blocking", Text: "qa"}}, nil)
	so := run("so1", "second_opinion", "codex", "")
	report(t, s, sqlDB, so, []NewFinding{{ID: "f-so", Severity: "blocking", Text: "so"}}, nil)

	// A review that fails settles nothing it did not answer.
	reportIn(t, s, sqlDB, run("rev2", "review", "claude", ""), Round{}, []NewFinding{{ID: "f-c", Severity: "blocking", Text: "c"}}, nil, false)
	if got := states(t, s); got["f-a"] != StateOpen || got["f-b"] != StateOpen {
		t.Fatalf("a failing review settles nothing: %v", got)
	}
	// A passing second opinion settles nothing either: it is advisory.
	reportIn(t, s, sqlDB, run("so2", "second_opinion", "glm", ""), Round{}, nil, nil, true)
	if got := states(t, s); got["f-a"] != StateOpen {
		t.Fatalf("a second opinion settles nothing by passing: %v", got)
	}
	// A passing review answers one by name and says nothing of the others:
	// all that were its to check are settled, QA's and the advisory one
	// are not.
	reportIn(t, s, sqlDB, run("rev3", "review", "claude", ""), Round{}, nil, []Answer{{ID: "f-a", Status: "addressed", Note: "fixed in the handler"}}, true)
	got := states(t, s)
	if got["f-a"] != StateAddressed || got["f-b"] != StateAddressed || got["f-c"] != StateAddressed || got["f-qa"] != StateOpen || got["f-so"] != StateNoted {
		t.Fatalf("after the passing review: %v", got)
	}
	fs, _ := s.Item(ctx, item)
	for _, f := range fs {
		last := ""
		if len(f.Updates) > 0 {
			last = f.Updates[len(f.Updates)-1].Note
		}
		if (f.ID == "f-a" && last != "fixed in the handler") || (f.ID == "f-b" && last != NotRaisedAgain) {
			t.Fatalf("%s: the record says how it was settled: %q", f.ID, last)
		}
	}
	if v, _ := s.ForRun(ctx, run("impl", "implement", "claude", ""), Round{}); idsOf(v.Fix) != "f-qa" {
		t.Fatalf("settled findings do not go back to an implementer: %q", idsOf(v.Fix))
	}
	// What it says about them, for its summary.
	answers, _ := s.RunAnswers(ctx, item, "rev3")
	if len(answers) != 3 || answers[0].Finding.ID != "f-a" || answers[0].Kind != KindAddressed {
		t.Fatalf("the run's answers: %+v", answers)
	}
}

// A second opinion answers for its own runner's earlier blocking findings
// round after round, sent back or not, until one is addressed.
func TestSecondOpinionFollowsItsOwn(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("so1", "second_opinion", "codex", ""), []NewFinding{{ID: "f-r", Severity: "blocking", Text: "race"}}, nil)
	so2 := run("so2", "second_opinion", "codex", "")
	if v, _ := s.ForRun(ctx, so2, Round{}); idsOf(v.Check) != "f-r" {
		t.Fatalf("the second one checks it: %q", idsOf(v.Check))
	}
	if still := report(t, s, sqlDB, so2, nil, []Answer{{ID: "f-r", Status: "still_open"}}); still != 1 {
		t.Fatalf("still open counts against the second opinion: %d", still)
	}
	fs, _ := s.Item(ctx, item)
	if fs[0].State != StateStillOpen || fs[0].Tracked {
		t.Fatalf("still open, and still advisory: %+v", fs[0])
	}
	// Nobody else is asked about it: not the implementer, not the review.
	if v, _ := s.ForRun(ctx, run("impl", "implement", "claude", ""), Round{}); len(v.Fix) != 0 {
		t.Fatalf("an advisory finding nobody sent back is not the implementer's: %q", idsOf(v.Fix))
	}
	if v, _ := s.ForRun(ctx, run("rev", "review", "claude", ""), Round{}); len(v.Check) != 0 {
		t.Fatalf("nor the review's: %q", idsOf(v.Check))
	}
	// The third one is still given it, and settles it.
	so3 := run("so3", "second_opinion", "codex", "")
	if v, _ := s.ForRun(ctx, so3, Round{}); idsOf(v.Check) != "f-r" {
		t.Fatalf("the third one checks it again: %q", idsOf(v.Check))
	}
	report(t, s, sqlDB, so3, nil, []Answer{{ID: "f-r", Status: "addressed"}})
	if got := states(t, s); got["f-r"] != StateAddressed {
		t.Fatalf("addressed: %v", got)
	}
	if v, _ := s.ForRun(ctx, run("so4", "second_opinion", "codex", ""), Round{}); len(v.Check) != 0 {
		t.Fatalf("an addressed finding is not checked again: %q", idsOf(v.Check))
	}
	// A review may not answer for a second opinion's finding nobody sent back.
	report(t, s, sqlDB, run("so5", "second_opinion", "codex", ""), []NewFinding{{ID: "f-s", Severity: "blocking", Text: "leak"}}, nil)
	report(t, s, sqlDB, run("rev2", "review", "claude", ""), nil, []Answer{{ID: "f-s", Status: "still_open"}})
	if got := states(t, s); got["f-s"] != StateNoted {
		t.Fatalf("not the review's to answer: %v", got)
	}
}

// Two checks of one commit that disagree leave the finding open; a check
// of a later commit settles it.
func TestSameCommitDisagreement(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("so1", "second_opinion", "codex", ""), []NewFinding{{ID: "f-r", Severity: "blocking", Text: "race"}}, nil)
	if _, err := s.SendBack(ctx, item, []string{"f-r"}, Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	rev := run("rev2", "review", "claude", "")
	rev.HeadSHA = "head-2"
	report(t, s, sqlDB, rev, nil, []Answer{{ID: "f-r", Status: "still_open", Note: "two statements"}})
	so := run("so2", "second_opinion", "codex", "")
	so.HeadSHA = "head-2"
	report(t, s, sqlDB, so, nil, []Answer{{ID: "f-r", Status: "addressed"}})
	if got := states(t, s); got["f-r"] != StateStillOpen {
		t.Fatalf("on the same commit, not fixed stands: %v", got)
	}
	so.ID, so.HeadSHA = "so3", "head-3"
	report(t, s, sqlDB, so, nil, []Answer{{ID: "f-r", Status: "addressed"}})
	if got := states(t, s); got["f-r"] != StateAddressed {
		t.Fatalf("a later commit settles it: %v", got)
	}
}

// A prompt says only what still holds about a finding: an old claim is not
// shown once a review answered it, an old "not fixed" note not once the
// finding was sent back afresh.
func TestStandingNotes(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("rev1", "review", "claude", ""), []NewFinding{{ID: "f1", Severity: "blocking", Text: "bug"}}, nil)
	report(t, s, sqlDB, run("impl2", "implement", "claude", ""), nil, []Answer{{ID: "f1", Status: "fixed", Note: "done"}})
	report(t, s, sqlDB, run("rev2", "review", "claude", ""), nil, []Answer{{ID: "f1", Status: "still_open", Note: "not done"}})
	// Round three's implementer says nothing; the next review is not told
	// the finding is claimed fixed.
	v, _ := s.ForRun(ctx, run("rev3", "review", "claude", ""), Round{})
	if e := v.Check[0]; e.Claim != "" || e.ReviewNote != "not done" {
		t.Fatalf("round two's claim does not stand: %+v", e)
	}
	report(t, s, sqlDB, run("rev3", "review", "claude", ""), nil, []Answer{{ID: "f1", Status: "addressed"}})
	if _, err := s.SendBack(ctx, item, []string{"f1"}, Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	v, _ = s.ForRun(ctx, run("impl4", "implement", "claude", ""), Round{})
	if e := v.Fix[0]; e.ReviewNote != "" || e.Claim != "" {
		t.Fatalf("sent back afresh: the old note does not stand: %+v", e)
	}
	if _, err := s.Waive(ctx, item, "f1", "accepted", Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendBack(ctx, item, []string{"f1"}, Who{Actor: "alex"}); err != nil {
		t.Fatal(err)
	}
	v, _ = s.ForRun(ctx, run("impl5", "implement", "claude", ""), Round{})
	if e := v.Fix[0]; e.WaiveNote != "" {
		t.Fatalf("a waive that was undone is not shown: %+v", e)
	}
}

// Accepting a blocking finding is an exception to what a review asked for:
// it needs a reason. A nit does not.
func TestWaiveNeedsAReason(t *testing.T) {
	s, sqlDB := testService(t)
	ctx := context.Background()
	report(t, s, sqlDB, run("rev0", "review", "claude", ""), []NewFinding{{ID: "f-b", Severity: "blocking", Text: "b"}, {ID: "f-n", Severity: "nit", Text: "n"}}, nil)
	if _, err := s.Waive(ctx, item, "f-b", "  ", Who{Actor: "alex"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a blocking finding waived without a reason: %v", err)
	}
	if got := states(t, s); got["f-b"] != StateOpen {
		t.Fatalf("nothing was recorded: %v", got)
	}
	if f, err := s.Waive(ctx, item, "f-n", "", Who{Actor: "alex"}); err != nil || f.State != StateWaived {
		t.Fatalf("a nit needs no reason: %+v %v", f, err)
	}
	if f, err := s.Waive(ctx, item, "f-b", "tracked in the next item", Who{Actor: "alex"}); err != nil || f.State != StateWaived || f.Updates[0].Note != "tracked in the next item" {
		t.Fatalf("with a reason: %+v %v", f, err)
	}
}

// A check of the same commit after the finding was answered is a new
// check: its "addressed" counts. (The commit need not change between two
// QA runs — what was wrong may have been the system it was tested on.)
// Before, a "not fixed" recorded for a commit kept every later "addressed"
// on that commit out for good, and the finding could never be settled.
func TestALaterCheckOfTheSameCommitCounts(t *testing.T) {
	s, sqlDB := testService(t)
	q1 := run("qa1", "qa", "claude", "")
	q1.HeadSHA = "head-1"
	report(t, s, sqlDB, q1, []NewFinding{{ID: "f-q", Severity: "blocking", Text: "the login fails"}}, nil)
	impl := run("impl2", "implement", "claude", "")
	impl.HeadSHA = "head-1"
	report(t, s, sqlDB, impl, nil, []Answer{{ID: "f-q", Status: "fixed"}})
	q2 := run("qa2", "qa", "claude", "")
	q2.HeadSHA = "head-1"
	report(t, s, sqlDB, q2, nil, []Answer{{ID: "f-q", Status: "still_open", Note: "the test system was down"}})
	if got := states(t, s); got["f-q"] != StateStillOpen {
		t.Fatalf("not fixed on head-1: %v", got)
	}
	impl.ID = "impl3"
	report(t, s, sqlDB, impl, nil, []Answer{{ID: "f-q", Status: "not_fixed", Note: "nothing to change"}})
	q3 := run("qa3", "qa", "claude", "")
	q3.HeadSHA = "head-1"
	report(t, s, sqlDB, q3, nil, []Answer{{ID: "f-q", Status: "addressed"}})
	if got := states(t, s); got["f-q"] != StateAddressed {
		t.Fatalf("a later check of the same commit settles it: %v", got)
	}
}
