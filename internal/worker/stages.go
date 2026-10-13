package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagePlan: run the planner in the clone (read-only), write the plan +
// revision (bound to the base the planner read), link it. The queue's
// chaining moves the item to awaiting_approval on the success report.
func (w *Worker) stagePlan(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.cfg.Repos[run.WorkspaceID]
	if repo == nil {
		return w.failNoRepo(run.WorkspaceID)
	}
	item, err := w.client.GetItem(ctx, run.ItemID)
	if err != nil {
		return err
	}
	account := w.pickAccount(run, log)
	if account == nil {
		return &quotaError{detail: "no available account for planning"}
	}

	// The planner reads the clone's working tree at the base branch; the
	// recorded base binds Martin's approval to exactly what was read.
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", repo.DefaultBranch); err != nil {
		return err
	}
	if _, err := git(ctx, repo.Path, "merge", "--ff-only", "--quiet", "origin/"+repo.DefaultBranch); err != nil {
		return fmt.Errorf("clone not fast-forwardable to origin/%s: %w", repo.DefaultBranch, err)
	}
	baseSHA, _ := git(ctx, repo.Path, "rev-parse", "origin/"+repo.DefaultBranch)

	prompt, err := renderPrompt("plan.md", map[string]any{
		"Workspace": run.WorkspaceID, "Base": repo.DefaultBranch,
		"Title": item.Title, "Content": item.Content, "Notes": lastSection(item.Content, "## Review notes"),
	})
	if err != nil {
		return err
	}
	log.Info("planning", "account", account.Name, "base", baseSHA)
	res := runClaude(ctx, w.agentRun(w.cfg.RunnerBin(run.Runner), repo.Path, prompt, planSchema, planTools, w.cfg.PlanTimeout, account, run, "plan"))
	if err := w.handleAgentOutcome(ctx, run, res, account, "plan"); err != nil {
		return err
	}

	var pr planResult
	if err := json.Unmarshal(res.Structured, &pr); err != nil || (!pr.NeedsClarification && len(pr.Steps) == 0) {
		return fmt.Errorf("planner returned no usable plan: %s", firstLine(res.Text))
	}
	if pr.NeedsClarification {
		_ = w.client.AppendSection(ctx, run.ItemID, "Questions — planner", bullets(pr.Questions))
		return w.client.ReportResult(ctx, run.ID, w.cfg.ID, false, map[string]any{
			"reason": "needs clarification: " + bullets(pr.Questions), "summary": res.Text[:min(len(res.Text), 500)],
		})
	}

	branch := normalizeBranch(item.ID, item.Title, repo.BranchPrefix)
	plan, err := w.client.CreatePlan(ctx, run.WorkspaceID, pr.Title, branch, w.cfg.ID)
	if err != nil {
		return err
	}
	for _, s := range pr.Steps {
		if _, err := w.client.AddPlanItem(ctx, plan.ID, s.Title, s.Description, "implement"); err != nil {
			return err
		}
	}
	if _, err := w.client.AddPlanItem(ctx, plan.ID, "Review and push (Martin)", "Martin reviews the branch and pushes it onward himself.", "review"); err != nil {
		return err
	}
	if err := w.client.LinkPlan(ctx, run.ItemID, plan.ID); err != nil {
		return err
	}
	rev, err := w.client.CreateRevision(ctx, plan.ID, baseSHA)
	if err != nil {
		return err
	}
	enrich := fmt.Sprintf("**Plan:** %s · **Size:** %s · **Branch:** `%s` · **Base:** `%s` · **Revision:** %s\n\n%s\n\n### Steps\n\n%s",
		plan.ID, pr.Size, branch, baseSHA[:min(10, len(baseSHA))], rev.ID, pr.Summary, numberedSteps(pr.Steps))
	_ = w.client.AppendSection(ctx, run.ItemID, "Enrichment — skopos worker", enrich)

	log.Info("plan ready", "plan", plan.ID, "revision", rev.ID, "steps", len(pr.Steps), "cost_usd", res.CostUSD)
	return w.client.ReportResult(ctx, run.ID, w.cfg.ID, true, map[string]any{
		"summary": "plan ready: " + pr.Summary, "branch": branch, "base_sha": baseSHA,
	})
}

// stageImplement: worktree from the approved base, implement per the locked
// plan, run checks, push the branch, report the head.
func (w *Worker) stageImplement(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.cfg.Repos[run.WorkspaceID]
	if repo == nil {
		return w.failNoRepo(run.WorkspaceID)
	}
	item, err := w.client.GetItem(ctx, run.ItemID)
	if err != nil {
		return err
	}
	if item.PlanID == "" {
		return fmt.Errorf("item has no linked plan; cannot implement")
	}
	account := w.pickAccount(run, log)
	if account == nil {
		return &quotaError{detail: "no available account for implementation"}
	}

	branch := run.Branch
	if branch == "" {
		branch = normalizeBranch(item.ID, item.Title, repo.BranchPrefix)
	}
	worktree := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(branch, "/", "-"))
	if _, err := git(ctx, repo.Path, "worktree", "list", "--porcelain"); err == nil {
		// reuse an existing worktree for this branch (resume) if present
		if !dirExists(worktree) {
			if err := createWorktree(ctx, repo.Path, worktree, branch, repo.DefaultBranch); err != nil {
				return fmt.Errorf("worktree: %w", err)
			}
			if err := runSetup(ctx, worktree, repo.Setup, repo.Path, 10*time.Minute); err != nil {
				removeWorktree(ctx, repo.Path, worktree)
				return err
			}
		}
	}

	// The approved plan: read from the item's enrichment (the locked
	// revision content rides on the item after planning).
	steps := numberedFromContent(item.Content)
	prompt, err := renderPrompt("implement.md", map[string]any{
		"Branch": branch, "Base": repo.DefaultBranch, "Title": item.Title,
		"Content": item.Content, "Steps": steps, "Notes": lastSection(item.Content, "## Change requests"),
		"ApprovalNotes": lastSection(item.Content, "## Approval notes"),
	})
	if err != nil {
		return err
	}
	log.Info("implementing", "account", account.Name, "branch", branch, "worktree", worktree)
	res := runClaude(ctx, w.agentRun(w.cfg.RunnerBin(run.Runner), worktree, prompt, implSchema, implTools, w.cfg.ImplTimeout, account, run, "implement"))
	if err := w.handleAgentOutcome(ctx, run, res, account, "implement"); err != nil {
		return err
	}

	var ir implResult
	if err := json.Unmarshal(res.Structured, &ir); err != nil {
		return fmt.Errorf("no structured result: %s", firstLine(res.Text))
	}
	switch ir.Status {
	case "amendment":
		_ = w.client.AppendSection(ctx, run.ItemID, "Amendment proposed — worker", bullets([]string{ir.Reason}))
		return w.pauseForAmendment(ctx, run, ir)
	case "blocked":
		reason := ir.Reason
		if reason == "" {
			reason = ir.Summary
		}
		return w.client.ReportResult(ctx, run.ID, w.cfg.ID, false, map[string]any{"reason": reason})
	}

	if commitCount(ctx, worktree, repo.DefaultBranch) == 0 {
		return fmt.Errorf("agent reported done but made no commits")
	}
	checks := runChecks(ctx, worktree, repo.Checks)
	head := headSHA(ctx, worktree)

	// The push: validated refspec, env-only token, never force.
	if repo.Push.Kind != "none" {
		token := os.Getenv(repo.Push.TokenEnv)
		if token == "" {
			return fmt.Errorf("push token env %s is empty", repo.Push.TokenEnv)
		}
		if err := pushBranch(ctx, worktree, repo.Push.RemoteURL, token, repo.Push.Kind, branch, repo.Protected); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}

	log.Info("implementation finished", "head", head, "commits", commitCount(ctx, worktree, repo.DefaultBranch), "cost_usd", res.CostUSD)
	return w.client.ReportResult(ctx, run.ID, w.cfg.ID, true, map[string]any{
		"summary": ir.Summary, "head_sha": head, "branch": branch,
		"diffstat": diffStat(ctx, worktree, repo.DefaultBranch), "checks": checks,
	})
}

// stageReview: a second opinion over the diff (the review/QA stages — the
// account pool makes this a DIFFERENT agent than the implementer).
func (w *Worker) stageReview(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.cfg.Repos[run.WorkspaceID]
	if repo == nil {
		return w.failNoRepo(run.WorkspaceID)
	}
	account := w.pickAccount(run, log)
	if account == nil {
		return &quotaError{detail: "no available account for review"}
	}
	worktree := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(orDefault(run.Branch, "review"), "/", "-"))
	prompt, err := renderPrompt("review.md", map[string]any{
		"Base": repo.DefaultBranch, "Stage": run.Stage,
	})
	if err != nil {
		return err
	}
	dir := worktree
	if !dirExists(dir) {
		dir = repo.Path
	}
	log.Info("reviewing", "account", account.Name, "stage", run.Stage)
	res := runClaude(ctx, w.agentRun(w.cfg.RunnerBin(run.Runner), dir, prompt, "", reviewTools, w.cfg.PlanTimeout, account, run, run.Stage))
	if err := w.handleAgentOutcome(ctx, run, res, account, run.Stage); err != nil {
		return err
	}
	verdict := firstLine(res.Text)
	_ = w.client.AppendSection(ctx, run.ItemID, strings.Title(run.Stage)+" — "+account.Name, res.Text)
	passed := !strings.Contains(strings.ToLower(res.Text), "fail") || strings.Contains(strings.ToLower(verdict), "pass")
	log.Info("review finished", "verdict", verdict, "cost_usd", res.CostUSD)
	return w.client.ReportResult(ctx, run.ID, w.cfg.ID, passed, map[string]any{
		"summary": verdict, "checks": []Check{{Name: run.Stage + ":" + account.Name, Passed: passed, Output: verdict}},
	})
}

func (w *Worker) pauseForAmendment(ctx context.Context, run *Run, ir implResult) error {
	return w.client.Pause(ctx, run.ID, w.cfg.ID, "amendment proposed: "+firstLine(ir.Reason))
}

func (w *Worker) failNoRepo(workspaceID string) error {
	return fmt.Errorf("no local clone configured for workspace %s (worker config repos)", workspaceID)
}

// --- plan/implement result shapes (schemas above) ---

type Step struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

type planResult struct {
	Title              string   `json:"title"`
	Summary            string   `json:"summary"`
	Size               string   `json:"size"`
	NeedsClarification bool     `json:"needs_clarification"`
	Steps              []Step   `json:"steps"`
	Risks              []string `json:"risks"`
	Questions          []string `json:"questions"`
}

type implResult struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
}

// --- small helpers ---

func lastSection(content, heading string) string {
	idx := strings.LastIndex(content, heading)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(content[idx:])
}

func numberedSteps(steps []Step) string {
	var b strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&b, "%d. **%s** — %s\n", i+1, s.Title, s.Description)
	}
	return b.String()
}

func numberedFromContent(content string) []Step {
	// The enrichment section carries the numbered steps; the implementer
	// prompt receives the whole item content anyway.
	return nil
}

func bullets(xs []string) string {
	var b strings.Builder
	for _, x := range xs {
		fmt.Fprintf(&b, "- %s\n", x)
	}
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
