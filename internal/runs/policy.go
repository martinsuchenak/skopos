package runs

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/policy"
)

// The claim's runner decision (tmp/central-config — design.md, phase 1a).
//
// For every run it hands out, the claim resolves the one runner the worker
// will run, checks that runner against the workspace's policy using what
// the claiming worker reports about it, and writes the runner, the policy
// revision and the facts to the run in the same transaction. All three
// scans go through it — the worker's own interrupted runs included — the
// way they all honour `unhealthy`.

// claimFacts is what a claiming worker offers, indexed for the decision.
type claimFacts struct {
	offered map[string]bool         // runner names with an account available now
	facts   map[string]policy.Facts // by runner name
	order   []string                // names in the worker's order of preference
	sent    bool                    // the worker reports runner facts at all
	// executors: what contains a run on the worker, per workspace (nil
	// when it reports none — no rule with `executor` then matches).
	executors map[string][]policy.ExecutorFact
}

func newClaimFacts(runners []string, facts []policy.Facts, executors map[string][]policy.ExecutorFact) *claimFacts {
	c := &claimFacts{offered: map[string]bool{}, facts: map[string]policy.Facts{}, sent: len(facts) > 0, executors: executors}
	for _, r := range runners {
		c.offered[r] = true
	}
	for _, f := range facts {
		if _, dup := c.facts[f.Name]; dup {
			continue
		}
		c.facts[f.Name] = f
		c.order = append(c.order, f.Name)
	}
	return c
}

// at is the worker's facts for a runner as a run of r would see them: with
// the executor it reports for r's workspace, stage and that runner.
func (c *claimFacts) at(r *Run, name string) (policy.Facts, bool) {
	f, ok := c.facts[name]
	if !ok {
		return policy.Facts{}, false
	}
	f.Executor = policy.ResolveExecutor(c.executors[r.WorkspaceID], r.Stage, name)
	return f, true
}

// decision is what the claim decided for one run and one worker.
type decision struct {
	ok     bool
	runner string        // the runner the worker runs ("" only for a worker without facts under no policy)
	picked bool          // the server chose it: nothing pinned the run
	facts  *policy.Facts // the worker's facts for it (nil when it sent none)
	rules  []int         // per policy layer, the rule that allowed it
	// refused: the policy layer that allowed nothing ("" when the policy
	// is not what stands in the way). noFacts: a policy applies and the
	// worker reports no runner facts, so nothing could be checked. hold:
	// the policy could not be read — decide nothing from this answer.
	refused string
	noFacts bool
	hold    bool
	// unavailable: the worker has the runner and reports that it cannot
	// run this stage with it in the workspace (executor none:…).
	unavailable string
}

// policyStage is the stage a policy rule sees: a review-only item's
// branch_review is a review.
func policyStage(stage string) string { return routeStage(stage) }

// pinnedRunner is the runner a run is tied to, or "" when nothing pins it:
// a group member and a run queued for a runner without a current stage
// preference keep that runner; an ordinary run follows the workspace's
// current preference. A runner the server picked at an earlier claim pins
// nothing: the next worker to take the run gets its own pick.
func pinnedRunner(r *Run, prefs *WorkspacePrefs) string {
	pref := prefs.StageRunner(r.Stage)
	if r.GroupID != "" || (pref == "" && r.Runner != "" && !r.RunnerPicked) {
		pref = r.Runner
	}
	return pref
}

// decide resolves and checks the runner for a queued (or takeover) run.
func decide(r *Run, prefs *WorkspacePrefs, c *claimFacts) decision {
	return decideFor(r, pinnedRunner(r, prefs), prefs, c, true)
}

// decideResume is decide for a run its owner takes back: the runner it was
// running stays (its session belongs to it), and whether an account is
// free right now is the worker's business (a quota pause), not a reason to
// give the run away.
func decideResume(r *Run, prefs *WorkspacePrefs, c *claimFacts) decision {
	return decideFor(r, r.Runner, prefs, c, false)
}

func decideFor(r *Run, name string, prefs *WorkspacePrefs, c *claimFacts, needOffered bool) decision {
	var pol *policy.Effective
	if prefs != nil {
		pol = prefs.Policy
	}
	if pol.Held() {
		return decision{hold: true}
	}
	stage := policyStage(r.Stage)
	if !c.sent {
		// A worker from before runner facts: as it always was where no
		// policy applies (a pinned run when it offers the runner, an
		// unpinned one for it to pick) — and nothing under a policy, which
		// could not be checked.
		if !pol.AllowAll() {
			return decision{noFacts: true}
		}
		if name != "" && needOffered && !c.offered[name] {
			return decision{}
		}
		return decision{ok: true, runner: name}
	}
	if name != "" {
		f, has := c.at(r, name)
		// A worker that reports facts reports them for every runner it
		// has: none for this name means it does not have the runner (a
		// renamed one, for a run it is asked to resume).
		if !has || (needOffered && !c.offered[name]) || !f.CanRun(r.Stage) {
			d := decision{}
			if has && policy.Unavailable(f.Executor) {
				d.unavailable = f.Executor
			}
			return d
		}
		d := pol.Allows(f, stage)
		return decision{ok: d.Allowed, runner: name, facts: &f, rules: d.Rules, refused: d.Refused}
	}
	refused, unavailable := "", ""
	for _, n := range c.order {
		f, _ := c.at(r, n)
		if (needOffered && !c.offered[n]) || !f.CanRun(r.Stage) {
			if policy.Unavailable(f.Executor) {
				unavailable = f.Executor
			}
			continue
		}
		d := pol.Allows(f, stage)
		if d.Allowed {
			return decision{ok: true, runner: n, picked: true, facts: &f, rules: d.Rules}
		}
		refused = d.Refused
	}
	return decision{refused: refused, unavailable: unavailable}
}

// factsRecord is what a run keeps about the decision (runs.facts).
type factsRecord struct {
	policy.Facts
	Picked bool     `json:"picked,omitempty"`
	Policy []string `json:"policy,omitempty"` // the policy layers' revisions, in order
	Rules  []int    `json:"rules,omitempty"`  // per layer, the rule that allowed the runner
}

// record renders the decision for the run: the policy revision and the
// facts JSON ("" when the worker sent none).
func (d decision) record(prefs *WorkspacePrefs) (policyRev, facts string) {
	var pol *policy.Effective
	if prefs != nil {
		pol = prefs.Policy
	}
	policyRev = pol.Rev()
	if d.facts == nil {
		return policyRev, ""
	}
	b, err := json.Marshal(factsRecord{Facts: *d.facts, Picked: d.picked, Policy: pol.Revs(), Rules: d.rules})
	if err != nil {
		return policyRev, ""
	}
	return policyRev, string(b)
}

// sanitizeFacts bounds what a worker says about its runners: client
// strings, a client-chosen list.
func sanitizeFacts(in []policy.Facts) []policy.Facts {
	const maxRunners, maxLen = 64, 200
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if r := []rune(s); len(r) > maxLen {
			s = string(r[:maxLen])
		}
		return s
	}
	seen := map[string]bool{}
	out := make([]policy.Facts, 0, len(in))
	for _, f := range in {
		f.Name = clip(f.Name)
		if f.Name == "" || seen[f.Name] || len(out) == maxRunners {
			continue
		}
		seen[f.Name] = true
		f.Kind, f.Model, f.Effort = strings.ToLower(clip(f.Kind)), clip(f.Model), clip(f.Effort)
		f.Provider, f.Endpoint = strings.ToLower(clip(f.Provider)), strings.ToLower(clip(f.Endpoint))
		if f.Provider == "" {
			f.Provider = policy.UnknownPrefix + "none"
		}
		var stages []string
		for _, s := range f.Stages {
			if s = strings.ToLower(clip(s)); s != "" && len(stages) < 16 {
				stages = append(stages, s)
			}
		}
		sort.Strings(stages)
		f.Stages = stages
		out = append(out, f)
	}
	return out
}

// sanitizeExecutors bounds what a worker says contains its runs: only for
// workspaces it offers, a bounded list of bounded strings, and only
// executor ids of a known class. A list with anything in it that cannot
// be kept — too long, too many, a duplicate, an unknown class — is dropped
// whole: the entries resolve by specificity, so losing one would let a
// broader entry (the default) answer for it. Dropped reads as "reports no
// executors" for that workspace, and no rule with `executor` then matches.
func sanitizeExecutors(in map[string][]policy.ExecutorFact, offered map[string]bool) map[string][]policy.ExecutorFact {
	const maxFacts, maxLen = 128, 200
	out := map[string][]policy.ExecutorFact{}
	for ws, facts := range in {
		if !offered[ws] || len(facts) > maxFacts {
			continue
		}
		var keep []policy.ExecutorFact
		seen := map[[2]string]bool{}
		whole := true
		for _, f := range facts {
			f.Stage, f.Runner, f.Executor = strings.ToLower(strings.TrimSpace(f.Stage)), strings.TrimSpace(f.Runner), strings.TrimSpace(f.Executor)
			k := [2]string{f.Stage, f.Runner}
			if seen[k] || len(f.Stage) > maxLen || len(f.Runner) > maxLen || len(f.Executor) > maxLen || !policy.KnownExecutor(f.Executor) {
				whole = false
				break
			}
			seen[k] = true
			keep = append(keep, f)
		}
		if whole && len(keep) > 0 {
			sort.Slice(keep, func(i, j int) bool {
				if keep[i].Stage != keep[j].Stage {
					return keep[i].Stage < keep[j].Stage
				}
				return keep[i].Runner < keep[j].Runner
			})
			out[ws] = keep
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// executorsJSON is the stored form of a worker's executor facts ("" = none).
func executorsJSON(m map[string][]policy.ExecutorFact) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseExecutors reads a worker's stored executor facts.
func parseExecutors(raw string) map[string][]policy.ExecutorFact {
	if raw == "" {
		return nil
	}
	var out map[string][]policy.ExecutorFact
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// describeExecutors renders executor facts as one line.
func describeExecutors(m map[string][]policy.ExecutorFact) string {
	if len(m) == 0 {
		return "none"
	}
	names := make([]string, 0, len(m))
	for ws := range m {
		names = append(names, ws)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, ws := range names {
		var fs []string
		for _, f := range m[ws] {
			where := "default"
			switch {
			case f.Stage != "" && f.Runner != "":
				where = f.Stage + "/" + f.Runner
			case f.Stage != "":
				where = f.Stage
			case f.Runner != "":
				where = "runner " + f.Runner
			}
			fs = append(fs, where+"="+f.Executor)
		}
		parts = append(parts, ws+" ("+strings.Join(fs, " ")+")")
	}
	return strings.Join(parts, ", ")
}

// factsJSON is the stored form of a worker's facts ("" = none).
func factsJSON(facts []policy.Facts) string {
	if len(facts) == 0 {
		return ""
	}
	b, err := json.Marshal(facts)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseFacts reads a worker's stored facts.
func parseFacts(raw string) []policy.Facts {
	if raw == "" {
		return nil
	}
	var out []policy.Facts
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// recordFacts writes the audit event for a change in what a worker reports
// about its runners and what contains its runs: old and new. A worker's
// first report is recorded too — it is what later changes are measured
// against.
func (s *Service) recordFacts(ctx context.Context, workerID string, before, after workerFactsRow, known bool) {
	if s.audit == nil {
		return
	}
	describe := func(w workerFactsRow) string {
		out := describeFactsList(parseFacts(w.runners))
		if w.executors != "" {
			out += "; executors: " + describeExecutors(parseExecutors(w.executors))
		}
		switch w.commands {
		case "1":
			out += "; server_commands on"
		case "0":
			out += "; server_commands off"
		}
		return out
	}
	// The runner definitions a worker holds are the server's own: applying
	// a new revision of them is its own event, not a change in what the
	// worker says about itself.
	if before.fleetRev != after.fleetRev && after.fleetRev != "" {
		_ = s.audit.Record(ctx, audit.RecordInput{EntityType: "worker", EntityID: workerID, Action: "worker.config_applied", Via: audit.ViaWorker,
			Notes: "runner definitions " + after.fleetRev + " (was " + cmp.Or(before.fleetRev, "none") + ")"})
	}
	before.fleetRev, after.fleetRev = "", ""
	if before == after && known {
		return
	}
	was := describe(before)
	if !known {
		was = "no worker row"
	}
	notes := "was: " + was + "; now: " + describe(after)
	if len(notes) > 4000 {
		notes = notes[:4000] + "…"
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: "worker", EntityID: workerID, Action: "worker.facts", Via: audit.ViaWorker, Notes: notes})
}

// describeFactsList renders runner facts as one line.
func describeFactsList(facts []policy.Facts) string {
	if len(facts) == 0 {
		return "none"
	}
	parts := make([]string, len(facts))
	for i, f := range facts {
		detail := []string{"kind=" + f.Kind, "provider=" + f.Provider}
		if f.Model != "" {
			detail = append(detail, "model="+f.Model)
		}
		if f.Endpoint != "" {
			detail = append(detail, "endpoint="+f.Endpoint)
		}
		if len(f.Stages) > 0 {
			detail = append(detail, "stages="+strings.Join(f.Stages, "|"))
		}
		parts[i] = f.Name + " (" + strings.Join(detail, " ") + ")"
	}
	return strings.Join(parts, ", ")
}

// checkExecutor compares the executor a worker reports for a finished run
// with the one the claim matched the policy on. They differ only when the
// worker's configuration changed between claim and run, or the worker
// misreports. A difference is always recorded (run.executor_mismatch);
// when the policy does not allow the runner with the executor that really
// ran, the report becomes a failure with that reason — the item is
// blocked, never silently accepted. A report without an executor (no agent
// ran in this attempt) and a run claimed without one are left alone.
//
// checkExecutor only decides: it may turn `in` into a failure, and returns
// the note to record ("" = nothing differs). The caller records it with
// recordMismatch once the report is accepted — a report refused for its
// lease or its worker writes nothing.
func (s *Service) checkExecutor(ctx context.Context, id string, in *ReportInput) string {
	if in.Executor == "" {
		return ""
	}
	cur, err := s.storage.Get(ctx, id)
	if err != nil || len(cur.Facts) == 0 {
		return ""
	}
	var rec factsRecord
	if json.Unmarshal(cur.Facts, &rec) != nil || rec.Executor == "" || rec.Executor == in.Executor {
		return ""
	}
	var pol *policy.Effective
	// As at the claim: the prefs source answers DenyAll when it knows
	// nothing, so nil here is a workspace with no settings and no policy.
	if prefs := s.prefsOf(ctx, cur.WorkspaceID); prefs != nil {
		pol = prefs.Policy
	}
	ran := rec.Facts
	ran.Executor = in.Executor
	note := fmt.Sprintf("ran %s, claimed as %s (runner %s, worker %s)", in.Executor, rec.Executor, rec.Name, in.WorkerID)
	refused := ""
	switch {
	case pol.Held():
		refused = "the policy could not be read to check it"
	default:
		if d := pol.Allows(ran, policyStage(cur.Stage)); !d.Allowed {
			refused = fmt.Sprintf("the %s policy does not allow %s there", d.Refused, in.Executor)
		}
	}
	if refused == "" {
		note += "; the policy allows both"
	} else {
		note += "; " + refused + " — the run is failed"
		reason := fmt.Sprintf("ran %s, the claim matched %s: %s", in.Executor, rec.Executor, refused)
		if in.Status == StatusFailed && in.Reason != "" {
			reason += " (the run itself failed: " + in.Reason + ")"
		}
		in.Status, in.Retryable, in.Reason = StatusFailed, false, reason
	}
	return note
}

// recordMismatch writes what checkExecutor found, for a report that was
// accepted.
func (s *Service) recordMismatch(ctx context.Context, run *Run, note string) {
	if note == "" || run == nil {
		return
	}
	slog.Warn("run executor differs from the claim", "run", run.ID, "item", run.ItemID, "stage", run.Stage, "note", note)
	if s.audit != nil {
		_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: run.WorkspaceID, EntityType: "run", EntityID: run.ID,
			Action: "run.executor_mismatch", Via: audit.ViaWorker, Notes: note})
	}
}

// strandedAfter: how long a run waits in the queue before it is judged —
// long enough for every live worker to have polled at least once.
const strandedAfter = 2 * time.Minute

// workerRecent: a worker not seen for this long does not count when
// judging whether some worker may take a run.
const workerRecent = 24 * time.Hour

// SweepStranded finds waiting runs (queued, or released for a takeover)
// that no worker may take under the policy: at least one recently seen
// worker serves the workspace, none of them passes, and the policy is what
// refuses (a runner nobody offers at all is the pipeline view's "no worker
// offers this runner", not a policy matter). A run found stranded is
// marked with the reason and recorded (run.stranded) when the mark is set
// and again if the reason changes; the mark goes when it no longer holds
// or the run stops waiting. Called by the serve loop.
func (s *Service) SweepStranded(ctx context.Context) {
	now := s.now().UTC()
	if err := s.storage.ClearStaleStranded(ctx); err != nil {
		slog.Warn("sweeping stranded runs", "err", err)
	}
	queued, err := s.storage.WaitingRuns(ctx)
	if err != nil {
		slog.Warn("sweeping stranded runs", "err", err)
		return
	}
	if len(queued) == 0 {
		return
	}
	workers, err := s.storage.WorkerOffers(ctx, now.Add(-workerRecent))
	if err != nil {
		slog.Warn("sweeping stranded runs", "err", err)
		return
	}
	for _, r := range queued {
		reason := ""
		if now.Sub(r.UpdatedAt) >= strandedAfter {
			reason = strandedReason(r, s.prefsOf(ctx, r.WorkspaceID), workers)
		}
		changed, err := s.storage.SetStranded(ctx, r.ID, reason, now)
		if err != nil {
			slog.Warn("marking a stranded run", "run", r.ID, "err", err)
			continue
		}
		if changed && reason != "" {
			slog.Warn("run stranded: no worker may take it", "run", r.ID, "item", r.ItemID, "stage", r.Stage, "reason", reason)
			if s.audit != nil {
				_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: r.WorkspaceID, EntityType: "run", EntityID: r.ID,
					Action: "run.stranded", Via: audit.ViaSystem, Notes: reason})
			}
		}
	}
}

// workerOffer is what one registered worker offers.
type workerOffer struct {
	workspaces map[string]bool
	facts      *claimFacts
}

// strandedReason says why no worker may take a waiting run under the
// policy, or "" when one may, or when policy is not what stands in the way.
func strandedReason(r *Run, prefs *WorkspacePrefs, workers []workerOffer) string {
	refused, noFacts, unavailable := "", false, ""
	for _, w := range workers {
		if !w.workspaces[r.WorkspaceID] {
			continue
		}
		d := decide(r, prefs, w.facts)
		if d.ok || d.hold {
			return ""
		}
		if d.refused != "" {
			refused = d.refused
		}
		if d.unavailable != "" {
			unavailable = d.unavailable
		}
		noFacts = noFacts || d.noFacts
	}
	switch {
	case refused != "":
		return fmt.Sprintf("no worker offers an allowed runner for %s: refused by the %s policy", r.Stage, refused)
	case unavailable != "":
		// Not the policy: a worker has the runner and cannot run the stage
		// with it under the workspace's config.
		return fmt.Sprintf("no worker can run %s: on a worker that has the runner, %s", r.Stage, policy.UnavailableReason(unavailable))
	case noFacts:
		return fmt.Sprintf("no worker may take %s: the workspace has a runner policy, and the workers serving it report no runner facts (update them)", r.Stage)
	}
	return ""
}
