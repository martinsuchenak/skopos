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
		return w.noAccount(run, "planning")
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

	// A multi-agent plan round: every candidate plans at the group's pinned
	// base (the first member's), so candidates differ in judgement, not in
	// what they read. When the clone has moved on, plan in a detached
	// worktree at the pinned commit.
	dir := repo.Path
	if run.GroupID != "" {
		pinned, perr := w.client.PinBase(ctx, run.ID, baseSHA)
		if perr != nil {
			return fmt.Errorf("pinning the plan group's base: %w", perr)
		}
		if pinned != "" && pinned != baseSHA {
			wt := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), "plan-"+shortID(run.ID))
			removeWorktree(ctx, repo.Path, wt)
			if err := createDetached(ctx, repo.Path, wt, pinned); err != nil {
				return fmt.Errorf("plan worktree at pinned base %s: %w", pinned, err)
			}
			defer removeWorktree(ctx, repo.Path, wt)
			dir, baseSHA = wt, pinned
		}
	}
	// Optional focus brief (plan members may have one), read at the base.
	var focus *Skill
	if run.Focus != "" {
		f, ferr := repoFocus(ctx, repo.Path, baseSHA, run.Focus)
		if ferr != nil {
			return w.report(ctx, run, false, map[string]any{"reason": "plan: " + ferr.Error()})
		}
		focus = &f
	}
	// Revising one candidate at a multi-agent gate: start from it.
	var seed *Revision
	if run.PlanRevID != "" && item.PlanID != "" {
		if rev, rerr := w.client.ApprovedRevision(ctx, item.PlanID, run.PlanRevID); rerr == nil && rev != nil {
			seed = rev
		}
	}
	var seedSteps []RevStep
	if seed != nil {
		for _, st := range seed.Steps {
			if st.Phase != "review" {
				seedSteps = append(seedSteps, st)
			}
		}
	}

	// Knowledge at the base the planner reads (never a worktree head).
	skills := repoSkills(ctx, repo.Path, baseSHA)
	prompt, err := renderPrompt("plan.md", map[string]any{
		"Workspace": run.WorkspaceID, "Base": repo.DefaultBranch,
		"Title": item.Title, "Content": item.Content, "Notes": lastSection(item.Content, "## Review notes"),
		"Instructions": repoInstructions(ctx, repo.Path, baseSHA),
		"SkillIndex":   skillIndex(skills), "Requested": tagHints(skills, item.Tags),
		"ImplCommands": w.stageCommands(run.WorkspaceID, "implement", implTools), "Checks": repo.Checks,
		"Focus": focus, "Seed": seedSteps,
	})
	if err != nil {
		return err
	}
	log.Info("planning", "account", account.Name, "base", baseSHA, "group", run.GroupID != "", "seeded", seed != nil)
	res := w.runAgent(ctx, w.agentRun(dir, prompt, planSchema, planTools, w.cfg.PlanTimeout, account, run, "plan"))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, "plan"); err != nil {
		return err
	}

	var pr planResult
	if err := json.Unmarshal(res.Structured, &pr); err != nil || (!pr.NeedsClarification && len(pr.Steps) == 0) {
		return fmt.Errorf("planner returned no usable plan: %s", firstLine(res.Text))
	}
	label := candidateLabel(run, account)
	if pr.NeedsClarification {
		heading := "Questions — planner"
		if run.GroupID != "" {
			heading = "Questions — planner " + label
		}
		_ = w.client.AppendSection(ctx, run.ItemID, heading, bullets(pr.Questions))
		return w.report(ctx, run, false, map[string]any{
			"reason": "needs clarification: " + bullets(pr.Questions), "summary": res.Text[:min(len(res.Text), 500)],
		})
	}

	branch := normalizeBranch(item.ID, item.Title, repo.BranchPrefix)
	if run.GroupID != "" {
		return w.submitCandidate(ctx, run, item, pr, skills, baseSHA, branch, label, res, log)
	}
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

// candidateLabel names a plan group member's candidate: "focus@runner" or
// "runner" (the same syntax as the routing config).
func candidateLabel(run *Run, account *Account) string {
	runner := run.Runner
	if runner == "" && account != nil {
		runner = account.Runner
	}
	if runner == "" {
		runner = "claude"
	}
	if run.Focus != "" {
		return run.Focus + "@" + runner
	}
	return runner
}

// submitCandidate writes a plan group member's result as a candidate
// revision. The server created and linked the plan when the first member
// was claimed; the live steps stay untouched until Martin approves one.
func (w *Worker) submitCandidate(ctx context.Context, run *Run, item *Item, pr planResult, skills []Skill, baseSHA, branch, label string, res AgentResult, log *slog.Logger) error {
	planID := item.PlanID
	if planID == "" {
		if fresh, err := w.client.GetItem(ctx, run.ItemID); err == nil {
			planID = fresh.PlanID
		}
	}
	if planID == "" {
		return fmt.Errorf("plan group: the item has no linked plan yet")
	}
	selected := selectSkills(skills, pr.Skills, item.Tags)
	rev, err := w.client.CreateCandidate(ctx, planID, baseSHA, pr.Steps, selected, run.GroupID, label)
	if err != nil {
		return err
	}
	enrich := fmt.Sprintf("**Candidate:** %s · **Size:** %s · **Base:** `%s` · **Revision:** %s\n\n%s\n\n### Steps\n\n%s",
		label, pr.Size, baseSHA[:min(10, len(baseSHA))], rev.ID, pr.Summary, numberedSteps(pr.Steps))
	if len(selected) > 0 {
		enrich += "\n**Skills:** " + strings.Join(selected, ", ") + "\n"
	}
	if len(pr.Risks) > 0 {
		enrich += "\n**Risks:**\n" + bullets(pr.Risks) + "\n"
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Candidate — "+label, enrich)
	log.Info("candidate ready", "plan", planID, "revision", rev.ID, "label", label, "steps", len(pr.Steps), "cost_usd", res.CostUSD)
	return w.report(ctx, run, true, map[string]any{
		"summary": "candidate ready: " + pr.Summary, "branch": branch, "base_sha": baseSHA,
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
		return w.noAccount(run, "implementation")
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
		if err := prepareImplementWorktree(ctx, repo.Path, worktree, branch, baseRef); err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		if err := runSetup(ctx, worktree, repo.Setup, repo.Path, repo.SetupEnv, w.cfg.TokenEnvs(), w.envFileKeys, 10*time.Minute); err != nil {
			removeWorktree(ctx, repo.Path, worktree)
			return err
		}
	}
	// What this item leaves on the worker, for the cleanup sweep. A later
	// round keeps the pull request the first one opened, but not Pushed:
	// until this round pushes, its commits exist only here.
	rec, _ := w.loadBranch(item.ID)
	rec.ItemID, rec.WorkspaceID, rec.Branch, rec.Worktree, rec.Pushed = item.ID, run.WorkspaceID, branch, worktree, false
	w.saveBranch(rec)

	// A run resumed after a pause during a fix round (quota) already has
	// its implementation: it goes straight back to the checks, with the
	// fix rounds it has used.
	var ir implResult
	if rec.ImplementedRun == run.ID {
		ir = implResult{Status: "done", Summary: rec.ImplementedSummary}
		log.Info("resuming the check fixes", "fix_rounds_used", rec.FixRounds)
	} else {
		instructions, skillBodies := w.runKnowledge(ctx, repo, run, item.PlanID, baseRef)
		prompt, err := renderPrompt("implement.md", map[string]any{
			"Branch": branch, "Base": repo.DefaultBranch, "Title": item.Title,
			"Content": item.Content, "Steps": steps, "Notes": lastSection(item.Content, "## Change requests"),
			"ApprovalNotes": lastSection(item.Content, "## Approval notes"),
			"Instructions":  instructions, "Skills": skillBodies,
			"Commands": w.stageCommands(run.WorkspaceID, "implement", implTools), "Checks": repo.Checks,
		})
		if err != nil {
			return err
		}
		log.Info("implementing", "account", account.Name, "branch", branch, "worktree", worktree)
		res := w.runAgent(ctx, w.agentRun(worktree, prompt, implSchema, implTools, w.cfg.ImplTimeout, account, run, "implement"))
		w.addCost(run, res)
		if err := w.handleAgentOutcome(ctx, run, res, account, "implement"); err != nil {
			return err
		}
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
		rec.ImplementedRun, rec.ImplementedSummary, rec.FixRounds = run.ID, ir.Summary, 0
		w.saveBranch(rec)
	}
	// The checks are the CI: a failure goes back to the implementer (the
	// same session) for a fix round, then the checks run again. A round
	// counts once it finished, so a quota pause repeats it on resume.
	cantFix := ""
	checks, _, stopped, stopErr := fixChecks(repo.FixRounds()-rec.FixRounds,
		func() []Check { return w.repoChecks(ctx, repo, worktree) },
		func(failed []Check, _ int) (bool, error) {
			round := rec.FixRounds + 1
			log.Info("checks failed; asking the implementer to fix them", "round", round, "failed", checkNames(failed))
			fix, err := w.fixChecksRound(ctx, run, item, repo, worktree, branch, account, failed, round)
			if err != nil {
				return true, err
			}
			rec.FixRounds = round
			w.saveBranch(rec)
			switch fix.Status {
			case "amendment":
				// Approving it resumes this run with new steps to implement:
				// not straight at the checks.
				rec.ImplementedRun = ""
				w.saveBranch(rec)
				return true, w.proposeAmendment(ctx, run, item, *fix)
			case "blocked":
				// Like a failure that outlasts the rounds: pushed and
				// recorded with its checks, so it can be looked at.
				cantFix = fix.Reason
				if cantFix == "" {
					cantFix = fix.Summary
				}
				return true, nil
			}
			return false, nil
		})
	if stopped && cantFix == "" {
		return stopErr
	}
	rounds := rec.FixRounds
	head := headSHA(ctx, worktree)
	failed := failedChecks(checks)

	// The push: validated refspec, env-only token, never force. A branch
	// whose checks still fail is pushed too, so it can be looked at.
	summary := ir.Summary
	if rounds > 0 && len(failed) == 0 {
		summary += fmt.Sprintf("\n\nThe checks failed at first and were fixed in %s.", plural(rounds, "fix round"))
	}
	if repo.Push.Kind != "none" {
		token := os.Getenv(repo.Push.TokenEnv)
		if token == "" {
			return fmt.Errorf("push token env %s is empty", repo.Push.TokenEnv)
		}
		if err := pushBranch(ctx, worktree, repo.Push.RemoteURL, token, repo.Push.Kind, branch, repo.BranchPrefix, repo.Protected); err != nil {
			return fmt.Errorf("push: %w", err)
		}
		rec.Pushed = true
		// The pull request is a convenience on top of the push: failing to
		// open one is noted in the summary and never fails the run.
		if repo.Push.PullRequest == "draft" || repo.Push.PullRequest == "ready" {
			// A branch that fails its checks opens as a draft, whatever
			// pull_request says: it isn't ready for anyone's review.
			pr, action, err := w.ensurePullRequest(ctx, repo, token, branch, item, steps, ir.Summary, repo.Push.PullRequest == "draft" || len(failed) > 0)
			if err != nil {
				log.Warn("pull request not opened", "err", err)
				summary += "\n\n(The pull request could not be opened: " + firstLine(err.Error()) + ")"
			} else {
				rec.PRNumber, rec.PRURL = pr.Number, pr.URL
				log.Info("pull request ready", "url", pr.URL, "action", action)
			}
		}
		w.saveBranch(rec)
	}

	fields := map[string]any{
		"summary": summary, "head_sha": head, "branch": branch,
		"diffstat": diffStat(ctx, worktree, repo.DefaultBranch), "checks": checks,
		"pushed": repo.Push.Kind != "none", "pr_url": rec.PRURL,
	}
	if len(failed) > 0 {
		// The gate: a branch that fails its checks never reaches review.
		reason := "checks failed: " + checkNames(failed)
		switch {
		case cantFix != "":
			reason += " (the implementer couldn't fix them: " + firstLine(cantFix) + ")"
		case rounds > 0:
			reason += " (after " + plural(rounds, "fix round") + ")"
		}
		log.Warn("implementation blocked by its checks", "head", head, "failed", checkNames(failed), "fix_rounds", rounds)
		fields["reason"] = reason
		return w.report(ctx, run, false, fields)
	}
	log.Info("implementation finished", "head", head, "commits", commitCount(ctx, worktree, repo.DefaultBranch), "fix_rounds", rounds, "cost_usd", w.costs[run.ID])
	return w.report(ctx, run, true, fields)
}

// fixChecksRound hands the failed checks back to the implementer, resuming
// its session, and returns its structured result (done, blocked or
// amendment). An agent outcome other than OK (quota, transient, auth) is
// returned as the stage error, as for the first turn.
func (w *Worker) fixChecksRound(ctx context.Context, run *Run, item *Item, repo *Repo, worktree, branch string, account *Account, failed []Check, round int) (*implResult, error) {
	prompt, err := renderPrompt("fix_checks.md", map[string]any{
		"Title": item.Title, "Branch": branch, "Base": repo.DefaultBranch,
		"Failed": failed, "Round": round, "Rounds": repo.FixRounds(),
		"Commands": w.stageCommands(run.WorkspaceID, "implement", implTools),
	})
	if err != nil {
		return nil, err
	}
	res := w.runAgent(ctx, w.agentRun(worktree, prompt, implSchema, implTools, w.cfg.ImplTimeout, account, run, "implement"))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, "implement"); err != nil {
		return nil, err
	}
	var ir implResult
	if err := json.Unmarshal(res.Structured, &ir); err != nil {
		return nil, fmt.Errorf("fix round %d: no structured result: %s", round, firstLine(res.Text))
	}
	return &ir, nil
}

// reviewSchema makes the verdict machine-checkable (review H6: a substring
// test let "2 issues found." pass). Findings carry a severity: blocking
// findings fail the review, nits are reported but never block (multi-agent
// stages merge them across reviewers). `issues` is the older shape, still
// accepted as all-blocking.
const reviewSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["pass","fail"]},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["blocking","nit"]},"text":{"type":"string"},"file":{"type":"string"}},"required":["severity","text"]}},"issues":{"type":"array","items":{"type":"string"}}},"required":["verdict","summary"]}`

type reviewResult struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Issues   []string  `json:"issues"`
}

// Finding is one review observation (mirrors runs.Finding on the wire).
type Finding struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
}

// normalise makes the verdict follow the findings instead of trusting the
// free-form verdict: a review fails exactly when it has a blocking finding.
// Older `issues` count as blocking; a "fail" with no findings at all keeps
// the reviewer's judgement as one blocking finding (its summary).
func (rr *reviewResult) normalise() {
	for _, is := range rr.Issues {
		if strings.TrimSpace(is) != "" {
			rr.Findings = append(rr.Findings, Finding{Severity: "blocking", Text: strings.TrimSpace(is)})
		}
	}
	rr.Issues = nil
	blocking := false
	kept := rr.Findings[:0]
	for _, f := range rr.Findings {
		f.Text = strings.TrimSpace(f.Text)
		if f.Text == "" {
			continue
		}
		if f.Severity != "nit" {
			f.Severity = "blocking"
			blocking = true
		}
		kept = append(kept, f)
	}
	rr.Findings = kept
	if !blocking && rr.Verdict == "fail" && len(rr.Findings) == 0 {
		rr.Findings = append(rr.Findings, Finding{Severity: "blocking", Text: strings.TrimSpace(rr.Summary)})
		blocking = true
	}
	if blocking {
		rr.Verdict = "fail"
	} else {
		rr.Verdict = "pass"
	}
}

// notPushedMarker is in a review's failure reason when the branch isn't on
// the remote. The dashboard matches it (unpushed() in web/src/main.ts) to
// tell, for runs from workers that don't report `pushed`, that a head
// exists only on the worker; TestNotPushedMarkerMatchesDashboard ties the
// two together.
const notPushedMarker = "not pushed"

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
	// A second opinion reviews exactly the head it was queued for (the one
	// waiting for approval), never whatever the branch tip is now; it runs
	// with the review profile and reports its verdict without failing.
	second := run.Stage == "second_opinion"
	if second && run.HeadSHA == "" {
		return fmt.Errorf("second opinion run has no head to review")
	}
	phase := run.Stage
	if second {
		phase = "review"
	}
	account := w.pickAccount(run, log)
	if account == nil {
		return w.noAccount(run, "review")
	}
	// Fresh checkout of the pushed branch: fetch, then a detached worktree
	// at origin/<branch>. If the fetch finds nothing the review fails — an
	// empty diff is not a pass.
	// Per run: members of a multi-agent stage review the same branch, and
	// must never share a worktree.
	wt := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(run.Branch, "/", "-")+"-"+run.Stage+"-"+shortID(run.ID))
	removeWorktree(ctx, repo.Path, wt)
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", run.Branch); err != nil {
		return fmt.Errorf("review: branch %s "+notPushedMarker+": %w", run.Branch, err)
	}
	ref := "origin/" + run.Branch
	if second {
		ref = run.HeadSHA
	}
	if err := createDetached(ctx, repo.Path, wt, ref); err != nil {
		return fmt.Errorf("review worktree at %s: %w", ref, err)
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
	var decisions []string
	if item, ierr := w.client.GetItem(ctx, run.ItemID); ierr == nil && item.PlanID != "" {
		// Martin's decisions (answers to the planner's questions, approval
		// notes, change requests) are part of the spec: a reviewer that
		// cannot see them blocks on behaviour he explicitly accepted (found
		// in the live multi-agent run).
		for _, h := range []string{"## Answers", "## Approval notes", "## Change requests"} {
			if sec := sectionOnly(item.Content, h); sec != "" {
				decisions = append(decisions, sec)
			}
		}
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
	// The member's focus brief, read at the approved base (never from the
	// branch under review). A missing brief fails this member loudly.
	var focus *Skill
	if run.Focus != "" {
		f, ferr := repoFocus(ctx, repo.Path, base, run.Focus)
		if ferr != nil {
			return w.report(ctx, run, false, map[string]any{"reason": run.Stage + ": " + ferr.Error()})
		}
		focus = &f
	}
	// What the worker's checks said about this exact commit: the reviewer
	// can't run tools, and without this it reports that nothing was verified.
	checks := w.client.HeadChecks(ctx, run.ItemID, headSHA(ctx, wt))
	prompt, err := renderPrompt("review.md", map[string]any{
		"Base": repo.DefaultBranch, "Stage": run.Stage, "Steps": steps,
		"Instructions": instructions, "Skills": skillBodies, "Focus": focus,
		"Decisions": decisions, "SecondOpinion": second,
		"Checks": checks, "ChecksFailed": len(failedChecks(checks)) > 0, "ChecksConfigured": len(repo.Checks) > 0,
	})
	if err != nil {
		return err
	}
	log.Info("reviewing", "account", account.Name, "stage", run.Stage, "branch", run.Branch)
	res := w.runAgent(ctx, w.agentRun(wt, prompt, reviewSchema, reviewTools, w.cfg.PlanTimeout, account, run, phase))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, phase); err != nil {
		return err
	}
	var rr reviewResult
	if err := json.Unmarshal(res.Structured, &rr); err != nil || rr.Verdict == "" {
		return fmt.Errorf("review returned no verdict: %s", firstLine(res.Text))
	}
	rr.normalise()
	if second {
		// The server writes the section (runner and head from the run
		// record), and the verdict is advisory: the run itself succeeded.
		log.Info("second opinion finished", "verdict", rr.Verdict, "findings", len(rr.Findings), "head", run.HeadSHA, "cost_usd", res.CostUSD)
		return w.report(ctx, run, true, map[string]any{
			"summary": rr.Summary, "findings": rr.Findings, "verdict": rr.Verdict, "head_sha": run.HeadSHA,
		})
	}
	passed := rr.Verdict == "pass"
	heading := run.Stage + " — " + account.Name
	if run.Focus != "" {
		heading = run.Stage + " — " + run.Focus + " · " + account.Name
	}
	_ = w.client.AppendSection(ctx, run.ItemID, heading, reviewSection(rr))
	log.Info("review finished", "verdict", rr.Verdict, "findings", len(rr.Findings), "focus", run.Focus, "cost_usd", res.CostUSD)
	if !passed {
		return w.report(ctx, run, false, map[string]any{
			"reason": run.Stage + " failed: " + rr.Summary, "summary": rr.Summary, "findings": rr.Findings,
		})
	}
	return w.report(ctx, run, true, map[string]any{
		"summary": rr.Summary, "findings": rr.Findings,
		"checks": []Check{{Name: run.Stage + ":" + account.Name, Passed: true, Output: rr.Summary}},
	})
}

// reviewSection renders one reviewer's verdict and findings for the item.
func reviewSection(rr reviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verdict:** %s\n\n%s", rr.Verdict, rr.Summary)
	for _, f := range rr.Findings {
		loc := ""
		if f.File != "" {
			loc = "`" + f.File + "` "
		}
		fmt.Fprintf(&b, "\n- **%s** %s%s", f.Severity, loc, f.Text)
	}
	return b.String()
}

// shortID is the tail of a UUIDv7 (the random part), for path names.
func shortID(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
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

// sectionOnly is the newest "heading" section alone: up to the next "## "
// heading (lastSection runs to the end of the content).
func sectionOnly(content, heading string) string {
	idx := strings.LastIndex(content, heading)
	if idx < 0 {
		return ""
	}
	rest := content[idx:]
	if j := strings.Index(rest[len(heading):], "\n## "); j >= 0 {
		rest = rest[:len(heading)+j]
	}
	return strings.TrimSpace(rest)
}

func numberedSteps(steps []Step) string {
	var b strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&b, "%d. **%s** — %s\n", i+1, s.Title, s.Description)
	}
	return b.String()
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

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
