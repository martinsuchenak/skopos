package runs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// zaiIsolated: Anthropic anywhere, GLM only where something confines the
// run itself.
func zaiIsolated() *policy.Effective {
	return &policy.Effective{Layers: []policy.Layer{{Source: "group personal", Rev: "rev-x",
		Policy: policy.Policy{Allow: []policy.Rule{{Provider: "anthropic"}, {Provider: "zai", Executor: policy.ExecutorIsolated}}}}}}
}

func claimExec(t *testing.T, s *Service, worker, ws string, executors []policy.ExecutorFact, facts ...policy.Facts) *Run {
	t.Helper()
	in := ClaimInput{WorkerID: worker, Workspaces: []string{ws}, Runners: names(facts...), RunnerFacts: facts}
	if executors != nil {
		in.Executors = map[string][]policy.ExecutorFact{ws: executors}
	}
	run, err := s.ClaimNext(rootCtx(), in)
	if err != nil {
		t.Fatalf("claim by %s: %v", worker, err)
	}
	return run
}

// A rule with `executor` matches on what the claiming worker reports for
// the run's workspace, stage and runner: local and a host container do not
// pass `isolated`, a wrapper does, and a worker that reports no executors
// gets nothing the rule covers. The run records the executor it matched on.
func TestClaimMatchesTheExecutor(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}, Policy: zaiIsolated()}})
	mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)

	if run := claimExec(t, s, "w-none", "ws", nil, fGLM); run != nil {
		t.Fatalf("no executor facts: a rule that needs an executor must not match: %+v", run)
	}
	if run := claimExec(t, s, "w-local", "ws", []policy.ExecutorFact{{Executor: "local"}}, fGLM); run != nil {
		t.Fatalf("local is not isolated: %+v", run)
	}
	if run := claimExec(t, s, "w-host", "ws", []policy.ExecutorFact{{Executor: "host:knot"}}, fGLM); run != nil {
		t.Fatalf("a host container does not keep runs apart: %+v", run)
	}
	// Wrapped for implement only: the review still runs local there.
	if run := claimExec(t, s, "w-impl", "ws", []policy.ExecutorFact{{Executor: "local"}, {Stage: "implement", Executor: "wrapper:bwrap:0123456789ab"}}, fGLM); run != nil {
		t.Fatalf("the executor is the one for the run's stage: %+v", run)
	}
	// Wrapped for another runner in the stage: not this one.
	if run := claimExec(t, s, "w-other", "ws", []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Runner: "codex", Executor: "runner:codex"}}, fGLM); run != nil {
		t.Fatalf("the executor is the one for the run's runner: %+v", run)
	}
	run := claimExec(t, s, "w-wrapped", "ws", []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Runner: "opencode-glm", Executor: "wrapper:bwrap:0123456789ab"}}, fGLM)
	if run == nil || run.Runner != "opencode-glm" {
		t.Fatalf("a wrapped GLM review passes: %+v", run)
	}
	var rec factsRecord
	if err := json.Unmarshal(run.Facts, &rec); err != nil || rec.Executor != "wrapper:bwrap:0123456789ab" || rec.Provider != "zai" {
		t.Fatalf("the run records the executor the claim matched on: %+v %v", rec, err)
	}
}

// A rule without `executor` matches any executor, an unreported one
// included; `contained` accepts a host container; a glob names one wrapper.
func TestExecutorRuleKinds(t *testing.T) {
	for _, c := range []struct {
		name     string
		rule     policy.Rule
		executor []policy.ExecutorFact
		want     bool
	}{
		{"no requirement, nothing reported", policy.Rule{Provider: "zai"}, nil, true},
		{"no requirement, local", policy.Rule{Provider: "zai"}, []policy.ExecutorFact{{Executor: "local"}}, true},
		{"contained accepts a host", policy.Rule{Provider: "zai", Executor: policy.ExecutorContained}, []policy.ExecutorFact{{Executor: "host:knot"}}, true},
		{"contained refuses local", policy.Rule{Provider: "zai", Executor: policy.ExecutorContained}, []policy.ExecutorFact{{Executor: "local"}}, false},
		{"isolated accepts codex's own sandbox", policy.Rule{Provider: "zai", Executor: policy.ExecutorIsolated}, []policy.ExecutorFact{{Executor: "runner:codex"}}, true},
		{"Claude's sandbox settings are not isolation", policy.Rule{Provider: "zai", Executor: policy.ExecutorIsolated}, []policy.ExecutorFact{{Executor: "sandbox:0123456789ab"}}, false},
		{"nor containment", policy.Rule{Provider: "zai", Executor: policy.ExecutorContained}, []policy.ExecutorFact{{Executor: "sandbox:0123456789ab"}}, false},
		{"a rule can name the sandbox", policy.Rule{Provider: "zai", Executor: "sandbox:*"}, []policy.ExecutorFact{{Executor: "sandbox:0123456789ab"}}, true},
		{"a named wrapper", policy.Rule{Provider: "zai", Executor: "wrapper:bwrap:*"}, []policy.ExecutorFact{{Executor: "wrapper:bwrap:aaaaaaaaaaaa"}}, true},
		{"another wrapper", policy.Rule{Provider: "zai", Executor: "wrapper:bwrap:*"}, []policy.ExecutorFact{{Executor: "wrapper:docker:aaaaaaaaaaaa"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testService(t)
			pol := &policy.Effective{Layers: []policy.Layer{{Source: "ws", Rev: "r", Policy: policy.Policy{Allow: []policy.Rule{c.rule}}}}}
			s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}, Policy: pol}})
			mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)
			if got := claimExec(t, s, "w1", "ws", c.executor, fGLM) != nil; got != c.want {
				t.Fatalf("claimed = %v, want %v", got, c.want)
			}
		})
	}
}

// An unpinned run gets the first runner in the worker's order that passes
// with ITS executor: GLM unwrapped is skipped, Claude taken.
func TestServerPickSeesEachRunnersExecutor(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: &policy.Effective{Layers: []policy.Layer{{Source: "ws", Rev: "r",
		Policy: policy.Policy{Allow: []policy.Rule{{Executor: policy.ExecutorIsolated}}}}}}}})
	mustQueue(t, s, rootCtx(), "ws", "item-1", StagePlan)
	executors := []policy.ExecutorFact{{Executor: "local"}, {Stage: "plan", Runner: "claude", Executor: "wrapper:bwrap:0123456789ab"}}
	run := claimExec(t, s, "w1", "ws", executors, fGLM, fClaude)
	if run == nil || run.Runner != "claude" || !run.RunnerPicked {
		t.Fatalf("the pick is the first runner whose executor passes: %+v", run)
	}
}

// What a worker says contains its runs is bounded and stored; a change is
// a worker.facts event; the workers list carries it.
func TestExecutorFactsAreStoredAndAudited(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	events := func() []string {
		var out []string
		for _, e := range spy.got {
			if e.Action == "worker.facts" {
				out = append(out, e.Notes)
			}
		}
		return out
	}
	claim := func(executors map[string][]policy.ExecutorFact) {
		t.Helper()
		if _, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"},
			RunnerFacts: []policy.Facts{fClaude}, Executors: executors}); err != nil {
			t.Fatal(err)
		}
	}
	claim(map[string][]policy.ExecutorFact{
		"ws":    {{Executor: "local"}, {Stage: "Review", Executor: "wrapper:bwrap:0123456789ab"}},
		"other": {{Executor: "host:knot"}}, // not a workspace it offers
	})
	workers, err := s.ListWorkers(rootCtx())
	if err != nil || len(workers) != 1 {
		t.Fatalf("workers: %v %v", workers, err)
	}
	got := workers[0].Executors
	if len(got) != 1 || len(got["ws"]) != 2 || got["ws"][0] != (policy.ExecutorFact{Executor: "local"}) ||
		got["ws"][1] != (policy.ExecutorFact{Stage: "review", Executor: "wrapper:bwrap:0123456789ab"}) {
		t.Fatalf("stored executors (offered workspaces only, stages lower-cased): %+v", got)
	}
	if ev := events(); len(ev) != 1 || !strings.Contains(ev[0], "executors: ws (default=local review=wrapper:bwrap:0123456789ab)") {
		t.Fatalf("the first report is recorded with the executors: %v", ev)
	}
	claim(map[string][]policy.ExecutorFact{"ws": {{Executor: "local"}, {Stage: "review", Executor: "wrapper:bwrap:0123456789ab"}}})
	if ev := events(); len(ev) != 1 {
		t.Fatalf("no event while nothing changes: %v", ev)
	}
	claim(map[string][]policy.ExecutorFact{"ws": {{Executor: "local"}}})
	if ev := events(); len(ev) != 2 || !strings.Contains(ev[1], "was: ") || !strings.Contains(ev[1], "now: ") ||
		!strings.Contains(ev[1], "review=wrapper:bwrap:0123456789ab") || !strings.HasSuffix(ev[1], "executors: ws (default=local)") {
		t.Fatalf("a wrapper that goes away is recorded: %v", ev)
	}
}

// At the report the server compares the executor the worker ran with the
// one the claim matched on. The same: nothing. Different and the policy
// still allows it: recorded, the run stands. Different and the policy does
// not allow it: the run is failed with the reason, never retried, recorded.
func TestReportChecksTheExecutor(t *testing.T) {
	wrapped := []policy.ExecutorFact{{Executor: "wrapper:bwrap:0123456789ab"}}
	for _, c := range []struct {
		name       string
		pol        *policy.Effective
		reported   string
		status     Status
		retryable  bool
		wantStatus Status
		wantAudit  string
		wantReason string
	}{
		{"same executor", zaiIsolated(), "wrapper:bwrap:0123456789ab", StatusSucceeded, false, StatusSucceeded, "", ""},
		{"no executor reported", zaiIsolated(), "", StatusSucceeded, false, StatusSucceeded, "", ""},
		{"another wrapper, still isolated", zaiIsolated(), "wrapper:bwrap:ffffffffffff", StatusSucceeded, false, StatusSucceeded, "the policy allows both", ""},
		{"ran local under a rule that needs isolation", zaiIsolated(), "local", StatusSucceeded, false, StatusFailed,
			"the group personal policy does not allow local there — the run is failed", "ran local, the claim matched wrapper:bwrap:0123456789ab: the group personal policy does not allow local there"},
		{"a retryable failure is not retried", zaiIsolated(), "host:knot", StatusFailed, true, StatusFailed,
			"does not allow host:knot there", "(the run itself failed: push failed)"},
		{"no policy: recorded only", nil, "local", StatusSucceeded, false, StatusSucceeded, "the policy allows both", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testService(t)
			spy := &auditSpy{}
			s.SetAuditRecorder(spy)
			s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}, Policy: c.pol}})
			mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)
			run := claimExec(t, s, "w1", "ws", wrapped, fGLM)
			if run == nil {
				t.Fatal("claim")
			}
			in := ReportInput{WorkerID: "w1", Status: c.status, Summary: "done", Executor: c.reported, Grants: "abcdef012345", Retryable: c.retryable}
			if c.status == StatusFailed {
				in.Reason = "push failed"
			}
			got, err := s.Report(rootCtx(), run.ID, in, run.LeaseToken)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != c.wantStatus || !strings.Contains(got.FailReason, c.wantReason) {
				t.Fatalf("status %s reason %q, want %s with %q", got.Status, got.FailReason, c.wantStatus, c.wantReason)
			}
			if c.reported != "" && (got.Executor != c.reported || got.Grants != "abcdef012345") {
				t.Fatalf("the run keeps what really ran and what it was granted: %q %q", got.Executor, got.Grants)
			}
			var notes []string
			for _, e := range spy.got {
				if e.Action == "run.executor_mismatch" {
					if e.EntityID != run.ID || e.WorkspaceID != "ws" {
						t.Fatalf("mismatch event: %+v", e)
					}
					notes = append(notes, e.Notes)
				}
			}
			switch {
			case c.wantAudit == "" && len(notes) != 0:
				t.Fatalf("no mismatch event expected: %v", notes)
			case c.wantAudit != "" && (len(notes) != 1 || !strings.Contains(notes[0], c.wantAudit) || !strings.Contains(notes[0], "claimed as wrapper:bwrap:0123456789ab")):
				t.Fatalf("mismatch event: %v", notes)
			}
		})
	}
}

// A report that is refused (another worker's lease) writes no mismatch
// event and changes nothing; a retryable failure whose executor differs but
// is allowed is requeued, with the difference recorded once.
func TestExecutorCheckOnlyCountsForAcceptedReports(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	mismatches := func() int {
		n := 0
		for _, e := range spy.got {
			if e.Action == "run.executor_mismatch" {
				n++
			}
		}
		return n
	}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}, Policy: zaiIsolated()}})
	mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)
	run := claimExec(t, s, "w1", "ws", []policy.ExecutorFact{{Executor: "wrapper:bwrap:0123456789ab"}}, fGLM)
	if run == nil {
		t.Fatal("claim")
	}
	in := ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "done", Executor: "local"}
	if _, err := s.Report(rootCtx(), run.ID, in, "not-the-lease"); err == nil {
		t.Fatal("a report without the run's lease must be refused")
	}
	other := in
	other.WorkerID = "w2"
	if _, err := s.Report(rootCtx(), run.ID, other, run.LeaseToken); err == nil {
		t.Fatal("another worker's report must be refused")
	}
	if got, _ := s.Get(rootCtx(), run.ID); got.Status != StatusClaimed || mismatches() != 0 {
		t.Fatalf("a refused report records nothing: status %s, %d events", got.Status, mismatches())
	}
	// Retryable, and the executor differs within what the policy allows.
	retry := ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "fetch failed", Retryable: true, Executor: "wrapper:bwrap:ffffffffffff"}
	got, err := s.Report(rootCtx(), run.ID, retry, run.LeaseToken)
	if err != nil || got.Status != StatusQueued || mismatches() != 1 {
		t.Fatalf("requeued with the difference recorded once: %+v %v, %d events", got, err, mismatches())
	}
}

// A list with an entry that cannot be kept is dropped whole: the entries
// resolve by specificity, so losing an override would let the default
// answer for it.
func TestSanitizeExecutorsFailsClosed(t *testing.T) {
	offered := map[string]bool{"ws": true}
	wrapped := policy.ExecutorFact{Executor: "wrapper:bwrap:0123456789ab"}
	long := policy.ExecutorFact{Stage: "implement", Executor: "host:" + strings.Repeat("x", 300)}
	many := []policy.ExecutorFact{wrapped}
	// More than a worker within the runner cap can have: 64 runners on 8
	// stages, and its default.
	for i := 0; i < 64*8+1; i++ {
		many = append(many, policy.ExecutorFact{Stage: "review", Runner: fmt.Sprintf("r%d", i), Executor: "local"})
	}
	// What such a worker can send is kept.
	fits := []policy.ExecutorFact{wrapped}
	for i := 0; i < 64*8; i++ {
		fits = append(fits, policy.ExecutorFact{Stage: "review", Runner: fmt.Sprintf("r%d", i), Executor: "local"})
	}
	if got := sanitizeExecutors(map[string][]policy.ExecutorFact{"ws": fits}, offered); len(got["ws"]) != len(fits) {
		t.Errorf("a worker with 64 runners differing on 8 stages: kept %d of %d entries", len(got["ws"]), len(fits))
	}
	for name, in := range map[string][]policy.ExecutorFact{
		"an override too long to keep": {wrapped, long},
		"an unknown class":             {wrapped, {Stage: "implement", Executor: "isolated-honest"}},
		"two entries for one place":    {wrapped, {Stage: "implement", Executor: "local"}, {Stage: "Implement", Executor: "wrapper:bwrap:0123456789ab"}},
		"too many entries":             many,
		"no executor":                  {wrapped, {Stage: "implement"}},
	} {
		if got := sanitizeExecutors(map[string][]policy.ExecutorFact{"ws": in}, offered); got != nil {
			t.Errorf("%s: the whole list must go, got %d workspaces", name, len(got))
		}
	}
	got := sanitizeExecutors(map[string][]policy.ExecutorFact{"ws": {{Stage: " Implement ", Executor: " local "}, wrapped}, "elsewhere": {wrapped}}, offered)
	if len(got) != 1 || len(got["ws"]) != 2 || got["ws"][0] != wrapped || got["ws"][1] != (policy.ExecutorFact{Stage: "implement", Executor: "local"}) {
		t.Fatalf("a clean list is kept, trimmed and ordered: %+v", got)
	}
}

// The executor rule in the claim's other scans: an owner whose run the rule
// no longer lets it resume gives it up; the takeover and the stranded
// sweep judge each worker by its own executor; a run does not wait out its
// preference for a worker that does not confine it.
func TestExecutorRuleInEveryScan(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	local := []policy.ExecutorFact{{Executor: "local"}}
	wrapped := []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Executor: "wrapper:bwrap:0123456789ab"}}
	open := map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}}}
	guarded := map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}, Policy: zaiIsolated()}}

	// The owner's paused run, once the rule arrives.
	s.SetPrefs(open)
	mustQueue(t, s, ctx, "ws", "item-1", StageReview)
	first := claimExec(t, s, "w-local", "ws", local, fGLM)
	if first == nil {
		t.Fatal("claimed under no policy")
	}
	if err := s.Pause(ctx, first.ID, "w-local", "quota", now.Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	s.SetPrefs(guarded)
	if run := claimExec(t, s, "w-local", "ws", local, fGLM); run != nil {
		t.Fatalf("the owner must not resume a run the rule now needs confined: %+v", run)
	}
	if got, _ := s.Get(ctx, first.ID); got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("released for a takeover: %+v", got)
	}

	// Nobody who serves it confines GLM reviews: stranded, with the reason.
	now = now.Add(strandedAfter + time.Minute)
	s.SweepStranded(ctx)
	if got, _ := s.Get(ctx, first.ID); !strings.Contains(got.Stranded, "refused by the group personal policy") {
		t.Fatalf("stranded while no worker confines the runner: %q", got.Stranded)
	}
	// The takeover: an unwrapped worker is passed over, a wrapped one takes it.
	if run := claimExec(t, s, "w-local-2", "ws", local, fGLM); run != nil {
		t.Fatalf("a takeover is checked like a queued run: %+v", run)
	}
	taken := claimExec(t, s, "w-wrapped", "ws", wrapped, fGLM)
	if taken == nil || taken.ID != first.ID || taken.WorkerID != "w-wrapped" {
		t.Fatalf("the wrapped worker takes it over: %+v", taken)
	}
	s.SweepStranded(ctx)
	if got, _ := s.Get(ctx, first.ID); got.Stranded != "" {
		t.Fatalf("no longer stranded once taken: %q", got.Stranded)
	}

	// Preference: the next round of the item's review prefers the worker
	// that ran it — but not one that no longer confines it.
	if _, err := s.Report(ctx, taken.ID, ReportInput{WorkerID: "w-wrapped", Status: StatusSucceeded, Summary: "ok", Executor: "wrapper:bwrap:0123456789ab"}, taken.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if run := claimExec(t, s, "w-wrapped", "ws", local, fGLM); run != nil { // its wrapper is gone
		t.Fatal("nothing queued")
	}
	if run := claimExec(t, s, "w-other", "ws", wrapped, fGLM); run != nil {
		t.Fatal("nothing queued")
	}
	again := mustQueue(t, s, ctx, "ws", "item-1", StageReview)
	if run := claimExec(t, s, "w-other", "ws", wrapped, fGLM); run == nil || run.ID != again.ID {
		t.Fatalf("the run must not wait for a preferred worker that no longer confines it (prefer %q): %+v", again.PreferWorker, run)
	}
}

// The report check where a failed run does not block the item by itself: a
// panel member is one failed member, a second opinion never moves the item
// — the run is failed and the difference recorded all the same.
func TestExecutorCheckForPanelAndSecondOpinion(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	ctx := rootCtx()
	insertItem(t, s, "item-1")
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Policy: zaiIsolated(),
		Groups: map[string][]Member{"review": {{Runner: "opencode-glm", Focus: "tests"}, {Runner: "claude", Focus: "security"}}}}})
	if _, err := s.Queue(ctx, QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageReview}); err != nil {
		t.Fatal(err)
	}
	wrapped := []policy.ExecutorFact{{Executor: "wrapper:bwrap:0123456789ab"}}
	member := claimExec(t, s, "w1", "ws-a", wrapped, fGLM)
	if member == nil || member.GroupID == "" {
		t.Fatalf("the GLM panel member: %+v", member)
	}
	got, err := s.Report(ctx, member.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "pass", Executor: "local"}, member.LeaseToken)
	if err != nil || got.Status != StatusFailed || !strings.Contains(got.FailReason, "ran local") {
		t.Fatalf("a panel member that ran outside the rule is a failed member: %+v %v", got, err)
	}
	second, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "opencode-glm", "abc123", "agent/item-1", "base123")
	if err != nil {
		t.Fatal(err)
	}
	run := claimExec(t, s, "w1", "ws-a", wrapped, fGLM)
	if run == nil || run.ID != second {
		t.Fatalf("the second opinion: %+v", run)
	}
	got, err = s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "pass", Executor: "host:knot"}, run.LeaseToken)
	if err != nil || got.Status != StatusFailed {
		t.Fatalf("a second opinion that ran outside the rule is failed: %+v %v", got, err)
	}
	n := 0
	for _, e := range spy.got {
		if e.Action == "run.executor_mismatch" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("both differences recorded: %d", n)
	}
}

// A worker that reports it cannot run a stage with a runner (executor
// none:…) gets no such run — with or without a policy, for a pinned runner
// and for one the server would pick — and a worker that can still does. A
// run only such workers could take is marked stranded with the reason.
func TestUnavailableExecutorIsNeverHandedOut(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}}})
	pinned := mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)
	noWrapper := []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Runner: "opencode-glm", Executor: policy.NoExecutor(policy.NoneWrapper)}}
	if run := claimExec(t, s, "w-broken", "ws", noWrapper, fGLM); run != nil {
		t.Fatalf("a worker that cannot run the stage got it: %+v", run)
	}
	// Nobody else serves the workspace: the run is stranded, and says why.
	now = now.Add(strandedAfter + time.Second)
	s.SweepStranded(rootCtx())
	if r, _ := s.Get(rootCtx(), pinned.ID); !strings.Contains(r.Stranded, "no worker can run review") || !strings.Contains(r.Stranded, "wrapper") {
		t.Fatalf("stranded reason: %q", r.Stranded)
	}
	recorded := false
	for _, e := range spy.got {
		recorded = recorded || (e.Action == "run.stranded" && e.EntityID == pinned.ID)
	}
	if !recorded {
		t.Fatal("no run.stranded event")
	}
	// A worker where the wrapper starts takes it.
	run := claimExec(t, s, "w-ok", "ws", []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Runner: "opencode-glm", Executor: "wrapper:box:0123456789ab"}}, fGLM)
	if run == nil || run.ID != pinned.ID {
		t.Fatalf("the able worker: %+v", run)
	}

	// An unpinned stage: the server picks among the runners the worker can
	// run there, skipping the one it cannot.
	s.SetPrefs(map[string]*WorkspacePrefs{"free": {}})
	mustQueue(t, s, rootCtx(), "free", "item-2", StagePlan)
	facts := []policy.ExecutorFact{{Executor: "host:knot"}, {Stage: "plan", Runner: "claude", Executor: policy.NoExecutor(policy.NoneUncontained)}}
	in := ClaimInput{WorkerID: "w-two", Workspaces: []string{"free"}, Runners: []string{"claude", "opencode-glm"}, RunnerFacts: []policy.Facts{fClaude, fGLM},
		Executors: map[string][]policy.ExecutorFact{"free": facts}}
	picked, err := s.ClaimNext(rootCtx(), in)
	if err != nil || picked == nil || picked.Runner != "opencode-glm" {
		t.Fatalf("the server should pick the runner the worker can run: %+v %v", picked, err)
	}
}

// The unavailable executor in the other scans, with no policy at all: an
// owner that can no longer run its paused run does not get it back (it is
// released for a takeover), a takeover passes over a worker that cannot run
// it, and a queued round does not wait for a preferred worker that cannot.
func TestUnavailableExecutorInEveryScan(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	ok := []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Executor: "wrapper:box:0123456789ab"}}
	broken := []policy.ExecutorFact{{Executor: "local"}, {Stage: "review", Runner: "opencode-glm", Executor: policy.NoExecutor(policy.NoneWrapper)}}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}}})

	mustQueue(t, s, ctx, "ws", "item-1", StageReview)
	first := claimExec(t, s, "w-a", "ws", ok, fGLM)
	if first == nil {
		t.Fatal("claimed")
	}
	if err := s.Pause(ctx, first.ID, "w-a", "quota", now.Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	// The owner's wrapper stops starting.
	if run := claimExec(t, s, "w-a", "ws", broken, fGLM); run != nil {
		t.Fatalf("the owner got back a run it cannot run: %+v", run)
	}
	if got, _ := s.Get(ctx, first.ID); got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("released for a takeover: %+v", got)
	}
	if run := claimExec(t, s, "w-b", "ws", broken, fGLM); run != nil {
		t.Fatalf("a takeover by a worker that cannot run it: %+v", run)
	}
	taken := claimExec(t, s, "w-c", "ws", ok, fGLM)
	if taken == nil || taken.ID != first.ID {
		t.Fatalf("an able worker takes it over: %+v", taken)
	}
	if _, err := s.Report(ctx, taken.ID, ReportInput{WorkerID: "w-c", Status: StatusSucceeded, Summary: "ok", Executor: "wrapper:box:0123456789ab"}, taken.LeaseToken); err != nil {
		t.Fatal(err)
	}
	// The next round prefers w-c; once w-c cannot run it, others need not wait.
	if run := claimExec(t, s, "w-c", "ws", broken, fGLM); run != nil {
		t.Fatal("nothing queued")
	}
	if run := claimExec(t, s, "w-d", "ws", ok, fGLM); run != nil {
		t.Fatal("nothing queued")
	}
	again := mustQueue(t, s, ctx, "ws", "item-1", StageReview)
	if run := claimExec(t, s, "w-d", "ws", ok, fGLM); run == nil || run.ID != again.ID {
		t.Fatalf("the run waits for a preferred worker that cannot run it (prefer %q): %+v", again.PreferWorker, run)
	}
}
