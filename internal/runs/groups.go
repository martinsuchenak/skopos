package runs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Stage groups: several agents on one stage, decided once by a recorded
// policy (tmp/multi-agent-stages — design.md). Opt-in: GroupMembers is nil
// unless a workspace routes a stage to two or more members, and then
// nothing here runs.

// groupStage reports whether a stage may be fanned out as a group: review
// and qa (decided by policy) and plan (every success is a candidate).
// Implement never fans out (design §5).
func groupStage(stage string) bool {
	return stage == StageReview || stage == StageQA || stage == StagePlan
}

// queueGroup opens a group for the stage and queues one run per member, in
// one transaction. The caller already refused a duplicate active run.
func (s *Service) queueGroup(ctx context.Context, in QueueInput, members []Member) (*Run, error) {
	prefs := s.prefsOf(ctx, in.WorkspaceID)
	policy := prefs.PolicyFor(in.Stage)
	if in.Stage == StagePlan {
		policy.Policy, policy.EarlyExit = PolicyCandidates, false
		// A new plan round closes the previous one.
		if s.planRoundStart != nil {
			if err := s.planRoundStart(ctx, in.ItemID); err != nil {
				return nil, err
			}
		}
	}
	round, err := s.storage.NextRound(ctx, in.ItemID, in.Stage)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	g := Group{
		ID: ids.New(), WorkspaceID: in.WorkspaceID, ItemID: in.ItemID, Stage: in.Stage,
		Round: round, Size: len(members), Policy: policy.Policy, EarlyExit: policy.EarlyExit,
		State: GroupOpen, Deadline: now.Add(policy.Timeout), CreatedAt: now,
	}
	runs := make([]Run, 0, len(members))
	for _, m := range members {
		runs = append(runs, Run{
			ID: ids.New(), WorkspaceID: in.WorkspaceID, ItemID: in.ItemID,
			PlanRevID: strings.TrimSpace(in.PlanRevID), Stage: in.Stage,
			Runner: m.Runner, Focus: m.Focus, GroupID: g.ID,
			Branch: strings.TrimSpace(in.Branch), BaseSHA: strings.TrimSpace(in.BaseSHA),
			CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := s.storage.CreateGroup(ctx, g, runs); err != nil {
		return nil, err
	}
	return &runs[0], nil
}

// memberLabel names a member in summaries: "security · claude".
func memberLabel(r Run) string {
	label := r.Runner
	if label == "" {
		label = "any runner"
	}
	if r.Focus != "" {
		label = r.Focus + " · " + label
	}
	return label
}

// groupDecision is the outcome a group's current results imply.
type groupDecision struct {
	decide  bool   // the group can be decided now
	outcome string // pass | fail | timeout
	reason  string // item-facing reason (blocked) or summary line
}

// evaluateGroup applies the policy to the members' current results. final
// treats every non-terminal member as cancelled (the deadline passed).
func evaluateGroup(g *Group, members []Run, final bool) groupDecision {
	var passed, failed, pending int
	var failers []string
	for _, r := range members {
		switch r.Status {
		case StatusSucceeded:
			passed++
		case StatusFailed:
			failed++
			failers = append(failers, memberLabel(r))
		case StatusCancelled:
		default:
			pending++
		}
	}
	if final {
		pending = 0
	}
	failReason := fmt.Sprintf("%s group failed (%s): %s", g.Stage, g.Policy, strings.Join(failers, ", "))
	switch g.Policy {
	case PolicyCandidates:
		// Planning: every success is a candidate; the gate opens once all
		// members are done (or at the deadline) with at least one.
		if pending > 0 {
			return groupDecision{}
		}
		if passed > 0 {
			return groupDecision{true, OutcomeCandidates, ""}
		}
		if failed > 0 {
			return groupDecision{true, OutcomeFail, "plan group: no planner produced a plan (" + strings.Join(failers, ", ") + ")"}
		}
		return groupDecision{true, OutcomeTimeout, "plan group: no planner finished before the deadline"}
	case PolicyMajority:
		if g.EarlyExit && !final {
			if failed*2 > g.Size {
				return groupDecision{true, OutcomeFail, failReason}
			}
			if passed*2 >= g.Size {
				return groupDecision{true, OutcomePass, ""}
			}
		}
		if pending > 0 {
			return groupDecision{}
		}
		if passed+failed == 0 {
			return groupDecision{true, OutcomeTimeout, g.Stage + " group: no member finished before the deadline"}
		}
		if failed*2 > passed+failed {
			return groupDecision{true, OutcomeFail, failReason}
		}
		return groupDecision{true, OutcomePass, ""}
	default: // any-blocks, all-pass
		if failed > 0 && g.EarlyExit && g.Policy == PolicyAnyBlocks && !final {
			return groupDecision{true, OutcomeFail, failReason}
		}
		if pending > 0 {
			return groupDecision{}
		}
		if failed > 0 {
			return groupDecision{true, OutcomeFail, failReason}
		}
		if passed == 0 {
			return groupDecision{true, OutcomeTimeout, g.Stage + " group: no member finished before the deadline"}
		}
		return groupDecision{true, OutcomePass, ""}
	}
}

// groupMemberDone is chain() for a grouped run: record, then decide when
// the policy allows. A group decides exactly once (DecideGroup is the
// open→decided transition in one transaction).
func (s *Service) groupMemberDone(ctx context.Context, run *Run) {
	g, err := s.storage.GetGroup(ctx, run.GroupID)
	if err != nil || g.State != GroupOpen {
		return
	}
	members, err := s.storage.GroupMembers(ctx, g.ID)
	if err != nil {
		s.chainNote(ctx, run, "reading group members failed: "+err.Error())
		return
	}
	d := evaluateGroup(g, members, false)
	if !d.decide {
		return
	}
	s.applyGroupDecision(ctx, g, members, d, "group decided: "+d.outcome)
}

// SweepGroups decides open groups whose deadline passed, with whatever
// results they have; members still queued, running or paused are cancelled
// and named in the summary. Called by the serve loop.
func (s *Service) SweepGroups(ctx context.Context) {
	groups, err := s.storage.ExpiredGroups(ctx, s.now().UTC())
	if err != nil {
		slog.Warn("sweeping run groups", "err", err)
		return
	}
	for i := range groups {
		g := &groups[i]
		members, err := s.storage.GroupMembers(ctx, g.ID)
		if err != nil {
			continue
		}
		d := evaluateGroup(g, members, true)
		s.applyGroupDecision(ctx, g, members, d, "group deadline passed")
	}
}

// RunSweepLoop runs SweepGroups every interval until ctx ends.
func (s *Service) RunSweepLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepGroups(ctx)
		}
	}
}

func (s *Service) applyGroupDecision(ctx context.Context, g *Group, members []Run, d groupDecision, cancelReason string) {
	ok, err := s.storage.DecideGroup(ctx, g.ID, d.outcome, cancelReason, s.now().UTC())
	if err != nil || !ok {
		return // another report or the sweep decided it first
	}
	// Re-read: members cancelled by the decision now show as cancelled.
	if fresh, err := s.storage.GroupMembers(ctx, g.ID); err == nil {
		members = fresh
	}
	g.Outcome = d.outcome
	if s.itemAppend != nil {
		heading := groupSummaryHeading(g)
		_ = s.itemAppend(ctx, g.ItemID, heading, groupSummary(g, members))
	}
	if s.itemTransition == nil {
		return
	}
	agent := "skopos:group"
	switch d.outcome {
	case OutcomePass:
		rep := representative(members)
		s.advance(ctx, &rep, agent)
	case OutcomeCandidates:
		// Two or more candidates: the optional judge and merge run first
		// (compare → synthesize → gate); with one, straight to the gate.
		passed := 0
		for _, m := range members {
			if m.Status == StatusSucceeded {
				passed++
			}
		}
		if passed >= 2 {
			s.afterAdvisory(ctx, StagePlan, g.ItemID, g.WorkspaceID, agent)
			return
		}
		s.openPlanGate(ctx, g.ItemID, agent)
	default:
		if err := s.itemTransition(ctx, g.ItemID, "blocked", d.reason, agent); err != nil {
			slog.Warn("blocking item after group decision", "group", g.ID, "err", err)
		}
	}
}

// representative is the run whose branch/base/revision the next stage
// continues from: every member reviewed the same head, so the first
// succeeded member (else the first member) carries them.
func representative(members []Run) Run {
	for _, r := range members {
		if r.Status == StatusSucceeded {
			return r
		}
	}
	return members[0]
}

func groupSummaryHeading(g *Group) string {
	switch g.Stage {
	case StagePlan:
		return fmt.Sprintf("Plan candidates — round %d", g.Round)
	case StageQA:
		return fmt.Sprintf("QA summary — round %d", g.Round)
	}
	return fmt.Sprintf("Review summary — round %d", g.Round)
}

// groupSummary is the one section the implementer and the human read: the
// decision, each member's result, blocking findings grouped and
// deduplicated (with who found them), nits separately.
func groupSummary(g *Group, members []Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Outcome:** %s · policy `%s` · %d members\n\n", g.Outcome, g.Policy, g.Size)
	for _, r := range members {
		var blocking, nits int
		for _, f := range r.Findings {
			if f.Severity == SeverityNit {
				nits++
			} else {
				blocking++
			}
		}
		status := string(r.Status)
		switch r.Status {
		case StatusSucceeded:
			status = "pass"
			if g.Stage == StagePlan {
				status = "candidate ready"
			}
		case StatusFailed:
			status = "fail"
			if g.Stage == StagePlan {
				status = "no plan"
			}
		}
		line := fmt.Sprintf("- **%s**: %s", memberLabel(r), status)
		if blocking+nits > 0 {
			line += fmt.Sprintf(" — %d blocking, %d nit", blocking, nits)
		}
		if r.Status == StatusFailed && len(r.Findings) == 0 && r.FailReason != "" {
			line += " — " + firstLine(r.FailReason)
		}
		if r.Status == StatusCancelled && r.FailReason != "" {
			line += " (" + firstLine(r.FailReason) + ")"
		}
		b.WriteString(line + "\n")
	}
	blocking, nits := mergeFindings(members)
	if len(blocking) > 0 {
		b.WriteString("\n### Blocking\n\n")
		for _, f := range blocking {
			b.WriteString(f + "\n")
		}
	}
	if len(nits) > 0 {
		b.WriteString("\n### Nits (not blocking)\n\n")
		for _, f := range nits {
			b.WriteString(f + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// mergeFindings dedupes findings deterministically: same file and the same
// normalised text count once, tagged with every member that raised them.
func mergeFindings(members []Run) (blocking, nits []string) {
	type agg struct {
		f       Finding
		sources []string
	}
	order := map[string][]string{}
	byKey := map[string]*agg{}
	for _, r := range members {
		for _, f := range r.Findings {
			sev := SeverityBlocking
			if f.Severity == SeverityNit {
				sev = SeverityNit
			}
			key := sev + "|" + strings.TrimSpace(f.File) + "|" + normaliseFinding(f.Text)
			a, ok := byKey[key]
			if !ok {
				a = &agg{f: f}
				byKey[key] = a
				order[sev] = append(order[sev], key)
			}
			label := memberLabel(r)
			if !contains(a.sources, label) {
				a.sources = append(a.sources, label)
			}
		}
	}
	render := func(keys []string) []string {
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			a := byKey[k]
			loc := ""
			if f := strings.TrimSpace(a.f.File); f != "" {
				loc = "`" + f + "` "
			}
			sort.Strings(a.sources)
			out = append(out, fmt.Sprintf("- %s%s — _%s_", loc, strings.TrimSpace(a.f.Text), strings.Join(a.sources, ", ")))
		}
		return out
	}
	return render(order[SeverityBlocking]), render(order[SeverityNit])
}

func normaliseFinding(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "."))), " ")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// GroupView is a group with its members (dashboard / API read).
type GroupView struct {
	Group
	Members []Run `json:"members"`
}

// ListGroups returns an item's groups with members, newest first.
func (s *Service) ListGroups(ctx context.Context, itemID string) ([]GroupView, error) {
	groups, err := s.storage.ListGroups(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]GroupView, 0, len(groups))
	for _, g := range groups {
		if err := s.checkWorkspace(ctx, g.WorkspaceID); err != nil {
			continue
		}
		members, err := s.storage.GroupMembers(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, GroupView{Group: g, Members: members})
	}
	return out, nil
}

// afterAdvisory walks a multi-agent plan round after `done`: candidates →
// compare (if configured) → synthesize (if configured) → the gate. Each
// advisory stage is one run; it reads the round's candidates itself.
func (s *Service) afterAdvisory(ctx context.Context, done, itemID, workspaceID, agent string) {
	prefs := s.prefsOf(ctx, workspaceID)
	var next []string
	switch done {
	case StagePlan:
		next = []string{StageCompare, StageSynthesize}
	case StageCompare:
		next = []string{StageSynthesize}
	}
	for _, stage := range next {
		if !prefs.Configured(stage) {
			continue
		}
		if _, err := s.Queue(ctx, QueueInput{WorkspaceID: workspaceID, ItemID: itemID, Stage: stage, Single: true}); err == nil {
			return
		} else {
			slog.Warn("queueing advisory plan stage", "stage", stage, "item", itemID, "err", err)
		}
	}
	s.openPlanGate(ctx, itemID, agent)
}

// openPlanGate walks the item (planning) to awaiting_approval; the first
// move fails quietly when the item is already planning.
func (s *Service) openPlanGate(ctx context.Context, itemID, agent string) {
	if s.itemTransition == nil {
		return
	}
	_ = s.itemTransition(ctx, itemID, "planning", "", agent)
	if err := s.itemTransition(ctx, itemID, "awaiting_approval", "", agent); err != nil {
		slog.Warn("opening the plan gate", "item", itemID, "err", err)
	}
}
