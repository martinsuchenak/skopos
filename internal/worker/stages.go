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

	// Knowledge at the base the planner reads (never a worktree head).
	skills := repoSkills(ctx, repo.Path, baseSHA)
	prompt, err := renderPrompt("plan.md", map[string]any{
		"Workspace": run.WorkspaceID, "Base": repo.DefaultBranch,
		"Title": item.Title, "Content": item.Content, "Notes": lastSection(item.Content, "## Review notes"),
		"Instructions": repoInstructions(ctx, repo.Path, baseSHA),
		"SkillIndex":   skillIndex(skills), "Requested": tagHints(skills, item.Tags),
	})
	if err != nil {
		return err
	}
	log.Info("planning", "account", account.Name, "base", baseSHA)
	res := w.runAgent(ctx, w.agentRun(repo.Path, prompt, planSchema, planTools, w.cfg.PlanTimeout, account, run, "plan"))
	if err := w.handleAgentOutcome(ctx, run, res, account, "plan"); err != nil {
		return err
	}

	var pr planResult
	if err := json.Unmarshal(res.Structured, &pr); err != nil || (!pr.NeedsClarification && len(pr.Steps) == 0) {
		return fmt.Errorf("planner returned no usable plan: %s", firstLine(res.Text))
	}
	if pr.NeedsClarification {
		_ = w.client.AppendSection(ctx, run.ItemID, "Questions — planner", bullets(pr.Questions))
		return w.report(ctx, run, false, map[string]any{
			"reason": "needs clarification: " + bullets(pr.Questions), "summary": res.Text[:min(len(res.Text), 500)],
		})
	}

	branch := normalizeBranch(item.ID, item.Title, repo.BranchPrefix)
	// Single atomic path (round-5 M6): ensure a linked plan exists, then
	// ONE replan call replaces all steps and snapshots. No piecemeal
	// Add/Delete — a failure can no longer gut the plan.
	planID := item.PlanID
	if planID == "" {
		plan, err := w.client.CreatePlan(ctx, run.WorkspaceID, pr.Title, branch, w.cfg.ID)
		if err != nil {
			return err
		}
		planID = plan.ID
		if err := w.client.LinkPlan(ctx, run.ItemID, planID); err != nil {
			return err // empty unlinked plan at worst — harmless, GC-able
		}
	}
	// The selection is resolved HERE, once: the planner's picks plus the
	// tag hints, bound into the revision the approval names.
	selected := selectSkills(skills, pr.Skills, item.Tags)
	rev, err := w.client.Replan(ctx, planID, baseSHA, pr.Steps, selected)
	if err != nil {
		return err
	}
	enrich := fmt.Sprintf("**Plan:** %s · **Size:** %s · **Branch:** `%s` · **Base:** `%s` · **Revision:** %s\n\n%s\n\n### Steps\n\n%s",
		planID, pr.Size, branch, baseSHA[:min(10, len(baseSHA))], rev.ID, pr.Summary, numberedSteps(pr.Steps))
	if len(selected) > 0 {
		enrich += "\n**Skills:** " + strings.Join(selected, ", ") + "\n"
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Enrichment — skopos worker", enrich)

	log.Info("plan ready", "plan", planID, "revision", rev.ID, "steps", len(pr.Steps), "cost_usd", res.CostUSD)
	return w.report(ctx, run, true, map[string]any{
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
	// H3: the run carries the approved revision and base. Branch from the
	// approved base_sha — "what runs is exactly what was approved" — not
	// the default branch's moving tip.
	baseRef := run.BaseSHA
	if baseRef == "" {
		baseRef = "origin/" + repo.DefaultBranch
	}
	// H3: the steps come from the linked plan (the revision snapshot), not
	// scraped from item content — after a revise the content holds several
	// Enrichment sections and the agent could implement an older plan.
	planDetail, perr := w.client.GetPlan(ctx, item.PlanID)
	steps := []Step{}
	if perr == nil {
		for _, pi := range planDetail.Items {
			if pi.Title == "Review and push (Martin)" {
				continue
			}
			steps = append(steps, Step{Title: pi.Title, Description: pi.Description})
		}
	}
	worktree := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(branch, "/", "-"))
	if !dirExists(worktree) {
		if err := createWorktreeAt(ctx, repo.Path, worktree, branch, baseRef); err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		if err := runSetup(ctx, worktree, repo.Setup, repo.Path, repo.SetupEnv, w.cfg.TokenEnvs(), w.envFileKeys, 10*time.Minute); err != nil {
			removeWorktree(ctx, repo.Path, worktree)
			return err
		}
	}

	instructions, skillBodies := w.runKnowledge(ctx, repo, run, item.PlanID, baseRef)
	prompt, err := renderPrompt("implement.md", map[string]any{
		"Branch": branch, "Base": repo.DefaultBranch, "Title": item.Title,
		"Content": item.Content, "Steps": steps, "Notes": lastSection(item.Content, "## Change requests"),
		"ApprovalNotes": lastSection(item.Content, "## Approval notes"),
		"Instructions":  instructions, "Skills": skillBodies,
	})
	if err != nil {
		return err
	}
	log.Info("implementing", "account", account.Name, "branch", branch, "worktree", worktree)
	res := w.runAgent(ctx, w.agentRun(worktree, prompt, implSchema, implTools, w.cfg.ImplTimeout, account, run, "implement"))
	if err := w.handleAgentOutcome(ctx, run, res, account, "implement"); err != nil {
		return err
	}

	var ir implResult
	if err := json.Unmarshal(res.Structured, &ir); err != nil {
		return fmt.Errorf("no structured result: %s", firstLine(res.Text))
	}
	switch ir.Status {
	case "amendment":
		return w.proposeAmendment(ctx, run, item, ir)
	case "blocked":
		reason := ir.Reason
		if reason == "" {
			reason = ir.Summary
		}
		return w.report(ctx, run, false, map[string]any{"reason": reason})
	}

	if commitCount(ctx, worktree, repo.DefaultBranch) == 0 {
		return fmt.Errorf("agent reported done but made no commits")
	}
	checks := runChecks(ctx, worktree, repo.Checks, w.cfg.TokenEnvs(), w.envFileKeys)
	head := headSHA(ctx, worktree)

	// The push: validated refspec, env-only token, never force.
	if repo.Push.Kind != "none" {
		token := os.Getenv(repo.Push.TokenEnv)
		if token == "" {
			return fmt.Errorf("push token env %s is empty", repo.Push.TokenEnv)
		}
		if err := pushBranch(ctx, worktree, repo.Push.RemoteURL, token, repo.Push.Kind, branch, repo.BranchPrefix, repo.Protected); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}

	log.Info("implementation finished", "head", head, "commits", commitCount(ctx, worktree, repo.DefaultBranch), "cost_usd", res.CostUSD)
	return w.report(ctx, run, true, map[string]any{
		"summary": ir.Summary, "head_sha": head, "branch": branch,
		"diffstat": diffStat(ctx, worktree, repo.DefaultBranch), "checks": checks,
	})
}

// reviewSchema makes the verdict machine-checkable (review H6: a substring
// test let "2 issues found." pass).
const reviewSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["pass","fail"]},"summary":{"type":"string"},"issues":{"type":"array","items":{"type":"string"}}},"required":["verdict","summary"]}`

type reviewResult struct {
	Verdict string   `json:"verdict"`
	Summary string   `json:"summary"`
	Issues  []string `json:"issues"`
}

// stageReview: a second opinion over the pushed branch (the review/QA
// stages — the account pool makes this a DIFFERENT agent than the
// implementer). Always reviews a fresh checkout of the pushed branch, never
// the clone's default branch where the diff would be empty.
func (w *Worker) stageReview(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.cfg.Repos[run.WorkspaceID]
	if repo == nil {
		return w.failNoRepo(run.WorkspaceID)
	}
	if run.Branch == "" {
		return fmt.Errorf("review run has no branch to review")
	}
	account := w.pickAccount(run, log)
	if account == nil {
		return &quotaError{detail: "no available account for review"}
	}
	// Fresh checkout of the pushed branch: fetch, then a detached worktree
	// at origin/<branch>. If the fetch finds nothing the review fails — an
	// empty diff is not a pass.
	wt := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(run.Branch, "/", "-")+"-review")
	removeWorktree(ctx, repo.Path, wt)
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", run.Branch); err != nil {
		return fmt.Errorf("review: branch %s not pushed: %w", run.Branch, err)
	}
	if err := createDetached(ctx, repo.Path, wt, "origin/"+run.Branch); err != nil {
		return fmt.Errorf("review worktree: %w", err)
	}
	defer removeWorktree(ctx, repo.Path, wt)
	base := run.BaseSHA
	if base == "" {
		base = "origin/" + repo.DefaultBranch
	}
	if err := restoreInstructionFiles(ctx, wt, base); err != nil {
		return fmt.Errorf("review: restoring instruction files to %s: %w", base, err)
	}

	// Cheap pre-check (round-2 low): an empty diff fails without an
	// agent run.
	if _, err := git(ctx, wt, "diff", "--quiet", "origin/"+repo.DefaultBranch+"...HEAD"); err == nil {
		return fmt.Errorf("review: empty diff on %s", run.Branch)
	}
	// The plan the review checks against, and the knowledge at the
	// APPROVED base — never this worktree, which is the implementer's
	// branch (it could otherwise brief its own reviewer).
	var steps []Step
	var instructions string
	var skillBodies []Skill
	if item, ierr := w.client.GetItem(ctx, run.ItemID); ierr == nil && item.PlanID != "" {
		if plan, perr := w.client.GetPlan(ctx, item.PlanID); perr == nil {
			for _, pi := range plan.Items {
				if pi.Title == "Review and push (Martin)" {
					continue
				}
				steps = append(steps, Step{Title: pi.Title, Description: pi.Description})
			}
		}
		instructions, skillBodies = w.runKnowledge(ctx, repo, run, item.PlanID, base)
	}
	prompt, err := renderPrompt("review.md", map[string]any{
		"Base": repo.DefaultBranch, "Stage": run.Stage, "Steps": steps,
		"Instructions": instructions, "Skills": skillBodies,
	})
	if err != nil {
		return err
	}
	log.Info("reviewing", "account", account.Name, "stage", run.Stage, "branch", run.Branch)
	res := w.runAgent(ctx, w.agentRun(wt, prompt, reviewSchema, reviewTools, w.cfg.PlanTimeout, account, run, run.Stage))
	if err := w.handleAgentOutcome(ctx, run, res, account, run.Stage); err != nil {
		return err
	}
	var rr reviewResult
	if err := json.Unmarshal(res.Structured, &rr); err != nil || rr.Verdict == "" {
		return fmt.Errorf("review returned no verdict: %s", firstLine(res.Text))
	}
	passed := rr.Verdict == "pass"
	_ = w.client.AppendSection(ctx, run.ItemID, run.Stage+" — "+account.Name,
		fmt.Sprintf("**Verdict:** %s\n\n%s", rr.Verdict, rr.Summary))
	log.Info("review finished", "verdict", rr.Verdict, "issues", len(rr.Issues), "cost_usd", res.CostUSD)
	if !passed {
		return w.report(ctx, run, false, map[string]any{
			"reason": "review failed: " + rr.Summary, "summary": rr.Summary,
		})
	}
	return w.report(ctx, run, true, map[string]any{
		"summary": rr.Summary, "checks": []Check{{Name: run.Stage + ":" + account.Name, Passed: true, Output: rr.Summary}},
	})
}

// proposeAmendment is the amendment gate (1b round-2 N2): the item returns
// to awaiting_approval with the amended steps snapshotted over the locked
// revision in ONE server transaction, and the run STAYS PAUSED — never
// auto-reclaimed. Martin's approve resumes it. The amendment names the
// RUN's base, not the default branch tip.
func (w *Worker) proposeAmendment(ctx context.Context, run *Run, item *Item, ir implResult) error {
	if ir.Reason == "" {
		ir.Reason = ir.Summary
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Amendment proposed — worker", bullets([]string{ir.Reason}))
	base := run.BaseSHA
	if base == "" {
		base, _ = git(ctx, w.cfg.Repos[run.WorkspaceID].Path, "rev-parse", "origin/"+w.cfg.Repos[run.WorkspaceID].DefaultBranch)
		base = strings.TrimSpace(base)
	}
	// Amend FIRST (the guard accepts implementing), THEN move the item to
	// the gate: an amend failure no longer strands the item at the gate
	// with the old revision (round-3 low) — the run reports failed from
	// implementing, a valid edge.
	if _, err := w.client.Amend(ctx, item.PlanID, base, ir.AmendedSteps); err != nil {
		return err
	}
	if err := w.client.Transition(ctx, run.ItemID, "awaiting_approval", "amendment: "+firstLine(ir.Reason)); err != nil {
		return err
	}
	return w.client.Pause(ctx, run.ID, w.cfg.ID, "amendment proposed: "+firstLine(ir.Reason), time.Time{})
}

// runKnowledge is what an implement/review/qa prompt carries: the repo
// instructions and the bodies of the APPROVED revision's skills, both read
// from the clone at base (capability profiles §3).
func (w *Worker) runKnowledge(ctx context.Context, repo *Repo, run *Run, planID, base string) (string, []Skill) {
	instructions := repoInstructions(ctx, repo.Path, base)
	if planID == "" {
		return instructions, nil
	}
	rev, err := w.client.ApprovedRevision(ctx, planID, run.PlanRevID)
	if err != nil || rev == nil {
		return instructions, nil
	}
	return instructions, approvedSkills(ctx, repo.Path, base, rev.Skills)
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
	Skills             []string `json:"skills"`
	Title              string   `json:"title"`
	Summary            string   `json:"summary"`
	Size               string   `json:"size"`
	NeedsClarification bool     `json:"needs_clarification"`
	Steps              []Step   `json:"steps"`
	Risks              []string `json:"risks"`
	Questions          []string `json:"questions"`
}

type implResult struct {
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	Summary      string `json:"summary"`
	AmendedSteps []Step `json:"amended_steps"`
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
