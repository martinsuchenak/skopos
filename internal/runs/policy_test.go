package runs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
)

var (
	fClaude = policy.Facts{Name: "claude", Kind: "claude", Model: "claude-opus", Provider: "anthropic", Endpoint: "api.anthropic.com"}
	fGLM    = policy.Facts{Name: "opencode-glm", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai", Endpoint: "opencode:zai-coding-plan"}
	fCodex  = policy.Facts{Name: "codex", Kind: "codex", Model: "gpt-5.2", Provider: "openai", Endpoint: "api.openai.com",
		Stages: []string{"branch_review", "compare", "plan", "qa", "review", "second_opinion", "synthesize"}}
)

func anthropicOnly() *policy.Effective {
	return &policy.Effective{Layers: []policy.Layer{{Source: "group fortix", Rev: "rev-1",
		Policy: policy.Policy{Allow: []policy.Rule{{Provider: "anthropic"}, {Provider: "openai", Stages: []string{"second_opinion"}}}}}}}
}

func names(facts ...policy.Facts) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Name
	}
	return out
}

func claimWith(t *testing.T, s *Service, worker, ws string, facts ...policy.Facts) *Run {
	t.Helper()
	run, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: worker, Workspaces: []string{ws}, Runners: names(facts...), RunnerFacts: facts})
	if err != nil {
		t.Fatalf("claim by %s: %v", worker, err)
	}
	return run
}

// A pinned run goes only to a worker whose facts for that runner pass the
// policy — the same runner name can be allowed on one worker and refused on
// another — and the run records the runner, the policy and the facts.
func TestClaimChecksPolicyByTheWorkersFacts(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "reviewer"}, Policy: anthropicOnly()}})
	mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)

	// "reviewer" on w1 is opencode with GLM; on w2 it is Claude.
	glmReviewer, claudeReviewer := fGLM, fClaude
	glmReviewer.Name, claudeReviewer.Name = "reviewer", "reviewer"
	if run := claimWith(t, s, "w1", "ws", glmReviewer); run != nil {
		t.Fatalf("a GLM runner must not get a run the policy keeps to Anthropic: %+v", run)
	}
	run := claimWith(t, s, "w2", "ws", claudeReviewer)
	if run == nil || run.Runner != "reviewer" || run.RunnerPicked {
		t.Fatalf("the Claude worker takes it, pinned by the preference: %+v", run)
	}
	if run.PolicyRev == "" || run.PolicyRev != anthropicOnly().Rev() {
		t.Fatalf("the run must record the policy it was decided with: %q", run.PolicyRev)
	}
	var rec factsRecord
	if err := json.Unmarshal(run.Facts, &rec); err != nil {
		t.Fatalf("facts: %v %s", err, run.Facts)
	}
	if rec.Provider != "anthropic" || rec.Endpoint != "api.anthropic.com" || rec.Model != "claude-opus" ||
		len(rec.Policy) != 1 || rec.Policy[0] != "rev-1" || len(rec.Rules) != 1 || rec.Rules[0] != 0 {
		t.Fatalf("the run must record the facts and the rule that allowed it: %+v", rec)
	}
	// What a later read of the run shows is the same record.
	if got, err := s.Get(rootCtx(), run.ID); err != nil || got.PolicyRev != run.PolicyRev || string(got.Facts) != string(run.Facts) {
		t.Fatalf("stored decision: %+v %v", got, err)
	}
}

// A worker that reports no runner facts gets nothing from a workspace with
// a policy, pinned or not — and everything it got before from one without.
func TestWorkerWithoutFactsFailsClosedUnderAPolicy(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{
		"guarded": {Stages: map[string]string{"review": "claude"}, Policy: anthropicOnly()},
		"open":    {Stages: map[string]string{"review": "claude"}},
	})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "guarded", "item-1", StageReview)
	mustQueue(t, s, ctx, "guarded", "item-2", StagePlan) // nothing pins it
	mustQueue(t, s, ctx, "open", "item-3", StageReview)
	mustQueue(t, s, ctx, "open", "item-4", StagePlan)

	old := func(ws string) *Run {
		t.Helper()
		run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "old-" + ws, Workspaces: []string{ws}, Runners: []string{"claude"}})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	if run := old("guarded"); run != nil {
		t.Fatalf("no facts, policy: nothing — got %+v", run)
	}
	first := old("open")
	if first == nil || first.Runner != "claude" || first.PolicyRev != "" || len(first.Facts) != 0 {
		t.Fatalf("no policy: the pinned run as before: %+v", first)
	}
	if _, err := s.Report(ctx, first.ID, ReportInput{WorkerID: "old-open", Status: StatusSucceeded}, first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if second := old("open"); second == nil || second.Runner != "" || second.RunnerPicked {
		t.Fatalf("no policy, no facts: the unpinned run stays unpinned (the worker picks, as before): %+v", second)
	}
	// With facts the same worker gets both guarded runs.
	if run := claimWith(t, s, "new", "guarded", fClaude); run == nil {
		t.Fatal("a worker with facts takes the guarded run")
	}
}

// A run nothing pins: the server picks the first runner in the worker's
// order that the policy allows and that can run the stage, writes it to
// the run, and forgets the pick when the run is requeued.
func TestServerPicksTheRunnerOfAnUnpinnedRun(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: anthropicOnly()}})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)

	// GLM is first in the worker's order but not allowed; codex is allowed
	// for nothing here and cannot implement anyway; Claude fits.
	run := claimWith(t, s, "w1", "ws", fGLM, fCodex, fClaude)
	if run == nil || run.Runner != "claude" || !run.RunnerPicked {
		t.Fatalf("the server must pick claude: %+v", run)
	}
	// A retryable failure sends it back: the pick is forgotten, so another
	// worker's own runner name is picked, not waited for.
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "setup failed", Retryable: true}, run.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, run.ID); got.Status != StatusQueued || got.Runner != "" || got.RunnerPicked {
		t.Fatalf("a requeue must clear a picked runner: %+v", got)
	}
	if _, err := s.storage.db.Exec(`UPDATE runs SET retry_after = '' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	other := fClaude
	other.Name = "claude-max"
	again := claimWith(t, s, "w2", "ws", other)
	if again == nil || again.ID != run.ID || again.Runner != "claude-max" || !again.RunnerPicked {
		t.Fatalf("the second worker's runner is picked: %+v", again)
	}

	// Without a policy the server still picks (the worker always gets a
	// named runner), skipping a read-only runner for implement.
	s.SetPrefs(nil)
	mustQueue(t, s, ctx, "open", "item-2", StageImplement)
	if picked := claimWith(t, s, "w3", "open", fCodex, fGLM); picked == nil || picked.Runner != "opencode-glm" || !picked.RunnerPicked {
		t.Fatalf("codex cannot implement; GLM is picked where no policy applies: %+v", picked)
	}
	// A worker whose only runner cannot run the stage gets nothing.
	mustQueue(t, s, ctx, "open", "item-3", StageImplement)
	if run := claimWith(t, s, "w4", "open", fCodex); run != nil {
		t.Fatalf("a read-only runner must not be handed implement: %+v", run)
	}
}

// The claim runs the workspace's current preference, whatever the run was
// queued with, and says so on the run.
func TestClaimWritesTheCurrentPreference(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "claude"}}})
	queued := mustQueue(t, s, rootCtx(), "ws", "item-1", StageReview)
	if queued.Runner != "claude" {
		t.Fatalf("queued with the preference of the time: %+v", queued)
	}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"review": "opencode-glm"}}})
	if run := claimWith(t, s, "w1", "ws", fClaude); run != nil {
		t.Fatalf("the preference is GLM now; a Claude-only worker waits: %+v", run)
	}
	run := claimWith(t, s, "w2", "ws", fGLM, fClaude)
	if run == nil || run.Runner != "opencode-glm" {
		t.Fatalf("the run must name the runner the worker is to run, not the one it was queued with: %+v", run)
	}
}

// A paused run whose owner may no longer run it — the policy changed while
// it waited — is released: another worker the policy allows takes it over
// from the branch, and the owner does not get it back.
func TestOwnersRunReleasedWhenThePolicyChanges(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"implement": "opencode-glm"}}})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	first := claimWith(t, s, "w-glm", "ws", fGLM)
	if first == nil {
		t.Fatal("claimed under no policy")
	}
	if err := s.Pause(ctx, first.ID, "w-glm", "quota", time.Now().Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	// The policy arrives, and the preference moves to Claude.
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"implement": "claude"}, Policy: anthropicOnly()}})
	if run := claimWith(t, s, "w-glm", "ws", fGLM); run != nil {
		t.Fatalf("the owner must not resume a run the policy now refuses: %+v", run)
	}
	got, _ := s.Get(ctx, first.ID)
	if got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("the run must be released for a takeover: %+v", got)
	}
	taken := claimWith(t, s, "w-claude", "ws", fClaude)
	if taken == nil || taken.ID != first.ID || taken.WorkerID != "w-claude" || taken.Runner != "claude" {
		t.Fatalf("an allowed worker takes it over: %+v", taken)
	}
}

// The owner's interrupted run comes back to it even when no account for
// its runner is free right now: that is a quota wait for the worker to
// handle, not a reason to give the run away.
func TestOwnersRunReturnsWhileItsRunnerIsExhausted(t *testing.T) {
	s := shortLeaseService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"implement": "claude"}, Policy: anthropicOnly()}})
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	first := claimWith(t, s, "w1", "ws", fClaude)
	if first == nil {
		t.Fatal("first claim")
	}
	time.Sleep(80 * time.Millisecond) // the lease runs out
	// The worker still has claude configured (it reports its facts), but
	// no account is available now: it offers no runner.
	run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: nil, RunnerFacts: []policy.Facts{fClaude}})
	if err != nil || run == nil || run.ID != first.ID || run.Runner != "claude" {
		t.Fatalf("the owner gets its run back: %+v %v", run, err)
	}
}

// Each member of a panel is checked on its own: the policy refuses the GLM
// member's run and lets the Claude member's through.
func TestPanelMembersAreCheckedEach(t *testing.T) {
	s := testService(t)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {
		Groups: map[string][]Member{"review": {{Runner: "claude"}, {Runner: "opencode-glm"}}},
		Policy: anthropicOnly(),
	}})
	insertItem(t, s, "item-1")
	if _, err := s.Queue(rootCtx(), QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageReview}); err != nil {
		t.Fatal(err)
	}
	if run := claimWith(t, s, "w-glm", "ws-a", fGLM); run != nil {
		t.Fatalf("the GLM member must not be claimable under the policy: %+v", run)
	}
	run := claimWith(t, s, "w-both", "ws-a", fGLM, fClaude)
	if run == nil || run.Runner != "claude" || run.GroupID == "" {
		t.Fatalf("the Claude member is claimed: %+v", run)
	}
	if again := claimWith(t, s, "w-both-2", "ws-a", fGLM, fClaude); again != nil {
		t.Fatalf("the GLM member stays queued: %+v", again)
	}
}

// A requeued run does not wait for the worker that ran it before when the
// policy refuses that worker.
func TestPreferredWorkerRefusedByPolicyIsNotWaitedFor(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {}})
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	first := claimWith(t, s, "w-glm", "ws", fGLM)
	if first == nil || first.Runner != "opencode-glm" {
		t.Fatalf("first run on GLM: %+v", first)
	}
	if _, err := s.Report(ctx, first.ID, ReportInput{WorkerID: "w-glm", Status: StatusFailed, Reason: "timed out"}, first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if run := claimWith(t, s, "w-claude", "ws", fClaude); run != nil { // known, idle
		t.Fatal("nothing queued")
	}
	if run := claimWith(t, s, "w-glm", "ws", fGLM); run != nil { // still online
		t.Fatal("nothing queued")
	}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: anthropicOnly()}})
	again := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if again.PreferWorker != "w-glm" {
		t.Fatalf("the retry prefers the worker that ran it: %+v", again)
	}
	run := claimWith(t, s, "w-claude", "ws", fClaude)
	if run == nil || run.ID != again.ID {
		t.Fatalf("the policy refuses the preferred worker, so the run must not wait for it: %+v", run)
	}
}

// What a worker reports about its runners is recorded when it changes: the
// first report, a changed provider, and nothing while it stays the same.
func TestWorkerFactsChangesAreAudited(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	factsEvents := func() []string {
		var out []string
		for _, e := range spy.got {
			if e.Action == "worker.facts" {
				if e.EntityType != "worker" || e.EntityID != "w1" {
					t.Errorf("entry shape: %+v", e)
				}
				out = append(out, e.Notes)
			}
		}
		return out
	}
	claimWith(t, s, "w1", "ws", fClaude)
	claimWith(t, s, "w1", "ws", fClaude)
	if got := factsEvents(); len(got) != 1 || !strings.Contains(got[0], "was: no worker row") || !strings.Contains(got[0], "claude (kind=claude provider=anthropic") {
		t.Fatalf("the first report is recorded once: %q", got)
	}
	moved := fClaude
	moved.Provider, moved.Endpoint = "unknown:proxy.example", "proxy.example"
	claimWith(t, s, "w1", "ws", moved)
	got := factsEvents()
	if len(got) != 2 || !strings.Contains(got[1], "was: claude (kind=claude provider=anthropic") || !strings.Contains(got[1], "now: claude (kind=claude provider=unknown:proxy.example") {
		t.Fatalf("a changed endpoint must be recorded with old and new: %q", got)
	}
	// Dropping the facts (a downgraded worker) is a change too.
	if _, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if got := factsEvents(); len(got) != 3 || !strings.HasSuffix(got[2], "now: none") {
		t.Fatalf("facts that stop being reported: %q", got)
	}
}

// Runner facts are client claims: bounded, deduplicated, normalised.
func TestSanitizeFacts(t *testing.T) {
	long := strings.Repeat("x", 500)
	in := []policy.Facts{
		{Name: " claude ", Kind: "Claude", Provider: "Anthropic", Endpoint: "API.Anthropic.com", Stages: []string{"Review", "", "plan"}},
		{Name: "claude", Provider: "zai"}, // a second entry for the name is dropped
		{Name: "", Provider: "x"},
		{Name: long, Model: long},
		{Name: "noprovider"},
	}
	out := sanitizeFacts(in)
	if len(out) != 3 {
		t.Fatalf("kept %d: %+v", len(out), out)
	}
	if f := out[0]; f.Name != "claude" || f.Kind != "claude" || f.Provider != "anthropic" || f.Endpoint != "api.anthropic.com" ||
		strings.Join(f.Stages, ",") != "plan,review" {
		t.Fatalf("normalised: %+v", f)
	}
	if len(out[1].Name) != 200 || len(out[1].Model) != 200 {
		t.Fatalf("strings are clipped: %d %d", len(out[1].Name), len(out[1].Model))
	}
	if out[2].Provider != "unknown:none" {
		t.Fatalf("a runner with no provider is unknown, never empty: %+v", out[2])
	}
	many := make([]policy.Facts, 100)
	for i := range many {
		many[i] = policy.Facts{Name: strings.Repeat("r", i+1)}
	}
	if got := sanitizeFacts(many); len(got) != 64 {
		t.Fatalf("at most 64 runners: %d", len(got))
	}
}

// A queued run no worker may take under the policy is marked stranded with
// the reason and recorded once; a run that only waits for a runner nobody
// offers, or for a worker that is allowed, is not; the mark goes when a
// worker that may take it appears.
func TestSweepStranded(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{
		"guarded": {Stages: map[string]string{"review": "opencode-glm", "plan": "gemini"}, Policy: anthropicOnly()},
		"open":    {Stages: map[string]string{"review": "opencode-glm"}},
	})
	refused := mustQueue(t, s, ctx, "guarded", "item-1", StageReview)     // GLM is offered, the policy refuses it
	nobody := mustQueue(t, s, ctx, "guarded", "item-2", StagePlan)        // nobody offers gemini: not a policy matter
	unpinned := mustQueue(t, s, ctx, "guarded", "item-3", StageImplement) // only GLM workers serve it
	fine := mustQueue(t, s, ctx, "open", "item-4", StageReview)           // no policy

	// A GLM-only worker serves both workspaces, and is busy elsewhere.
	if run := claimWith(t, s, "w-glm", "elsewhere", fGLM); run != nil {
		t.Fatal("nothing queued there")
	}
	if _, err := s.storage.db.Exec(`UPDATE workers SET workspaces = '["guarded","open"]' WHERE id = 'w-glm'`); err != nil {
		t.Fatal(err)
	}
	stranded := func(id string) string {
		t.Helper()
		r, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Stranded
	}

	// Too fresh to judge: every worker must have had time to poll.
	s.SweepStranded(ctx)
	if stranded(refused.ID) != "" {
		t.Fatal("a run is not judged before strandedAfter")
	}
	now = now.Add(strandedAfter + time.Second)
	s.SweepStranded(ctx)
	s.SweepStranded(ctx) // again: recorded once
	if got := stranded(refused.ID); !strings.Contains(got, "no worker offers an allowed runner for review") || !strings.Contains(got, "group fortix policy") {
		t.Fatalf("the refused run must be marked with the reason: %q", got)
	}
	if got := stranded(unpinned.ID); !strings.Contains(got, "for implement") {
		t.Fatalf("an unpinned run only refused workers serve is stranded too: %q", got)
	}
	if stranded(nobody.ID) != "" || stranded(fine.ID) != "" {
		t.Fatalf("not stranded: %q %q", stranded(nobody.ID), stranded(fine.ID))
	}
	var events []string
	for _, e := range spy.got {
		if e.Action == "run.stranded" {
			events = append(events, e.EntityID)
			if e.EntityType != "run" || e.WorkspaceID != "guarded" || e.Notes == "" {
				t.Errorf("event shape: %+v", e)
			}
		}
	}
	if len(events) != 2 {
		t.Fatalf("each stranded run is recorded once: %v", events)
	}

	// A Claude worker for the workspace appears: the unpinned run is no
	// longer stranded (it can take it); the GLM-pinned review still is.
	if run := claimWith(t, s, "w-claude", "elsewhere", fClaude); run != nil {
		t.Fatal("nothing queued there")
	}
	if _, err := s.storage.db.Exec(`UPDATE workers SET workspaces = '["guarded"]' WHERE id = 'w-claude'`); err != nil {
		t.Fatal(err)
	}
	s.SweepStranded(ctx)
	if stranded(unpinned.ID) != "" {
		t.Fatalf("the mark must go once a worker may take the run: %q", stranded(unpinned.ID))
	}
	if stranded(refused.ID) == "" {
		t.Fatal("the GLM-pinned review is still stranded")
	}
	// Claimed: the mark is cleared with the claim.
	run := claimWith(t, s, "w-claude", "guarded", fClaude)
	if run == nil || run.ID != unpinned.ID || run.Stranded != "" {
		t.Fatalf("claimed run: %+v", run)
	}
}

// A run released because the policy changed may come straight back to the
// same worker — as the runner the policy allows, named on the run, so the
// worker cannot carry on with the refused one.
func TestReleasedRunComesBackForTheAllowedRunner(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {}})
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	// The worker has both; GLM leads its order, so GLM is picked.
	first := claimWith(t, s, "w1", "ws", fGLM, fClaude)
	if first == nil || first.Runner != "opencode-glm" || !first.RunnerPicked {
		t.Fatalf("picked GLM under no policy: %+v", first)
	}
	if err := s.Pause(ctx, first.ID, "w1", "quota", time.Now().Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: anthropicOnly()}})
	again := claimWith(t, s, "w1", "ws", fGLM, fClaude)
	if again == nil || again.ID != first.ID {
		t.Fatalf("the same worker may take its released run over: %+v", again)
	}
	if again.Runner != "claude" || !again.RunnerPicked || again.PolicyRev == "" {
		t.Fatalf("it must come back for the allowed runner, recorded: %+v", again)
	}
	var rec factsRecord
	if err := json.Unmarshal(again.Facts, &rec); err != nil || rec.Provider != "anthropic" {
		t.Fatalf("the recorded facts are the allowed runner's: %s %v", again.Facts, err)
	}
}

// A worker's own run in a workspace its claim does not offer is still
// checked against that workspace's policy — never handed back unchecked.
func TestOwnRunOutsideTheClaimedWorkspacesIsChecked(t *testing.T) {
	s := shortLeaseService(t)
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"guarded": {}})
	mustQueue(t, s, ctx, "guarded", "item-1", StageImplement)
	first := claimWith(t, s, "w1", "guarded", fGLM)
	if first == nil {
		t.Fatal("claimed under no policy")
	}
	time.Sleep(80 * time.Millisecond) // the lease runs out
	s.SetPrefs(map[string]*WorkspacePrefs{"guarded": {Policy: anthropicOnly()}})
	// The worker now polls for another workspace only.
	if run := claimWith(t, s, "w1", "elsewhere", fGLM); run != nil {
		t.Fatalf("the policy of the run's own workspace applies, whatever the claim offers: %+v", run)
	}
	if got, _ := s.Get(ctx, first.ID); got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("refused, so released for a takeover: %+v", got)
	}
}

// A runner the server picked pins nothing: when the worker dies, another
// worker takes the run over with its own runner — one with other runner
// names, and one from before runner facts (which then picks for itself).
func TestPickedRunnerDoesNotPinATakeover(t *testing.T) {
	for name, takeover := range map[string]func(t *testing.T, s *Service) *Run{
		"another runner name": func(t *testing.T, s *Service) *Run {
			other := fClaude
			other.Name = "claude-max"
			return claimWith(t, s, "w2", "ws", other)
		},
		"a worker without facts": func(t *testing.T, s *Service) *Run {
			run, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w2", Workspaces: []string{"ws"}, Runners: []string{"claude-max"}})
			if err != nil {
				t.Fatal(err)
			}
			return run
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := testService(t)
			mustQueue(t, s, rootCtx(), "ws", "item-1", StageImplement)
			first := claimWith(t, s, "w1", "ws", fClaude)
			if first == nil || !first.RunnerPicked {
				t.Fatalf("first: %+v", first)
			}
			if _, err := s.storage.db.Exec(`UPDATE runs SET lease_expires_at = ? WHERE id = ?`, formatTime(time.Now().Add(-2*takeoverGrace)), first.ID); err != nil {
				t.Fatal(err)
			}
			run := takeover(t, s)
			if run == nil || run.ID != first.ID || run.WorkerID != "w2" {
				t.Fatalf("the takeover must not wait for the first worker's runner name: %+v", run)
			}
			if run.Runner == "claude" {
				t.Fatalf("the old pick must not be handed to a worker that may not have it: %+v", run)
			}
		})
	}
}

// A worker asked to resume a run on a runner it no longer has (renamed in
// its config) does not get it back: it would only fail it. The run is
// released for another worker.
func TestResumeOnARunnerTheWorkerNoLongerHas(t *testing.T) {
	s := shortLeaseService(t)
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Stages: map[string]string{"implement": "claude"}}})
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	first := claimWith(t, s, "w1", "ws", fClaude)
	if first == nil {
		t.Fatal("first claim")
	}
	time.Sleep(80 * time.Millisecond)
	renamed := fClaude
	renamed.Name = "claude-opus"
	if run := claimWith(t, s, "w1", "ws", renamed); run != nil {
		t.Fatalf("the worker no longer has runner claude: %+v", run)
	}
	if got, _ := s.Get(ctx, first.ID); got.Status != StatusInterrupted || got.ResumeKind != ResumeBranch {
		t.Fatalf("released for a worker that has the runner: %+v", got)
	}
}

// While the policy cannot be read nothing is decided: the owner's run is
// neither handed back nor given away, and queued runs wait.
func TestUnreadablePolicyHolds(t *testing.T) {
	s := shortLeaseService(t)
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {}})
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	mustQueue(t, s, ctx, "ws", "item-2", StagePlan)
	first := claimWith(t, s, "w1", "ws", fClaude)
	if first == nil {
		t.Fatal("first claim")
	}
	time.Sleep(80 * time.Millisecond)
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: policy.DenyAll("the policy could not be read")}})
	if run := claimWith(t, s, "w1", "ws", fClaude); run != nil {
		t.Fatalf("nothing is handed out while the policy cannot be read: %+v", run)
	}
	got, _ := s.Get(ctx, first.ID)
	if got.Status != StatusInterrupted || got.ResumeKind == ResumeBranch || got.WorkerID != "w1" {
		t.Fatalf("the owner keeps its run (session affinity) until the policy can be read: %+v", got)
	}
	// Readable again: the owner gets it back.
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {}})
	if run := claimWith(t, s, "w1", "ws", fClaude); run == nil || run.ID != first.ID {
		t.Fatalf("readable again: %+v", run)
	}
	// And a held policy never marks a run stranded.
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: policy.DenyAll("the policy could not be read")}})
	if _, err := s.storage.db.Exec(`UPDATE runs SET updated_at = ?, created_at = ? WHERE status = 'queued'`, formatTime(time.Now().Add(-time.Hour)), formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	s.SweepStranded(ctx)
	var n int
	if err := s.storage.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE stranded_reason != ''`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an unreadable policy strands nothing: %d %v", n, err)
	}
}

// The stranded sweep also covers a run released for a takeover that nobody
// may take, says so plainly when the only workers report no facts, ignores
// workers gone for a day, and drops the mark from a run that stopped
// waiting.
func TestSweepStrandedEdges(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {}, "old-only": {Policy: anthropicOnly()}, "stale": {Policy: anthropicOnly()}})
	reason := func(id string) string {
		t.Helper()
		r, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.Stranded
	}

	// Released for a takeover: a GLM worker's run under a policy that
	// arrives later, with no other worker for the workspace.
	mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	first := claimWith(t, s, "w-glm", "ws", fGLM)
	if err := s.Pause(ctx, first.ID, "w-glm", "quota", now.Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	s.SetPrefs(map[string]*WorkspacePrefs{"ws": {Policy: anthropicOnly()}, "old-only": {Policy: anthropicOnly()}, "stale": {Policy: anthropicOnly()}})
	if run := claimWith(t, s, "w-glm", "ws", fGLM); run != nil {
		t.Fatal("refused")
	}
	// Only a worker without facts serves this workspace.
	oldOnly := mustQueue(t, s, ctx, "old-only", "item-2", StagePlan)
	if _, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-old", Workspaces: []string{"old-only"}, Runners: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	// A Claude worker served this one, but was last seen two days ago; a
	// GLM worker serves it now.
	stale := mustQueue(t, s, ctx, "stale", "item-3", StagePlan)
	if run := claimWith(t, s, "w-gone", "elsewhere", fClaude); run != nil {
		t.Fatal("nothing queued there")
	}
	if run := claimWith(t, s, "w-glm-2", "elsewhere", fGLM); run != nil {
		t.Fatal("nothing queued there")
	}
	if _, err := s.storage.db.Exec(`UPDATE workers SET workspaces = '["stale"]' WHERE id IN ('w-gone', 'w-glm-2')`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	for _, id := range []string{"w-glm", "w-old", "w-glm-2"} { // still polling; w-gone is not
		if _, err := s.storage.db.Exec(`UPDATE workers SET last_seen = ? WHERE id = ?`, formatTime(now), id); err != nil {
			t.Fatal(err)
		}
	}
	s.SweepStranded(ctx)
	if got := reason(first.ID); !strings.Contains(got, "refused by the group fortix policy") {
		t.Fatalf("a run released for a takeover nobody may take is stranded: %q", got)
	}
	if got := reason(oldOnly.ID); !strings.Contains(got, "report no runner facts") || strings.Contains(got, "policy policy") {
		t.Fatalf("workers without facts: %q", got)
	}
	if got := reason(stale.ID); !strings.Contains(got, "refused by the group fortix policy") {
		t.Fatalf("a worker gone for two days must not make the run look takeable: %q", got)
	}
	// Cancelled: the mark goes with the next sweep.
	if err := s.CancelItem(ctx, "item-3", "rejected"); err != nil {
		t.Fatal(err)
	}
	s.SweepStranded(ctx)
	if got := reason(stale.ID); got != "" {
		t.Fatalf("a cancelled run is not waiting: %q", got)
	}
}
