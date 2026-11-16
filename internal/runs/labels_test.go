package runs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/labels"
)

// needsItem writes an inbox item that needs the labels: the claim reads an
// item's needs with its runs.
func needsItem(t *testing.T, s *Service, id string, needs ...string) {
	t.Helper()
	if _, err := s.storage.db.Exec(`INSERT INTO inbox_items (id, workspace_id, title, author_agent_id, created_at, updated_at, needs)
		VALUES (?, 'ws', ?, 'test', '2026-11-09T09:00:00.000000000Z', '2026-11-09T09:00:00.000000000Z', ?)
		ON CONFLICT(id) DO UPDATE SET needs = excluded.needs`, id, id, labels.JSON(needs)); err != nil {
		t.Fatal(err)
	}
}

func labelWorker(t *testing.T, s *Service, id string, list ...string) {
	t.Helper()
	if _, err := s.SetWorkerLabels(rootCtx(), id, LabelsInput{Labels: list}); err != nil {
		t.Fatalf("label %s: %v", id, err)
	}
}

func claimWS(t *testing.T, s *Service, worker string) *Run {
	t.Helper()
	r, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}})
	if err != nil {
		t.Fatalf("claim by %s: %v", worker, err)
	}
	return r
}

// A run of an item that needs labels goes only to a worker with all of
// them; an item that needs none goes to anyone. A poll never changes a
// worker's labels.
func TestRunWithNeedsGoesOnlyToALabelledWorker(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	claimWS(t, s, "w-plain")
	claimWS(t, s, "w-stock")
	labelWorker(t, s, "w-stock", "Stock")

	needsItem(t, s, "item-1", "stock")
	stock := mustQueue(t, s, ctx, "ws", "item-1", StagePlan)
	if got, _ := s.Get(ctx, stock.ID); !slices.Equal(got.Needs, []string{"stock"}) {
		t.Fatalf("the run carries its item's needs: %+v", got.Needs)
	}
	if r := claimWS(t, s, "w-plain"); r != nil {
		t.Fatalf("a worker without the label must not take it: %+v", r)
	}
	if r := claimWS(t, s, "w-new"); r != nil { // its first poll: no row, no labels
		t.Fatalf("an unknown worker must not take it: %+v", r)
	}
	r := claimWS(t, s, "w-stock")
	if r == nil || r.ID != stock.ID || !slices.Equal(r.Needs, []string{"stock"}) {
		t.Fatalf("the labelled worker takes it: %+v", r)
	}
	if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w-stock", Status: StatusSucceeded}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range all {
		want := []string(nil)
		if w.ID == "w-stock" {
			want = []string{"stock"}
		}
		if !slices.Equal(w.Labels, want) {
			t.Errorf("%s: labels %v after its polls, want %v", w.ID, w.Labels, want)
		}
	}

	// Every label is needed, in every stage; a second opinion too.
	needsItem(t, s, "item-2", "plant-2", "stock")
	both := mustQueue(t, s, ctx, "ws", "item-2", StageImplement)
	if r := claimWS(t, s, "w-stock"); r != nil {
		t.Fatalf("one of two labels is not enough: %+v", r)
	}
	labelWorker(t, s, "w-stock", "stock", "plant-2", "spare")
	if r := claimWS(t, s, "w-stock"); r == nil || r.ID != both.ID {
		t.Fatalf("with both labels: %+v", r)
	}
	opinion, err := s.QueueSecondOpinion(ctx, "item-1", "ws", "codex", "abc123", "b", "base")
	if err != nil {
		t.Fatal(err)
	}
	withCodex := func(worker string) *Run {
		t.Helper()
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}, Runners: []string{"codex"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := withCodex("w-plain"); r != nil {
		t.Fatalf("a second opinion follows the item's needs: %+v", r)
	}

	// The item's needs are read with the run: lifting them frees a run
	// that already waits.
	needsItem(t, s, "item-1")
	if r := withCodex("w-plain"); r == nil || r.ID != opinion {
		t.Fatalf("needs lifted: %+v", r)
	}

	// An item that needs nothing goes to anyone.
	free := mustQueue(t, s, ctx, "ws", "item-3", StagePlan)
	if r := claimWS(t, s, "w-new"); r == nil || r.ID != free.ID {
		t.Fatalf("no needs: %+v", r)
	}
}

// A worker that lost a label does not get its own interrupted run back:
// the run is released for a labelled worker to take over from the branch.
func TestOwnerWithoutTheLabelGivesTheRunUp(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	claimWS(t, s, "w1")
	claimWS(t, s, "w2")
	labelWorker(t, s, "w1", "stock")
	needsItem(t, s, "item-1", "stock")
	queued := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if r := claimWS(t, s, "w1"); r == nil || r.ID != queued.ID {
		t.Fatalf("claim: %+v", r)
	}
	// w1 restarts mid-run; meanwhile the label moved to w2.
	labelWorker(t, s, "w1")
	if r := claimWS(t, s, "w1"); r != nil {
		t.Fatalf("w1 no longer has the label: %+v", r)
	}
	got, err := s.Get(ctx, queued.ID)
	if err != nil || got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("released for a takeover: %+v %v", got, err)
	}
	if r := claimWS(t, s, "w2"); r != nil {
		t.Fatalf("w2 has no label yet: %+v", r)
	}
	labelWorker(t, s, "w2", "stock")
	if r := claimWS(t, s, "w2"); r == nil || r.ID != queued.ID || r.Attempt != 2 {
		t.Fatalf("w2 takes over: %+v", r)
	}
}

// A retry does not wait for the worker that ran the stage before when that
// worker no longer has the labels the item needs.
func TestRetryDoesNotWaitForAWorkerWithoutTheLabel(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	claimWS(t, s, "w1")
	claimWS(t, s, "w2")
	labelWorker(t, s, "w1", "stock")
	labelWorker(t, s, "w2", "stock")
	needsItem(t, s, "item-1", "stock")
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	r := claimWS(t, s, "w1")
	if r == nil {
		t.Fatal("w1 claims")
	}
	if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "x"}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	again := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if again.PreferWorker != "w1" {
		t.Fatalf("the retry prefers w1: %+v", again)
	}
	if r := claimWS(t, s, "w2"); r != nil {
		t.Fatalf("w1 is online and labelled: w2 waits, got %+v", r)
	}
	labelWorker(t, s, "w1")
	if r := claimWS(t, s, "w2"); r == nil || r.ID != again.ID {
		t.Fatalf("w1 lost the label: w2 takes it at once: %+v", r)
	}
}

// A waiting run no worker serving the workspace has the labels for is
// marked stranded with the missing labels, and the mark goes when a worker
// gets them.
func TestStrandedForLabels(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	claimWS(t, s, "w1")
	claimWS(t, s, "w2")
	labelWorker(t, s, "w1", "plant-2")
	needsItem(t, s, "item-1", "plant-2", "stock")
	run := mustQueue(t, s, ctx, "ws", "item-1", StagePlan)
	free := mustQueue(t, s, ctx, "ws", "item-2", StagePlan)
	stranded := func(id string) string {
		t.Helper()
		r, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Stranded
	}
	now = now.Add(strandedAfter + time.Second)
	s.SweepStranded(ctx)
	s.SweepStranded(ctx)
	if got := stranded(run.ID); got != "no worker serving the workspace has the label the item needs: stock" {
		t.Fatalf("stranded reason: %q", got)
	}
	if got := stranded(free.ID); got != "" {
		t.Fatalf("an item without needs is not stranded: %q", got)
	}
	// Each label is on some worker, on no worker both.
	labelWorker(t, s, "w2", "stock")
	s.SweepStranded(ctx)
	if got := stranded(run.ID); got != "no worker serving the workspace has all the labels the item needs: plant-2, stock" {
		t.Fatalf("stranded reason: %q", got)
	}
	labelWorker(t, s, "w2", "stock", "plant-2")
	s.SweepStranded(ctx)
	if got := stranded(run.ID); got != "" {
		t.Fatalf("a worker has the labels now: %q", got)
	}
	n := 0
	for _, e := range spy.got {
		if e.Action == "run.stranded" {
			n++
			if e.EntityID != run.ID || !strings.Contains(e.Notes, "the item needs") {
				t.Errorf("audit: %+v", e)
			}
		}
	}
	if n != 2 {
		t.Fatalf("recorded when marked and when the reason changed, got %d", n)
	}
}

// A workspace says which stages an item's needed labels bind: in the
// others its runs go to any worker. Without the setting they bind every
// stage.
func TestNeedsBindOnlyTheStagesTheWorkspaceNames(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{}, NeedsStages: []string{StageImplement, StageQA}}})
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	claimWS(t, s, "w-plain")
	for i, stage := range []string{StagePlan, StageImplement, StageReview, StageQA} {
		item := "item-" + string(rune('1'+i))
		needsItem(t, s, item, "stock")
		mustQueue(t, s, ctx, "ws", item, stage)
	}
	opinion, err := s.QueueSecondOpinion(ctx, "item-1", "ws", "codex", "abc123", "b", "base")
	if err != nil {
		t.Fatal(err)
	}
	take := func(worker string, runners ...string) string {
		t.Helper()
		var got []string
		for {
			r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}, Runners: runners})
			if err != nil {
				t.Fatal(err)
			}
			if r == nil {
				break
			}
			got = append(got, r.Stage)
			if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: worker, Status: StatusSucceeded}, r.LeaseToken); err != nil {
				t.Fatal(err)
			}
		}
		slices.Sort(got)
		return strings.Join(got, ",")
	}
	// No labelled worker yet: the stages that only read the repository are
	// worked all the same, and only the bound ones are stranded.
	if got := take("w-plain", "codex"); got != "plan,review,second_opinion" {
		t.Fatalf("an unlabelled worker takes %q, want plan, review and the second opinion", got)
	}
	if r, _ := s.Get(ctx, opinion); r.Status != StatusSucceeded {
		t.Fatalf("the second opinion counts as review: %+v", r)
	}
	now = now.Add(strandedAfter + time.Second)
	s.SweepStranded(ctx)
	waiting, err := s.storage.WaitingRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stranded []string
	for _, r := range waiting {
		if r.Stranded != "no worker serving the workspace has the label the item needs: stock" {
			t.Fatalf("%s: stranded %q", r.Stage, r.Stranded)
		}
		stranded = append(stranded, r.Stage)
	}
	slices.Sort(stranded)
	if got := strings.Join(stranded, ","); got != "implement,qa" {
		t.Fatalf("stranded: %s, want implement and qa", got)
	}
	claimWS(t, s, "w-stock")
	labelWorker(t, s, "w-stock", "stock")
	if got := take("w-stock"); got != "implement,qa" {
		t.Fatalf("the labelled worker takes %q, want implement and qa", got)
	}
}

// Which key each stage counts under, and what binds without a setting.
func TestNeedsStageKeys(t *testing.T) {
	for stage, want := range map[string]string{
		StagePlan: StagePlan, StageSynthesize: StagePlan, StageCompare: StagePlan,
		StageImplement: StageImplement, StageBranchReview: StageImplement,
		StageReview: StageReview, StageSecondOpinion: StageReview, StageQA: StageQA,
	} {
		if got := NeedsStageKey(stage); got != want {
			t.Errorf("%s counts as %s, want %s", stage, got, want)
		}
		if !slices.Contains(NeedsStageKeys, want) {
			t.Errorf("%s is not a key a workspace can name", want)
		}
	}
	var none *WorkspacePrefs
	if !none.NeedsBind(StagePlan) || !(&WorkspacePrefs{}).NeedsBind(StageReview) {
		t.Fatal("without a setting, and without prefs, the labels bind every stage")
	}
	p := &WorkspacePrefs{NeedsStages: []string{StageImplement}}
	if p.NeedsBind(StagePlan) || !p.NeedsBind(StageBranchReview) {
		t.Fatal("implement binds a review of an existing branch, not the plan")
	}
	run := &Run{Stage: StagePlan, Needs: []string{"stock"}}
	if got := boundNeeds(run, p); got != nil {
		t.Fatalf("a plan run requires %v of its worker, want nothing", got)
	}
}

// A worker's labels belong to the key its row was written by: another key
// polling under the same worker id gets no run that needs them.
func TestLabelsAreNotBorrowedByWorkerID(t *testing.T) {
	s := testService(t)
	liveKeys(t, s, "k-owner", "k-other")
	key := func(id string) context.Context {
		return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: id, Name: id, Workspaces: map[string]struct{}{"ws": {}}})
	}
	claim := func(ctx context.Context) *Run {
		t.Helper()
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-stock", Workspaces: []string{"ws"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	claim(key("k-owner"))
	labelWorker(t, s, "w-stock", "stock")
	needsItem(t, s, "item-1", "stock")
	run := mustQueue(t, s, rootCtx(), "ws", "item-1", StagePlan)
	if r := claim(key("k-other")); r != nil {
		t.Fatalf("another key under the labelled worker's id took the run: %+v", r)
	}
	if r := claim(key("k-owner")); r == nil || r.ID != run.ID {
		t.Fatalf("the worker the labels were given to takes it: %+v", r)
	}
}

// Who may label a worker: root, or an approver whose scope covers every
// workspace it serves — not the key it runs with. Labels are normalised,
// and a change is recorded once.
func TestWorkerLabelsAccess(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	worker := scopedCtx("ws-a", "ws-b") // key k1
	if _, err := s.SetWorkerLabels(rootCtx(), "w1", LabelsInput{Labels: []string{"stock"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a worker that never polled: %v", err)
	}
	if r, err := s.ClaimNext(worker, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a", "ws-b"}}); err != nil || r != nil {
		t.Fatal(r, err)
	}
	key := func(id string, approver bool, ws ...string) context.Context {
		set := map[string]struct{}{}
		for _, w := range ws {
			set[w] = struct{}{}
		}
		return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: id, Name: id, Approver: approver, Workspaces: set})
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"root", rootCtx(), nil},
		{"the worker's own key", worker, auth.ErrApproverRequired},
		{"an approver over both workspaces", key("k2", true, "ws-a", "ws-b"), nil},
		{"an approver over one", key("k3", true, "ws-a"), auth.ErrApproverRequired},
		{"a plain key in scope", key("k4", false, "ws-a", "ws-b"), auth.ErrApproverRequired},
		{"a key that can't see it", key("k5", true, "ws-c"), ErrNotFound},
	} {
		_, err := s.SetWorkerLabels(c.ctx, "w1", LabelsInput{Labels: []string{"stock"}})
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	got, err := s.SetWorkerLabels(rootCtx(), "w1", LabelsInput{Labels: []string{" Stock", "plant-2", "stock"}, Via: "cli"})
	if err != nil || !slices.Equal(got, []string{"plant-2", "stock"}) {
		t.Fatalf("normalised: %v %v", got, err)
	}
	for _, bad := range [][]string{{"has space"}, {"-x"}, {"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		if _, err := s.SetWorkerLabels(rootCtx(), "w1", LabelsInput{Labels: bad}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if got, err := s.SetWorkerLabels(rootCtx(), "w1", LabelsInput{}); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("cleared: %v %v", got, err)
	}
	var notes []string
	for _, e := range spy.got {
		if e.Action != "worker.labels" || e.EntityType != "worker" || e.EntityID != "w1" {
			t.Fatalf("audit: %+v", e)
		}
		notes = append(notes, e.Notes)
	}
	// root's first save, the approver's unchanged one (not recorded), the
	// second label, the clearing.
	if want := []string{"stock", "plant-2, stock", "none"}; !slices.Equal(notes, want) {
		t.Fatalf("audit notes %v, want %v", notes, want)
	}
}
