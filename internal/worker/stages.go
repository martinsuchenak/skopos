package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/itemtext"
)

// stagePlan: run the planner in the clone (read-only), write the plan +
// revision (bound to the base the planner read), link it. The queue's
// chaining moves the item to awaiting_approval on the success report.
func (w *Worker) stagePlan(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.repo(run.WorkspaceID)
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
	// recorded base binds the approver's approval to exactly what was read.
	done := runLogFrom(ctx).stepf("fetching origin/%s", repo.DefaultBranch)
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", repo.DefaultBranch); err != nil {
		return retryable(err)
	}
	done()
	if _, err := git(ctx, repo.Path, "merge", "--ff-only", "--quiet", "origin/"+repo.DefaultBranch); err != nil {
		return fmt.Errorf("clone not fast-forwardable to origin/%s: %w", repo.DefaultBranch, err)
	}
	baseSHA, _ := git(ctx, repo.Path, "rev-parse", "origin/"+repo.DefaultBranch)

	// The item's branch, and whether someone already started it. Several
	// candidates and no exact match: the approver picks (the worker never guesses
	// whose branch to continue). A branch that exists is a handover: the
	// plan is made against its head, and the approval names that head.
	choice, err := resolveBranch(ctx, repo, item)
	if err != nil {
		return w.report(ctx, run, false, map[string]any{"reason": "branch: " + err.Error()})
	}
	if choice.Question != "" {
		w.askQuestions(ctx, run, askerWorker, "", []itemtext.Question{{Text: choice.Question}}, true, "", "")
		return w.report(ctx, run, false, map[string]any{"reason": "needs clarification: " + choice.Question})
	}
	branch := choice.Name
	if choice.Exists {
		if baseSHA, err = fetchBranch(ctx, repo.Path, branch); err != nil {
			return err
		}
	}

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
	var ho *handover
	if choice.Exists {
		if dir == repo.Path {
			wt := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), "plan-"+shortID(run.ID))
			removeWorktree(ctx, repo.Path, wt)
			if err := createDetached(ctx, repo.Path, wt, baseSHA); err != nil {
				return fmt.Errorf("plan worktree at %s's head %s: %w", branch, shortSHA(baseSHA), err)
			}
			defer removeWorktree(ctx, repo.Path, wt)
			dir = wt
		}
		if ho, err = handoverContext(ctx, repo, branch, baseSHA); err != nil {
			return fmt.Errorf("reading the work on %s: %w", branch, err)
		}
	}
	// Knowledge comes from the default branch's history, never from a
	// branch being taken over.
	kbase := knowledgeBase(ctx, repo, baseSHA)
	// Optional focus brief (plan members may have one), read at the base.
	var focus *Skill
	if run.Focus != "" {
		f, ferr := repoFocus(ctx, repo.Path, kbase, run.Focus)
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
	skills := repoSkills(ctx, repo.Path, kbase)
	// The author's text plus the sections current for this round: their answers,
	// the plan(s) his newest notes are about, and those notes (brief.go).
	brief, secs := splitItem(item.Content)
	previous := ""
	if seed == nil {
		previous = previousPlans(secs)
	}
	// A reviewed branch that nobody planned (a review-only item the approver
	// asked changes for): his change requests and the review they answer.
	changes, review := "", ""
	if item.PlanID == "" {
		if changes = newestBody(secs, "Change requests"); changes != "" {
			review = lastReview(secs)
		}
	}
	prompt, err := renderPrompt("plan.md", map[string]any{
		"Workspace": run.WorkspaceID, "Base": repo.DefaultBranch,
		"Title": item.Title, "Content": brief, "QA": w.decisions(ctx, run, secs),
		"Previous": previous, "Notes": newestBody(secs, "Review notes"),
		"Handover": ho, "Changes": changes, "LastReview": review,
		"Instructions": repoInstructions(ctx, repo.Path, kbase),
		"SkillIndex":   skillIndex(skills), "Requested": tagHints(skills, item.Tags),
		"ImplCommands": w.stageCommands(run.WorkspaceID, "implement", implTools), "Checks": repo.Checks,
		"Focus": focus, "Seed": seedSteps,
		"Skopos": w.skoposPrompt(ctx, run, account, repo.DefaultBranch, branch, "plan", "", nil, ""),
	})
	if err != nil {
		return err
	}
	log.Info("planning", "account", account.Name, "base", baseSHA, "branch", branch, "handover", choice.Exists, "group", run.GroupID != "", "seeded", seed != nil)
	res := w.runAgent(ctx, w.agentRun(dir, prompt, planSchema, w.planToolsFor(run.WorkspaceID), w.planTimeout(run.WorkspaceID), account, run, "plan"))
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
		questions := itemtext.FormatQuestions(pr.Questions)
		w.askQuestions(ctx, run, askerPlanner, label, pr.Questions, true, subjectBase, baseSHA)
		return w.report(ctx, run, false, map[string]any{
			"reason": "needs clarification: " + questions, "summary": res.Text[:min(len(res.Text), 500)],
		})
	}

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
	if len(pr.Risks) > 0 {
		enrich += "\n**Risks:**\n" + bullets(pr.Risks) + "\n"
	}
	if choice.Exists {
		enrich += fmt.Sprintf("\n**Handover:** continues the existing branch `%s` from `%s`.\n", branch, shortSHA(baseSHA))
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Enrichment — skopos worker", enrich)
	// The plan's open questions follow it as their own section: steps that
	// depend on an answer name the question by its number.
	w.askQuestions(ctx, run, askerPlanner, label, pr.Questions, false, subjectPlanRevision, rev.ID+"+"+baseSHA)

	log.Info("plan ready", "plan", planID, "revision", rev.ID, "steps", len(pr.Steps), "cost_usd", res.CostUSD)
	return w.report(ctx, run, true, map[string]any{
		"summary": "plan ready: " + pr.Summary, "branch": branch, "base_sha": baseSHA,
	})
}

// decisions is the questions and the approver's answers a prompt carries: from the
// server's record (which marks those answers as used by this run), or,
// from a server without it, read from the item's sections.
func (w *Worker) decisions(ctx context.Context, run *Run, secs []itemSection) string {
	text, ok, err := w.client.ReadQuestions(ctx, run.ID, w.cfg.ID)
	if ok {
		return text
	}
	if err != nil {
		w.log.Warn("reading the questions and answers from the record failed; reading the item's sections", "run", run.ID, "err", err)
	}
	return questionsAndAnswers(secs)
}

// Who asks, and what a set of questions is about (internal/qa).
const (
	askerPlanner        = "planner"
	askerImplementer    = "implementer"
	askerWorker         = "worker"
	subjectPlanRevision = "plan_revision"
	subjectBase         = "base"
)

// askQuestions posts a run's questions to the server, which records them
// (who asked, about what, whether the item waits) and renders them into the
// item. A server without the endpoint gets the section appended as before.
// Nothing is sent when there are none.
func (w *Worker) askQuestions(ctx context.Context, run *Run, asker, label string, qs []itemtext.Question, blocking bool, subjectKind, subject string) {
	text := itemtext.FormatQuestions(qs)
	if strings.TrimSpace(text) == "" {
		return
	}
	post := QuestionsPost{WorkerID: w.cfg.ID, Asker: asker, Blocking: blocking, SubjectKind: subjectKind, Subject: subject}
	for _, q := range qs {
		post.Questions = append(post.Questions, QuestionPosted{Question: q.Text, Options: q.Options})
	}
	// The record is the server's: a failed post is tried once more, never
	// appended by hand (the server may have recorded it already, and an
	// appended section would be recorded a second time). Only a server
	// without the endpoint gets the section appended.
	ok, err := w.client.AskQuestions(ctx, run.ID, post)
	if err != nil {
		ok, err = w.client.AskQuestions(ctx, run.ID, post)
	}
	if err != nil {
		w.log.Warn("posting the questions failed", "run", run.ID, "err", err)
		return
	}
	if ok {
		return
	}
	heading := "Questions — planner"
	switch {
	case asker == askerImplementer:
		heading = "Questions — implementer"
	case run.GroupID != "" && label != "":
		heading = "Questions — planner " + label
	}
	_ = w.client.AppendSection(ctx, run.ItemID, heading, text)
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
// was claimed; the live steps stay untouched until the approver approves one.
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
	w.askQuestions(ctx, run, askerPlanner, label, pr.Questions, false, subjectPlanRevision, rev.ID+"+"+baseSHA)
	log.Info("candidate ready", "plan", planID, "revision", rev.ID, "label", label, "steps", len(pr.Steps), "cost_usd", res.CostUSD)
	return w.report(ctx, run, true, map[string]any{
		"summary": "candidate ready: " + pr.Summary, "branch": branch, "base_sha": baseSHA,
	})
}

// stageImplement: worktree from the approved base, implement per the locked
// plan, run checks, push the branch, report the head.
func (w *Worker) stageImplement(ctx context.Context, run *Run, log *slog.Logger) error {
	rl := runLogFrom(ctx)
	repo := w.repo(run.WorkspaceID)
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
	// Setup and checks run the branch's code: behind the stage's wrapper,
	// when it has one.
	ctx = withShellWrap(ctx, w.stageShellWrap(run, account, "implement"))

	branch := run.Branch
	if branch == "" {
		// The plan stage resolved the same name (an existing branch is
		// continued: implementStart prefers it over the base).
		if branch, err = branchFor(item, repo); err != nil {
			return w.report(ctx, run, false, map[string]any{"reason": "branch: " + err.Error()})
		}
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
			if isReviewStep(pi.Title, pi.Phase) {
				continue
			}
			steps = append(steps, Step{Title: pi.Title, Description: pi.Description, ID: pi.ID, Done: pi.Status == "done"})
		}
	}
	worktree := filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), strings.ReplaceAll(branch, "/", "-"))
	branchMode := repo.Checkout == checkoutBranch
	if branchMode {
		worktree = repo.Path
	}
	// What this item leaves on the worker, for the cleanup sweep. A later
	// round keeps the pull request the first one opened, but not Pushed:
	// until this round pushes, its commits exist only here. A branch
	// checkout leaves no worktree.
	rec, _ := w.loadBranch(item.ID)
	rec.ItemID, rec.WorkspaceID, rec.Branch, rec.Pushed = item.ID, run.WorkspaceID, branch, false
	if !branchMode {
		rec.Worktree = worktree
	}
	fresh := !dirExists(worktree)
	// A git command killed with an earlier run's agent leaves index.lock
	// behind, and git refuses everything after it: clear it before the
	// checkout touches the tree.
	if !fresh && clearStaleIndexLock(ctx, worktree) {
		rl.notef("removed the index.lock a killed git command left in %s", worktree)
	}
	if branchMode {
		// The clone (held by execute) is switched to the item's branch;
		// when the run ends it goes back to the default branch, and anything
		// uncommitted (a pause mid-turn) is stashed for the item's next run.
		// Setup runs unless the clone is still set up for this branch.
		if rec.Worktree != "" {
			// From before the repo switched to a branch checkout: the old
			// worktree has the branch checked out, so it goes, unless it
			// holds work nothing else has.
			if dirExists(rec.Worktree) {
				if dirty, err := git(ctx, rec.Worktree, "status", "--porcelain"); err != nil || dirty != "" {
					return fmt.Errorf("the item's worktree %s from before the branch checkout has uncommitted changes; commit or remove them to continue in the clone", rec.Worktree)
				}
				removeWorktree(ctx, repo.Path, rec.Worktree)
			}
			rec.Worktree = ""
		}
		done := rl.stepf("fetching and checking out %s in the clone", branch)
		// Before the checkout: a run killed from here on left the clone on
		// this branch (readyClone then stashes what it left).
		setRunMarker(ctx, repo.Path, branch)
		if err := checkoutImplementBranch(ctx, repo.Path, branch, baseRef); err != nil {
			w.park(repo, branch, log) // a failed merge happens on the branch
			return retryable(fmt.Errorf("checkout: %w", err))
		}
		done()
		defer w.park(repo, branch, log)
		if err := applyStash(ctx, repo.Path, stashLabel(branch)); err != nil {
			log.Warn("continuing from the branch's last commit", "err", err)
			rl.notef("continuing from the branch's last commit: %v", err)
		}
		cs := readCloneSetup(ctx, repo.Path)
		head := headSHA(ctx, repo.Path)
		fresh = cs.Branch != branch || cs.Head != head
		if !fresh && !rec.SetupPending && len(repo.Setup) > 0 {
			rl.notef("setup skipped: the clone is still set up for %s at %s", branch, shortSHA(head))
		}
	} else if fresh {
		done := rl.stepf("fetching and creating the worktree for %s", branch)
		if err := prepareImplementWorktree(ctx, repo.Path, worktree, branch, baseRef); err != nil {
			return retryable(fmt.Errorf("worktree: %w", err))
		}
		done()
	} else {
		// The item's worktree from an earlier attempt: origin may have moved
		// on since (the push that failed it was rejected).
		done := rl.stepf("fetching and updating the worktree for %s", branch)
		if err := pullWorktree(ctx, repo.Path, worktree, branch); err != nil {
			return retryable(fmt.Errorf("worktree: %w", err))
		}
		done()
	}
	// Setup runs in a fresh worktree, and again in one whose setup never
	// finished (the worker died or lost the run mid-setup). Its results ride
	// on this run's report (kept in the record across a pause).
	if fresh || rec.SetupPending {
		rec.SetupPending = len(repo.Setup) > 0
		w.saveBranch(rec)
		if branchMode {
			// Set up for nothing until this setup finishes.
			writeCloneSetup(ctx, repo.Path, cloneSetup{})
		}
		setup, err := runSetup(ctx, worktree, repo.Setup, repo.Path, repo.SetupEnv, w.cfg.TokenEnvs(), w.envFileKeys, repo.setupLimits())
		if err != nil {
			if !branchMode {
				removeWorktree(ctx, repo.Path, worktree)
			}
			// The environment's problem: skopos retries the run.
			return w.report(ctx, run, false, map[string]any{"reason": err.Error(), "setup": setup, "retryable": true})
		}
		rec.SetupPending, rec.SetupRun, rec.Setup = false, run.ID, setup
		if branchMode {
			writeCloneSetup(ctx, repo.Path, cloneSetup{Branch: branch, Head: headSHA(ctx, repo.Path)})
		}
	}
	w.saveBranch(rec)
	var setup []Check
	if rec.SetupRun == run.ID {
		setup = rec.Setup
	}
	// The implementer's commits go to origin as it makes them (and the fix
	// rounds'), so a round cut off here leaves its work for the next one.
	stopSteps := w.pushSteps(ctx, repo, worktree, branch, log)
	defer stopSteps()

	// A run resumed after a pause during a fix round (quota) already has
	// its implementation: it goes straight back to the checks, with the
	// fix rounds it has used.
	var ir implResult
	if rec.ImplementedRun == run.ID {
		ir = implResult{Status: "done", Summary: rec.ImplementedSummary}
		log.Info("resuming the check fixes", "fix_rounds_used", rec.FixRounds)
	} else {
		// An earlier run of this implementation that was cut off (its
		// timeout, a crash, a retry) left its finished steps committed and,
		// often, the step it was on half done: the implementer continues
		// from there instead of starting over.
		var resumed []string
		for i, c := range committedSteps(ctx, worktree, baseRef, steps) {
			if c && !steps[i].Done {
				steps[i].Done = true
				if steps[i].ID != "" {
					_ = w.client.SetPlanItemStatus(ctx, item.PlanID, steps[i].ID, "done")
				}
			}
			if steps[i].Done {
				resumed = append(resumed, strconv.Itoa(i+1))
			}
		}
		leftover := leftoverChanges(ctx, worktree)
		if len(resumed) > 0 {
			rl.notef("continuing the implementation: steps %s are already committed", strings.Join(resumed, ", "))
		}
		if len(leftover) > 0 {
			rl.notef("an earlier run left %d uncommitted change(s); the implementer decides what to keep", len(leftover))
		}
		instructions, skillBodies := w.runKnowledge(ctx, repo, run, item.PlanID, baseRef)
		// The author's text and the current decisions, never the earlier plans:
		// the approved steps come from the plan revision (brief.go).
		brief, secs := splitItem(item.Content)
		notes := newestBody(secs, "Change requests")
		review := ""
		if notes != "" {
			review = lastReview(secs)
		}
		// The review findings to fix, from the record: each is answered by
		// id in the result (fixed, or not fixed and why).
		var toFix []findingRef
		if view := w.runFindings(ctx, run); view != nil {
			toFix = findingRefs(view.Fix)
		}
		prompt, err := renderPrompt("implement.md", map[string]any{
			"Branch": branch, "Base": repo.DefaultBranch, "Title": item.Title,
			"Content": brief, "QA": w.decisions(ctx, run, secs), "Steps": steps, "Notes": notes, "LastReview": review,
			"Findings":      toFix,
			"ApprovalNotes": newestBody(secs, "Approval notes"),
			"Resumed":       len(resumed) > 0, "Leftover": leftover,
			"Instructions": instructions, "Skills": skillBodies,
			"Commands": w.stageCommands(run.WorkspaceID, "implement", implTools), "Checks": repo.Checks,
			"Skopos": w.skoposPrompt(ctx, run, account, repo.DefaultBranch, branch, "implement", item.PlanID, steps, ""),
		})
		if err != nil {
			return err
		}
		log.Info("implementing", "account", account.Name, "branch", branch, "worktree", worktree)
		res := w.runAgent(ctx, w.agentRun(worktree, prompt, implSchema, w.implToolsFor(run.WorkspaceID), w.implTimeout(run.WorkspaceID), account, run, "implement"))
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
			w.appendImplQuestions(ctx, run, ir)
			return w.report(ctx, run, false, map[string]any{"reason": reason, "setup": setup})
		}
		if commitCount(ctx, worktree, baseRef) == 0 {
			return fmt.Errorf("agent reported done but made no commits")
		}
		rec.ImplementedRun, rec.ImplementedSummary, rec.FixRounds = run.ID, ir.Summary, 0
		said := resolveAnswers(toFix, ir.Findings, findings.KindFixed, findings.KindNotFixed)
		rec.FindingAnswers = wireAnswers(said)
		if len(toFix) > 0 {
			rl.notef("the implementer answered %d of %d review findings", len(said), len(toFix))
		}
		w.saveBranch(rec)
	}
	// The checks are the CI: a failure goes back to the implementer (the
	// same session) for a fix round, then the checks run again. A round
	// counts once it finished, so a quota pause repeats it on resume.
	cantFix := ""
	checks, _, stopped, stopErr := fixChecks(repo.FixRounds()-rec.FixRounds,
		func() []Check {
			w.setPhase(ctx, run, phaseChecks)
			return w.repoChecks(ctx, repo, worktree)
		},
		func(failed []Check, _ int) (bool, error) {
			if hung := stoppedChecks(failed); len(hung) > 0 {
				// A hang is the worker's problem: no fix round for it.
				return true, retryable(fmt.Errorf("checks stopped before they finished: %s", checkNames(hung)))
			}
			w.setPhase(ctx, run, phaseFixing)
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
				w.appendImplQuestions(ctx, run, *fix)
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
	if hung := stoppedChecks(checks); len(hung) > 0 && cantFix == "" {
		// No fix rounds configured, or a hang in the last run of the checks.
		return retryable(fmt.Errorf("checks stopped before they finished: %s", checkNames(hung)))
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
	stopSteps() // the final push is the one that counts
	if repo.Push.Kind != "none" {
		token := os.Getenv(repo.Push.TokenEnv)
		if token == "" {
			return fmt.Errorf("push token env %s is empty", repo.Push.TokenEnv)
		}
		if err := pushBranch(ctx, worktree, repo.Push.RemoteURL, token, repo.Push.Kind, branch, repo.BranchPrefix, repo.Protected); err != nil {
			// Rejected (origin moved on) or unreachable: the retry pulls first.
			return retryable(fmt.Errorf("push: %w", err))
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
				owned := action == "opened" || ownsPullRequest(pr, item.ID)
				rec.PROwned = &owned
				log.Info("pull request ready", "url", pr.URL, "action", action)
			}
		}
		w.saveBranch(rec)
	}

	fields := map[string]any{
		"summary": summary, "head_sha": head, "branch": branch,
		"diffstat": diffStat(ctx, worktree, baseRef), "checks": checks,
		"pushed": repo.Push.Kind != "none", "pr_url": rec.PRURL, "setup": setup, "fix_rounds": rounds,
	}
	if rec.ImplementedRun == run.ID && len(rec.FindingAnswers) > 0 {
		fields["finding_answers"] = rec.FindingAnswers
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
	// Review and qa query this branch's code: index it before reporting,
	// after the push so the commits are safe on origin first (only where the
	// workspace's code_index switch is on; bounded, else at the next idle
	// moment). A branch blocked by its checks is never reviewed: no index.
	w.indexBranchNow(ctx, repo, run.WorkspaceID, branch)
	log.Info("implementation finished", "head", head, "commits", commitCount(ctx, worktree, baseRef), "fix_rounds", rounds, "cost_usd", w.costs[run.ID])
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
	res := w.runAgent(ctx, w.agentRun(worktree, prompt, implSchema, w.implToolsFor(run.WorkspaceID), w.implTimeout(run.WorkspaceID), account, run, "implement"))
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
const reviewSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["pass","fail"]},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["blocking","nit"]},"text":{"type":"string"},"file":{"type":"string"}},"required":["severity","text"]}},"issues":{"type":"array","items":{"type":"string"}},"previous":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"status":{"type":"string"},"note":{"type":"string"}}}}},"required":["verdict","summary"]}`

type reviewResult struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Issues   []string  `json:"issues"`
	// Previous: what the review says about the earlier findings its prompt
	// listed, by handle.
	Previous findingAnswers `json:"previous"`
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
func (rr *reviewResult) normalise() { rr.normaliseWith(0) }

// normaliseWith is normalise for a review that also checked earlier
// findings: stillOpen of them are blocking and not fixed, which fails the
// review like a finding of its own.
func (rr *reviewResult) normaliseWith(stillOpen int) {
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
	if stillOpen > 0 {
		blocking = true
	}
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
	return w.review(ctx, run, log, nil)
}

// review is the review/QA/second-opinion stage; br is set for a review-only
// item's branch_review (the head it checked and the checks' results).
func (w *Worker) review(ctx context.Context, run *Run, log *slog.Logger, br *branchReview) error {
	repo := w.repo(run.WorkspaceID)
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
	if second || br != nil {
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
	done := runLogFrom(ctx).stepf("fetching origin/%s", run.Branch)
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", run.Branch); err != nil {
		return fmt.Errorf("review: branch %s "+notPushedMarker+": %w", run.Branch, err)
	}
	done()
	ref := "origin/" + run.Branch
	switch {
	case second:
		ref = run.HeadSHA
	case br != nil:
		ref = br.Head
	}
	if err := createDetached(ctx, repo.Path, wt, ref); err != nil {
		return fmt.Errorf("review worktree at %s: %w", ref, err)
	}
	defer removeWorktree(ctx, repo.Path, wt)
	base := run.BaseSHA
	if base == "" {
		base = "origin/" + repo.DefaultBranch
	}
	if br != nil {
		base = br.Head // its fork point, below
	}
	// A handover's base is the branch head the plan was made against: the
	// commits up to it were someone else's. The reviewer reviews them all
	// (they merge together) but is told which are which.
	handoverBase := ""
	if run.BaseSHA != "" && !onDefaultBranch(ctx, repo, run.BaseSHA) {
		handoverBase = run.BaseSHA
	}
	// Instructions and knowledge from the default branch's history, never
	// from the branch under review.
	base = knowledgeBase(ctx, repo, base)
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
	brief := ""
	if br != nil {
		// Nobody planned this branch: what it is for is the item's text and
		// The approver's answers.
		b, secs := splitItem(br.Item.Content)
		brief = b
		if qa := w.decisions(ctx, run, secs); qa != "" {
			brief += "\n\n" + qa
		}
		instructions = repoInstructions(ctx, repo.Path, base)
	} else if item, ierr := w.client.GetItem(ctx, run.ItemID); ierr == nil && item.PlanID != "" {
		// The approver's decisions (answers to the planner's questions, approval
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
				if isReviewStep(pi.Title, pi.Phase) {
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
	var checks []Check
	if br != nil {
		checks = br.Checks
	} else {
		checks = w.client.HeadChecks(ctx, run.ItemID, headSHA(ctx, wt))
	}
	// Earlier findings, from the record: the ones this reviewer checks
	// again (answered by id in the result), the ones another reviewer
	// checks, and the ones the approver accepted.
	var recheck, others, waived []findingRef
	if view := w.runFindings(ctx, run); view != nil {
		recheck, others, waived = findingRefs(view.Check), findingRefs(view.Others), findingRefs(view.Waived)
	}
	reviewed := headSHA(ctx, wt)
	prompt, err := renderPrompt("review.md", map[string]any{
		"Recheck": recheck, "OthersCheck": others, "Waived": waived,
		"Base": repo.DefaultBranch, "Stage": run.Stage, "Steps": steps, "Brief": brief, "BranchReview": br != nil,
		"Instructions": instructions, "Skills": skillBodies, "Focus": focus,
		"Decisions": decisions, "SecondOpinion": second, "HandoverBase": handoverBase,
		"Checks": checks, "ChecksFailed": len(failedChecks(checks)) > 0, "ChecksConfigured": len(repo.Checks) > 0,
		"Skopos": w.skoposPrompt(ctx, run, account, repo.DefaultBranch, run.Branch, phase, "", nil, headSHA(ctx, wt)),
	})
	if err != nil {
		return err
	}
	log.Info("reviewing", "account", account.Name, "stage", run.Stage, "branch", run.Branch)
	res := w.runAgent(ctx, w.agentRun(wt, prompt, reviewSchema, reviewTools, w.planTimeout(run.WorkspaceID), account, run, phase))
	w.addCost(run, res)
	if err := w.handleAgentOutcome(ctx, run, res, account, phase); err != nil {
		return err
	}
	var rr reviewResult
	if err := json.Unmarshal(res.Structured, &rr); err != nil || rr.Verdict == "" {
		return fmt.Errorf("review returned no verdict: %s", firstLine(res.Text))
	}
	// What it says about the earlier findings; one that blocks and is not
	// fixed fails the review like a finding of its own.
	said := resolveAnswers(recheck, rr.Previous, findings.KindAddressed, findings.KindStillOpen)
	rr.normaliseWith(stillOpenBlocking(said))
	answers := wireAnswers(said)
	if len(recheck) > 0 {
		runLogFrom(ctx).notef("the reviewer answered %d of %d earlier findings", len(said), len(recheck))
	}
	if second {
		// The server writes the section (runner and head from the run
		// record), and the verdict is advisory: the run itself succeeded.
		log.Info("second opinion finished", "verdict", rr.Verdict, "findings", len(rr.Findings), "head", run.HeadSHA, "cost_usd", res.CostUSD)
		return w.report(ctx, run, true, map[string]any{
			"summary": rr.Summary, "findings": rr.Findings, "verdict": rr.Verdict, "head_sha": run.HeadSHA,
			"finding_answers": answers,
		})
	}
	if br != nil {
		// The verdict is what the approver asked for: written into the item, and
		// the run succeeds whatever it says (he decides at in_review).
		heading := "review — " + account.Name
		if run.Focus != "" {
			heading = "review — " + run.Focus + " · " + account.Name
		}
		_ = w.client.AppendSection(ctx, run.ItemID, heading, reviewSection(rr, said))
		log.Info("branch review finished", "verdict", rr.Verdict, "findings", len(rr.Findings), "branch", run.Branch, "head", br.Head, "cost_usd", res.CostUSD)
		return w.report(ctx, run, true, map[string]any{
			"summary": rr.Summary, "findings": rr.Findings, "verdict": rr.Verdict, "finding_answers": answers,
			"head_sha": br.Head, "branch": run.Branch, "checks": br.Checks, "setup": br.Setup,
			"diffstat": diffStat(ctx, wt, base),
		})
	}
	passed := rr.Verdict == "pass"
	heading := run.Stage + " — " + account.Name
	if run.Focus != "" {
		heading = run.Stage + " — " + run.Focus + " · " + account.Name
	}
	_ = w.client.AppendSection(ctx, run.ItemID, heading, reviewSection(rr, said))
	log.Info("review finished", "verdict", rr.Verdict, "findings", len(rr.Findings), "still_open", stillOpenBlocking(said), "focus", run.Focus, "cost_usd", res.CostUSD)
	if !passed {
		return w.report(ctx, run, false, map[string]any{
			"reason": run.Stage + " failed: " + rr.Summary, "summary": rr.Summary, "findings": rr.Findings,
			"finding_answers": answers, "head_sha": reviewed,
		})
	}
	return w.report(ctx, run, true, map[string]any{
		"summary": rr.Summary, "findings": rr.Findings, "finding_answers": answers, "head_sha": reviewed,
		"checks": []Check{{Name: run.Stage + ":" + account.Name, Passed: true, Output: rr.Summary}},
	})
}

// reviewSection renders one reviewer's verdict for the item: what it says
// about the earlier findings it checked again, then what it found new.
func reviewSection(rr reviewResult, said []answered) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verdict:** %s\n\n%s", rr.Verdict, rr.Summary)
	for _, a := range said {
		word := "addressed"
		if a.Status == findings.KindStillOpen {
			word = "still open"
		}
		note := ""
		if a.Note != "" {
			note = " — " + a.Note
		}
		fmt.Fprintf(&b, "\n- **%s** (raised by %s) %s%s", word, a.Ref.By, a.Ref.Text, note)
	}
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
// auto-reclaimed. The approver's approve resumes it. The amendment names the
// RUN's base, not the default branch tip.
func (w *Worker) proposeAmendment(ctx context.Context, run *Run, item *Item, ir implResult) error {
	if ir.Reason == "" {
		ir.Reason = ir.Summary
	}
	_ = w.client.AppendSection(ctx, run.ItemID, "Amendment proposed — worker", bullets([]string{ir.Reason}))
	w.appendImplQuestions(ctx, run, ir)
	base := run.BaseSHA
	if base == "" {
		base, _ = git(ctx, w.repo(run.WorkspaceID).Path, "rev-parse", "origin/"+w.repo(run.WorkspaceID).DefaultBranch)
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
	reason := "amendment proposed: " + firstLine(ir.Reason)
	rl := runLogFrom(ctx)
	rl.line("worker", "paused for your decision: "+reason, false)
	rl.flush()
	return w.client.Pause(ctx, run.ID, w.cfg.ID, reason, time.Time{})
}

// runKnowledge is what an implement/review/qa prompt carries: the repo
// instructions and the bodies of the APPROVED revision's skills, both read
// from the clone at base (capability profiles §3).
func (w *Worker) runKnowledge(ctx context.Context, repo *Repo, run *Run, planID, base string) (string, []Skill) {
	base = knowledgeBase(ctx, repo, base)
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
	// ID is the plan item's id (implement marks it done over MCP); empty
	// for steps an agent proposes.
	ID string `json:"-"`
	// Done: an earlier run of this implementation committed it (or marked
	// it done); the implementer continues after it.
	Done bool `json:"-"`
}

type planResult struct {
	Skills             []string            `json:"skills"`
	Title              string              `json:"title"`
	Summary            string              `json:"summary"`
	Size               string              `json:"size"`
	NeedsClarification bool                `json:"needs_clarification"`
	Steps              []Step              `json:"steps"`
	Risks              []string            `json:"risks"`
	Questions          []itemtext.Question `json:"questions"`
}

type implResult struct {
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	Summary      string `json:"summary"`
	AmendedSteps []Step `json:"amended_steps"`
	// Questions for the approver with an amendment or a block; Question is the
	// single-string shape the schema had before.
	Questions []itemtext.Question `json:"questions"`
	Question  string              `json:"question"`
	// Findings: what the implementer says about the review findings its
	// prompt listed, by handle.
	Findings findingAnswers `json:"findings"`
}

// asked is the implementer's questions in both shapes.
func (ir implResult) asked() []itemtext.Question {
	qs := ir.Questions
	if q := strings.TrimSpace(ir.Question); q != "" {
		qs = append(qs, itemtext.Question{Text: q})
	}
	return qs
}

// appendImplQuestions writes the implementer's questions (with an amendment
// or a block) as their own section: the dashboard lists them for answering
// at the gate or with Retry, Slack quotes them, and later prompts read them
// with the answers. They used to be dropped.
func (w *Worker) appendImplQuestions(ctx context.Context, run *Run, ir implResult) {
	kind, subject := subjectBase, run.BaseSHA
	if run.PlanRevID != "" {
		kind, subject = subjectPlanRevision, run.PlanRevID+"+"+run.BaseSHA
	}
	w.askQuestions(ctx, run, askerImplementer, "", ir.asked(), true, kind, subject)
}

// --- small helpers ---

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

// branchReview is what a review-only item's run checked before its review:
// the branch head and the checks' (and setup's) results there.
type branchReview struct {
	Item   *Item
	Head   string
	Checks []Check
	Setup  []Check
}

// stageBranchReview reviews a review-only item's existing branch
// (tmp/branch-handover — design.md §4): the repository's setup and checks at
// the branch head, read-only — no fix rounds, nothing committed or pushed —
// then the review, which gets the check results. Failed checks don't stop
// it: they are findings. The run succeeds whatever the verdict; the approver
// decides at in_review.
func (w *Worker) stageBranchReview(ctx context.Context, run *Run, log *slog.Logger) error {
	repo := w.repo(run.WorkspaceID)
	if repo == nil {
		return w.failNoRepo(run.WorkspaceID)
	}
	item, err := w.client.GetItem(ctx, run.ItemID)
	if err != nil {
		return err
	}
	choice, err := resolveBranch(ctx, repo, item)
	if err != nil {
		return w.report(ctx, run, false, map[string]any{"reason": "branch: " + err.Error()})
	}
	if choice.Question != "" {
		w.askQuestions(ctx, run, askerWorker, "", []itemtext.Question{{Text: choice.Question}}, true, "", "")
		return w.report(ctx, run, false, map[string]any{"reason": "needs clarification: " + choice.Question})
	}
	if !choice.Exists {
		return w.report(ctx, run, false, map[string]any{"reason": fmt.Sprintf("branch %s is not on origin: nothing to review", choice.Name)})
	}
	head, err := fetchBranch(ctx, repo.Path, choice.Name)
	if err != nil {
		return err
	}
	run.Branch = choice.Name
	// A review panel: every member reviews the same head, and the first
	// to finish its checks reports them; later members reuse those rather
	// than running setup and the checks again.
	var checks, setup []Check
	if run.GroupID != "" {
		checks = w.client.GroupChecks(ctx, run.ItemID, run.GroupID, head)
	}
	if len(checks) > 0 {
		log.Info("reusing the checks another reviewer ran on this head", "branch", choice.Name, "head", head)
	} else {
		log.Info("checking the branch before its review", "branch", choice.Name, "head", head)
		// Setup and checks run the branch's code: behind the review
		// stage's wrapper, when it has one (which needs the account).
		account := w.pickAccount(run, log)
		if account == nil {
			return w.noAccount(run, "review")
		}
		if checks, setup, err = w.checkHead(withShellWrap(ctx, w.stageShellWrap(run, account, "review")), run, repo, choice.Name, head, log); err != nil {
			return w.report(ctx, run, false, map[string]any{"reason": err.Error(), "setup": setup, "head_sha": head, "branch": choice.Name, "retryable": true})
		}
		if hung := stoppedChecks(checks); len(hung) > 0 {
			// A hung check says nothing about the branch: check again later.
			return w.report(ctx, run, false, map[string]any{"reason": "checks stopped before they finished: " + checkNames(hung),
				"checks": checks, "setup": setup, "head_sha": head, "branch": choice.Name, "retryable": true})
		}
	}
	w.setPhase(ctx, run, phaseReviewing)
	return w.review(ctx, run, log, &branchReview{Item: item, Head: head, Checks: checks, Setup: setup})
}

// checkHead runs the repository's setup and checks at a branch head without
// touching the branch. A branch checkout uses the clone (held for the run),
// detached at the head; setup is skipped when the clone is already set up
// for this branch at this head, and whatever the checks changed is thrown
// away before the clone goes back to its default branch — files a check
// writes are not anyone's work. A worktree checkout uses a detached worktree
// it removes afterwards. A setup failure is returned as the error.
func (w *Worker) checkHead(ctx context.Context, run *Run, repo *Repo, branch, head string, log *slog.Logger) (checks, setup []Check, err error) {
	if len(repo.Setup) == 0 && len(repo.Checks) == 0 {
		return nil, nil, nil
	}
	dir := repo.Path
	needSetup := true
	if repo.Checkout == checkoutBranch {
		// The marker tells readyClone, after a worker killed mid-checks,
		// that the detached, dirty clone is this run's leftovers to throw
		// away (not someone's work to refuse to touch).
		setCheckMarker(ctx, repo.Path, true)
		if _, err := git(ctx, repo.Path, "checkout", "--quiet", "--detach", head); err != nil {
			setCheckMarker(ctx, repo.Path, false)
			return nil, nil, fmt.Errorf("checkout %s: %w", shortSHA(head), err)
		}
		defer func() {
			// On its own context: a cancelled run still leaves the clone
			// clean, on its default branch.
			pctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := discardCheckRun(pctx, repo); err != nil {
				log.Warn("could not put the clone back on its default branch", "clone", repo.Path, "err", err)
			}
		}()
		cs := readCloneSetup(ctx, repo.Path)
		needSetup = cs.Branch != branch || cs.Head != head
	} else {
		dir = filepath.Join(w.cfg.StateDir, "worktrees", filepath.Base(repo.Path), "check-"+shortID(run.ID))
		removeWorktree(ctx, repo.Path, dir)
		if err := createDetached(ctx, repo.Path, dir, head); err != nil {
			return nil, nil, fmt.Errorf("worktree at %s: %w", shortSHA(head), err)
		}
		defer removeWorktree(ctx, repo.Path, dir)
	}
	if needSetup && len(repo.Setup) > 0 {
		if repo.Checkout == checkoutBranch {
			writeCloneSetup(ctx, repo.Path, cloneSetup{}) // set up for nothing until this finishes
		}
		setup, err = runSetup(ctx, dir, repo.Setup, repo.Path, repo.SetupEnv, w.cfg.TokenEnvs(), w.envFileKeys, repo.setupLimits())
		if err != nil {
			return nil, setup, err
		}
		if repo.Checkout == checkoutBranch {
			writeCloneSetup(ctx, repo.Path, cloneSetup{Branch: branch, Head: head})
		}
	}
	w.setPhase(ctx, run, phaseChecks)
	return w.repoChecks(ctx, repo, dir), setup, nil
}
