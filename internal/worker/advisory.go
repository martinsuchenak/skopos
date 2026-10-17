package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// Advisory plan stages (tmp/multi-agent-stages — design.md §4): after a
// multi-agent plan round, an optional judge compares the candidates and an
// optional merge writes one more candidate built from them. Both are read-
// only and advisory: they never approve, never hide a candidate, and a
// failure leaves the gate opening with the candidates as they are.

// compareSchema: a score per candidate per criterion (1 = worst, 5 = best),
// risks, and a recommendation with reasons. The dashboard and Slack show it
// next to the candidates; nothing pre-selects it.
const compareSchema = `{"type":"object","properties":{"summary":{"type":"string"},"recommendation":{"type":"string"},"reasons":{"type":"array","items":{"type":"string"}},"scores":{"type":"array","items":{"type":"object","properties":{"candidate":{"type":"integer"},"correctness":{"type":"integer"},"scope":{"type":"integer"},"granularity":{"type":"integer"},"tests":{"type":"integer"},"fit":{"type":"integer"},"risks":{"type":"array","items":{"type":"string"}}},"required":["candidate"]}}},"required":["summary","recommendation"]}`

type compareResult struct {
	Summary        string   `json:"summary"`
	Recommendation string   `json:"recommendation"`
	Reasons        []string `json:"reasons"`
	Scores         []struct {
		Candidate   int      `json:"candidate"`
		Correctness int      `json:"correctness"`
		Scope       int      `json:"scope"`
		Granularity int      `json:"granularity"`
		Tests       int      `json:"tests"`
		Fit         int      `json:"fit"`
		Risks       []string `json:"risks"`
	} `json:"scores"`
}

// candidateView is a candidate as the advisory prompts list it.
type candidateView struct {
	N      int
	Label  string
	Steps  []RevStep
	Skills []string
}

func candidateViews(cands []Revision) []candidateView {
	out := make([]candidateView, 0, len(cands))
	for i, c := range cands {
		var steps []RevStep
		for _, st := range c.Steps {
			if st.Phase != "review" {
				steps = append(steps, st)
			}
		}
		label := c.Label
		if label == "" {
			label = "planner"
		}
		out = append(out, candidateView{N: i + 1, Label: label, Steps: steps, Skills: c.Skills})
	}
	return out
}

// advisoryContext loads what both stages read: the item, the round's open
// candidates (at least two, else there is nothing to compare), and a read-
// only checkout at the candidates' base (they share it: the round pinned it).
func (w *Worker) advisoryContext(ctx context.Context, run *Run) (*Item, []Revision, string, string, func(), error) {
	noop := func() {}
	repo := w.cfg.Repos[run.WorkspaceID]
	if repo == nil {
		return nil, nil, "", "", noop, w.failNoRepo(run.WorkspaceID)
	}
	item, err := w.client.GetItem(ctx, run.ItemID)
	if err != nil {
		return nil, nil, "", "", noop, err
	}
	if item.PlanID == "" {
		return nil, nil, "", "", noop, fmt.Errorf("%s: the item has no plan", run.Stage)
	}
	cands, group, err := w.client.RoundCandidates(ctx, item.PlanID)
	if err != nil {
		return nil, nil, "", "", noop, err
	}
	if len(cands) < 2 {
		return nil, nil, "", "", noop, fmt.Errorf("%s: fewer than two open candidates", run.Stage)
	}
	base := cands[0].BaseSHA
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", repo.DefaultBranch); err != nil {
		return nil, nil, "", "", noop, err
	}
	dir := repo.Path
	cleanup := noop
	if base != "" {
		wt := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), run.Stage+"-"+shortID(run.ID))
		removeWorktree(ctx, repo.Path, wt)
		if err := createDetached(ctx, repo.Path, wt, base); err != nil {
			return nil, nil, "", "", noop, fmt.Errorf("%s worktree at %s: %w", run.Stage, base, err)
		}
		dir = wt
		cleanup = func() { removeWorktree(context.Background(), repo.Path, wt) }
	}
	_ = group
	return item, cands, base, dir, cleanup, nil
}

// stageCompare: the judge. Writes "## Comparison" into the item; the gate
// shows it beside the candidates without pre-selecting anything.
func (w *Worker) stageCompare(ctx context.Context, run *Run, log *slog.Logger) error {
	account := w.pickAccount(run, log)
	if account == nil {
		return w.noAccount(run, "compare")
	}
	item, cands, base, dir, cleanup, err := w.advisoryContext(ctx, run)
	defer cleanup()
	if err != nil {
		return err
	}
	repo := w.cfg.Repos[run.WorkspaceID]
	prompt, err := renderPrompt("compare.md", map[string]any{
		"Title": item.Title, "Content": item.Content, "Candidates": candidateViews(cands),
		"Instructions": repoInstructions(ctx, repo.Path, base),
	})
	if err != nil {
		return err
	}
	log.Info("comparing candidates", "account", account.Name, "candidates", len(cands))
	res := w.runAgent(ctx, w.agentRun(dir, prompt, compareSchema, planTools, w.cfg.PlanTimeout, account, run, "compare"))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, "compare"); err != nil {
		return err
	}
	var cr compareResult
	if err := json.Unmarshal(res.Structured, &cr); err != nil || strings.TrimSpace(cr.Summary) == "" {
		return fmt.Errorf("compare returned no usable result: %s", firstLine(res.Text))
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Comparison — "+candidateLabel(run, account), comparisonSection(cr, candidateViews(cands)))
	return w.report(ctx, run, true, map[string]any{"summary": "comparison: " + cr.Summary})
}

// comparisonSection renders the judge's result for the item (and Slack).
func comparisonSection(cr compareResult, views []candidateView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Recommendation:** %s\n\n%s\n", strings.TrimSpace(cr.Recommendation), strings.TrimSpace(cr.Summary))
	for _, r := range cr.Reasons {
		fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(r))
	}
	if len(cr.Scores) > 0 {
		b.WriteString("\n| Candidate | Correctness | Scope | Steps | Tests | Fit |\n|---|---|---|---|---|---|\n")
		for _, sc := range cr.Scores {
			name := fmt.Sprintf("%d", sc.Candidate)
			if sc.Candidate >= 1 && sc.Candidate <= len(views) {
				name = fmt.Sprintf("%d · %s", sc.Candidate, views[sc.Candidate-1].Label)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d |\n", name, sc.Correctness, sc.Scope, sc.Granularity, sc.Tests, sc.Fit)
		}
		for _, sc := range cr.Scores {
			for _, r := range sc.Risks {
				fmt.Fprintf(&b, "\n- Risk (candidate %d): %s", sc.Candidate, strings.TrimSpace(r))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// stageSynthesize: the merge. Writes one more candidate (label
// "synthesis") into the same round; the originals stay approvable.
func (w *Worker) stageSynthesize(ctx context.Context, run *Run, log *slog.Logger) error {
	account := w.pickAccount(run, log)
	if account == nil {
		return w.noAccount(run, "synthesize")
	}
	item, cands, base, dir, cleanup, err := w.advisoryContext(ctx, run)
	defer cleanup()
	if err != nil {
		return err
	}
	repo := w.cfg.Repos[run.WorkspaceID]
	skills := repoSkills(ctx, repo.Path, base)
	prompt, err := renderPrompt("synthesize.md", map[string]any{
		"Title": item.Title, "Content": item.Content, "Candidates": candidateViews(cands),
		"Comparison":   lastSection(item.Content, "## Comparison"),
		"Instructions": repoInstructions(ctx, repo.Path, base),
		"SkillIndex":   skillIndex(skills),
	})
	if err != nil {
		return err
	}
	log.Info("synthesizing candidates", "account", account.Name, "candidates", len(cands))
	res := w.runAgent(ctx, w.agentRun(dir, prompt, planSchema, planTools, w.cfg.PlanTimeout, account, run, "synthesize"))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, "synthesize"); err != nil {
		return err
	}
	var pr planResult
	if err := json.Unmarshal(res.Structured, &pr); err != nil || len(pr.Steps) == 0 {
		return fmt.Errorf("synthesize returned no usable plan: %s", firstLine(res.Text))
	}
	_, group, err := w.client.RoundCandidates(ctx, item.PlanID)
	if err != nil || group == "" {
		return fmt.Errorf("synthesize: the round's group is gone")
	}
	selected := selectSkills(skills, pr.Skills, item.Tags)
	rev, err := w.client.CreateCandidate(ctx, item.PlanID, base, pr.Steps, selected, group, "synthesis")
	if err != nil {
		return err
	}
	enrich := fmt.Sprintf("**Candidate:** synthesis · **Size:** %s · **Revision:** %s\n\n%s\n\n### Steps\n\n%s",
		pr.Size, rev.ID, pr.Summary, numberedSteps(pr.Steps))
	_ = w.client.AppendSection(ctx, run.ItemID, "Candidate — synthesis", enrich)
	return w.report(ctx, run, true, map[string]any{"summary": "synthesis candidate ready: " + pr.Summary})
}
