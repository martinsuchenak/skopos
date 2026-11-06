package worker

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = template.Must(template.New("").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).ParseFS(promptFS, "prompts/*.md"))

const planSchema = `{"type":"object","properties":{"title":{"type":"string"},"summary":{"type":"string"},"size":{"type":"string","enum":["s","m","l"]},"needs_clarification":{"type":"boolean"},"steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}},"risks":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"string"}},"skills":{"type":"array","items":{"type":"string"}}},"required":["summary","size","needs_clarification","steps","risks","questions"]}`

const implSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["done","amendment","blocked"]},"steps_done":{"type":"array","items":{"type":"integer"}},"summary":{"type":"string"},"reason":{"type":"string"},"question":{"type":"string"},"amended_steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}}},"required":["status","steps_done","summary"]}`

// guardSettings: deny rules that apply to every agent run (git push/remote/
// config, forge CLIs, network fetch tools, destructive commands). Deny
// beats any allow.
var guardSettings = `{"permissions":{"deny":["Bash(git push:*)","Bash(git remote:*)","Bash(git config:*)","Bash(git branch:*)","Bash(git checkout:*)","Bash(git switch:*)","Bash(git reset --hard:*)","Bash(git rebase:*)","Bash(git worktree:*)","Bash(gh:*)","Bash(tea:*)","Bash(curl:*)","Bash(wget:*)","Bash(ssh:*)","Bash(scp:*)","Bash(rm -rf:*)","Bash(sudo:*)"]}}`

var basePlanTools = []string{"Read", "Glob", "Grep"}
var planTools = basePlanTools

var baseImplTools = []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep", "TodoWrite",
	"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
	"Bash(git show:*)", "Bash(git restore:*)", "Bash(pwd)", "Bash(ls:*)", "Bash(find:*)", "Bash(grep:*)", "Bash(rg:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(mkdir:*)"}
var implTools = baseImplTools

var reviewTools = []string{"Read", "Glob", "Grep", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)"}

// ApplyMCPServers extends the tool allowlists with the configured MCP
// servers (the agent may use them; the deny list still wins per tool).
func (w *Worker) ApplyMCPServers() {
	if len(w.cfg.MCPServers) == 0 {
		return
	}
	planTools = append(append([]string{}, basePlanTools...), w.cfg.MCPServers...)
	implTools = append(append([]string{}, baseImplTools...), w.cfg.MCPServers...)
}

// settingsJSON merges the hard guards with the deployment's MCP deny list
// and the stage profile's settings (capability profiles §2). The profile's
// JSON is kept as given — e.g. Claude Code's sandbox block — except that
// skopos's deny list is always unioned in (deny beats allow) and a profile
// cannot pick the permission mode (the runner sets dontAsk).
func settingsJSON(mcpDeny []string, profile string) string {
	var g struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(guardSettings), &g)
	deny := append(g.Permissions.Deny, mcpDeny...)
	out := map[string]any{}
	if strings.TrimSpace(profile) != "" {
		_ = json.Unmarshal([]byte(profile), &out) // validated at config load
	}
	perms, _ := out["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	if extra, ok := perms["deny"].([]any); ok {
		for _, d := range extra {
			if s, ok := d.(string); ok {
				deny = append(deny, s)
			}
		}
	}
	perms["deny"] = deny
	delete(perms, "defaultMode")
	out["permissions"] = perms
	b, _ := json.Marshal(out)
	return string(b)
}

// Worker is the execution loop.
type Worker struct {
	cfg    *Config
	client *Client
	log    *slog.Logger
	// sessions persists per-run agent session ids and accounts (stickiness:
	// a re-claimed run resumes its own session on its own account).
	sessions map[string]string
	accounts map[string]string
	// envFileKeys are the variables the env file provided (review): the
	// scrubber drops them from every agent environment — env-file values
	// are the daemon's secrets, not the agent's.
	envFileKeys []string
	// executors remembers how each run's agent stage was executed (the
	// stage profile's Executor id) so every report can record it.
	executors map[string]string
	// costs: what each run's agent turns cost, sent with its report.
	costs map[string]float64
	// lastSweep: when finished items' worktrees were last cleaned up.
	lastSweep time.Time
	// phases: what each running implement run is doing past its first turn
	// (run id → phase), sent with every heartbeat.
	phases sync.Map
	// health: each repo's last preflight (workspace → result).
	health map[string]preflightResult
	// upgradeTried: when the last self-upgrade attempt failed (retried
	// after upgradeRetry); exePath and upgradeHTTP are swapped in tests.
	upgradeTried time.Time
	exePath      func() (string, error)
	upgradeHTTP  *http.Client
}

func New(cfg *Config, client *Client, log *slog.Logger) *Worker {
	w := &Worker{cfg: cfg, client: client, log: log, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, costs: map[string]float64{}, health: map[string]preflightResult{}}
	w.loadSessions()
	w.ApplyMCPServers()
	return w
}

// Loop runs until the context is cancelled: claim, execute, report; pause on
// quota (affinity keeps the run); idle polls at cfg.PollEvery.
func (w *Worker) Loop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		run, err := w.client.Claim(ctx, w.cfg.ID, w.cfg.AvailableRunners(time.Now()), w.cfg.Workspaces, w.checkHealth(ctx))
		if err != nil {
			w.log.Error("claim failed", "err", err)
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				return ctx.Err()
			}
			continue
		}
		if run == nil {
			if w.maybeUpgrade(ctx) {
				return ErrRestartForUpgrade
			}
			w.maybeSweep(ctx)
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				return ctx.Err()
			}
			continue
		}
		// N5: the run's stage must match the item's phase. A queued-then-
		// rejected item, a bypassed authorization, a stale resume — the
		// claim-time move is best-effort, so the worker verifies.
		if it, ierr := w.client.GetItem(ctx, run.ItemID); ierr == nil && !PhaseAdmissible(it.Status, run.Stage) {
			w.log.Warn("abandoning run: item not in its phase", "run", run.ID, "stage", run.Stage, "item_status", it.Status)
			_ = w.client.CancelRun(ctx, run.ID, w.cfg.ID, fmt.Sprintf("item is %s, not admissible for a %s run", it.Status, run.Stage))
			continue
		}
		w.execute(ctx, run)
	}
}

// PhaseAdmissible: the item statuses under which a stage's run may execute.
func PhaseAdmissible(itemStatus, stage string) bool {
	switch stage {
	case "plan", "compare", "synthesize":
		return itemStatus == "queued" || itemStatus == "planning"
	case "implement", "review", "qa":
		return itemStatus == "approved" || itemStatus == "implementing"
	case "second_opinion":
		// Advisory, on a finished branch: never moves the item.
		return itemStatus == "in_review" || itemStatus == "blocked"
	case "branch_review":
		return itemStatus == "queued" || itemStatus == "implementing"
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// execute runs one claimed stage to a terminal report or pause.
func (w *Worker) execute(ctx context.Context, run *Run) {
	log := w.log.With("run", run.ID, "item", run.ItemID, "stage", run.Stage)
	// Heartbeat while executing.
	hbCtx, hbStop := context.WithCancel(ctx)
	defer w.phases.Delete(run.ID)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				phase, _ := w.phases.Load(run.ID)
				p, _ := phase.(string)
				if err := w.client.Heartbeat(hbCtx, run.ID, w.cfg.ID, p); err != nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()
	defer hbStop()

	// Transient failures retry in-place (round 7: a loop, not recursion —
	// one heartbeat goroutine for the whole stage, no nested stacks).
	const maxTransient = 3
	for attempt := 0; ; attempt++ {
		err := w.runStage(ctx, run, log)
		if err == nil {
			return // the stage reported its own outcome
		}
		if tr, ok := err.(*transientError); ok {
			// Transient (overloaded / 5xx / network): retry with back-off
			// instead of failing the run and blocking the item (review M4).
			// The run stays claimed by us throughout.
			if attempt+1 >= maxTransient {
				// Still failing: skopos may retry it later, elsewhere.
				_ = w.report(ctx, run, false, map[string]any{"reason": fmt.Sprintf("transient x%d: %s", maxTransient, tr.detail), "retryable": true})
				return
			}
			log.Warn("transient failure, retrying", "attempt", attempt+1, "detail", tr.detail)
			sleepCtx(ctx, time.Duration(attempt+1)*30*time.Second)
			continue
		}
		w.finish(ctx, run, log, err)
		return
	}
}

// runStage runs one attempt of the run's stage. A branch-checkout clone is
// held for the attempt, on its default branch at the start, whatever the
// stage: plans read its working tree, implementations check out in it, and
// every stage fetches into it. The back-offs between attempts and after a
// quota pause run without it.
func (w *Worker) runStage(ctx context.Context, run *Run, log *slog.Logger) error {
	if repo := w.cfg.Repos[run.WorkspaceID]; repo != nil && repo.Checkout == checkoutBranch {
		release, err := w.holdClone(ctx, repo, log)
		if err != nil {
			return err
		}
		defer release()
	}
	switch run.Stage {
	case "plan":
		return w.stagePlan(ctx, run, log)
	case "implement":
		return w.stageImplement(ctx, run, log)
	case "review", "qa", "second_opinion":
		return w.stageReview(ctx, run, log)
	case "branch_review":
		return w.stageBranchReview(ctx, run, log)
	case "compare":
		return w.stageCompare(ctx, run, log)
	case "synthesize":
		return w.stageSynthesize(ctx, run, log)
	}
	return fmt.Errorf("unknown stage %q", run.Stage)
}

// setPhase records what the run is doing (phaseChecks / phaseFixing)
// and tells skopos at once, so the dashboard's Checks step shows it running
// without waiting for the next heartbeat. Later heartbeats repeat it.
func (w *Worker) setPhase(ctx context.Context, run *Run, phase string) {
	w.phases.Store(run.ID, phase)
	if err := w.client.Heartbeat(ctx, run.ID, w.cfg.ID, phase); err != nil {
		w.log.Warn("phase heartbeat failed", "run", run.ID, "phase", phase, "err", err)
	}
}

// finish applies a non-transient stage outcome: quota pauses the run (the
// server's resume_after brings it back), anything else is a failed report.
func (w *Worker) finish(ctx context.Context, run *Run, log *slog.Logger, err error) {
	if q, ok := err.(*quotaError); ok {
		// Quota (round-2 N3): pause, keeping affinity. The owner reclaims
		// the run through resume_after on any claim once the reset passes.
		reason := "quota: " + q.detail
		resumeAt := q.resetAt
		if resumeAt.IsZero() {
			// R4: no parsed reset — the same 30-minute back-off the sleep
			// uses, but SERVER-side, so the pause survives a restart.
			resumeAt = time.Now().Add(30 * time.Minute)
		}
		_ = w.client.Pause(ctx, run.ID, w.cfg.ID, reason, resumeAt)
		// Another account (or runner) can still work: keep claiming — the
		// claim no longer offers exhausted runners. Only when nothing at all
		// is usable does the worker sleep through the back-off.
		if w.cfg.anyAccountAvailable(time.Now()) {
			log.Warn("quota pause (other accounts available)", "until", resumeAt, "detail", q.detail)
			return
		}
		wait := time.Until(q.resetAt)
		if wait <= 0 || wait > 30*time.Minute {
			wait = 30 * time.Minute
		}
		log.Warn("quota pause", "until", q.resetAt, "backoff", wait, "detail", q.detail)
		sleepCtx(ctx, wait)
		return
	}
	fields := map[string]any{"reason": err.Error()}
	if isRetryable(err) {
		fields["retryable"] = true
	}
	_ = w.report(ctx, run, false, fields)
	log.Error("stage failed", "err", err, "retryable", fields["retryable"] != nil)
}

type quotaError struct {
	detail  string
	resetAt time.Time
	account *Account
}

func (e *quotaError) Error() string { return "quota: " + e.detail }

// noAccount is the outcome when pickAccount finds nothing for a run: a
// quota pause until the earliest of the runner's accounts resets, or — when
// no account feeds the run's runner at all — a failure that says so (a
// pause would just repeat forever).
func (w *Worker) noAccount(run *Run, what string) error {
	runner := run.Runner
	if runner == "" {
		// Unpinned: wait for the first account that can run this stage —
		// not merely any account (a read-only runner's never can implement).
		var reset time.Time
		for _, a := range w.cfg.Accounts {
			if !w.cfg.canRunStage(accountRunner(a), run.Stage) {
				continue
			}
			if reset.IsZero() || a.exhaustedUntil.Before(reset) {
				reset = a.exhaustedUntil
			}
		}
		return &quotaError{detail: "no available account for " + what, resetAt: reset}
	}
	reset, found := w.cfg.NextAvailable(runner, time.Now())
	if !found {
		return fmt.Errorf("no account feeds runner %q (the stage preference for this %s) — add a [[worker.accounts]] entry with runner = %q", runner, what, runner)
	}
	return &quotaError{detail: fmt.Sprintf("no available %s account for %s", runner, what), resetAt: reset}
}

// retryableError marks a failure that is the worker's problem, not the
// item's: setup failed or hung, a check hung, git (fetch, checkout,
// worktree) failed, the push was rejected, the provider kept failing. The
// report says retryable, and skopos queues the run again (another worker
// may take it) until its retries are spent, instead of blocking the item
// (tmp/self-healing-workers — design.md §1). Anything unmarked blocks.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// retryable marks err retryable, unless it needs a person.
func retryable(err error) error {
	var p *needsPersonError
	if err == nil || errors.As(err, &p) {
		return err
	}
	return &retryableError{err: err}
}

func isRetryable(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

// needsPersonError: a failure only a person can resolve (a merge conflict,
// a branch rewritten since approval), so no retry would help even where
// the surrounding step is otherwise retryable.
type needsPersonError struct{ err error }

func (e *needsPersonError) Error() string { return e.err.Error() }
func (e *needsPersonError) Unwrap() error { return e.err }

func needsPerson(err error) error { return &needsPersonError{err: err} }

type transientError struct{ detail string }

func (e *transientError) Error() string { return "transient: " + e.detail }

// pickAccount selects the credential for a run on the stage's runner.
// Stickiness first (review finding 6): a resumed run reuses the account
// that owns its session — recorded when the session was created. Rotation
// happens only for genuinely new runs.
func (w *Worker) pickAccount(run *Run, log *slog.Logger) *Account {
	if name := w.accounts[run.ID]; name != "" {
		for _, a := range w.cfg.Accounts {
			if a.Name == name {
				return a
			}
		}
	}
	// A run that names a runner (the workspace's stage preference — e.g.
	// plan on opencode-glm, implement on opencode-qwen) is PINNED to it: it
	// never falls back to another runner's account, it waits for its own
	// (noAccount pauses it until the earliest reset). Only a run with no
	// preference may use any available account.
	runner := run.Runner
	pinned := runner != ""
	if !pinned {
		runner = "claude"
	}
	now := time.Now()
	a := w.cfg.AccountFor(runner, now)
	if a == nil && !pinned {
		// Fall back to any account of any runner offered (single-runner
		// deployments have exactly one) — one whose runner can run the
		// stage: a read-only runner (codex) would refuse implement and fail
		// the item, where waiting for a claude reset only pauses it.
		for _, cand := range w.cfg.Accounts {
			if now.After(cand.exhaustedUntil) && w.cfg.canRunStage(accountRunner(cand), run.Stage) {
				a = cand
				break
			}
		}
	}
	return a
}

func (w *Worker) agentRun(dir, prompt, schema string, tools []string, timeout time.Duration, account *Account, run *Run, phase string) AgentRun {
	w.rememberAccount(run, account)
	sessionKey := run.ID + ":" + phase
	sess := w.sessions[sessionKey]
	prof := w.cfg.StageProfile(run.WorkspaceID, phase)
	w.executors[run.ID] = prof.Executor()
	runner := account.Runner
	if runner == "" {
		runner = "claude"
	}
	ar := AgentRun{
		Bin: w.cfg.RunnerBin(runner), Dir: dir, Prompt: prompt, Runner: runner, Model: account.Model,
		SessionID: sess, Resume: sess != "",
		Schema: schema, AllowedTools: tools,
		Settings:  settingsJSON(w.cfg.MCPDeny, ""),
		Timeout:   timeout,
		LogFile:   filepath.Join(w.cfg.StateDir, "runs", run.ID, phase+".log"),
		Env:       account.Env,
		TokenEnvs: w.cfg.TokenEnvs(),
		MCPConfig: w.cfg.MCPConfig,

		FileEnvKeys: w.envFileKeys,
	}
	if prof == nil {
		return ar
	}
	// The stage profile (operator-supplied, validated fail-closed at load):
	// capabilities extend the run, the wrapper contains it. runAgent
	// re-checks containment per runner (sandbox settings only contain
	// runners that honor them).
	ar.Granted = len(prof.Tools) > 0 || len(prof.Env) > 0 || prof.grantsAllow()
	ar.Sandboxed = prof.sandboxed()
	ar.AllowedTools = append(append([]string{}, tools...), prof.Tools...)
	ar.Settings = settingsJSON(w.cfg.MCPDeny, prof.Settings)
	if len(prof.Env) > 0 {
		env := make(map[string]string, len(account.Env)+len(prof.Env))
		for k, v := range account.Env {
			env[k] = v
		}
		for k, v := range prof.Env {
			env[k] = v
		}
		ar.Env = env
	}
	if len(prof.Wrapper) > 0 {
		r := strings.NewReplacer("{dir}", dir, "{run}", run.ID, "{stage}", phase, "{workspace}", run.WorkspaceID)
		ar.Wrapper = make([]string, len(prof.Wrapper))
		for i, a := range prof.Wrapper {
			ar.Wrapper[i] = r.Replace(a)
		}
	}
	return ar
}

// report sends a run result with the executor that ran its agent stage
// (capability profiles §2: the audit trail names the envelope).
func (w *Worker) report(ctx context.Context, run *Run, succeeded bool, fields map[string]any) error {
	if ex := w.executors[run.ID]; ex != "" {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["executor"] = ex
	}
	delete(w.executors, run.ID)
	if c, ok := w.costs[run.ID]; ok {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["cost_usd"] = c
		delete(w.costs, run.ID)
	}
	return w.client.ReportResult(ctx, run.ID, w.cfg.ID, succeeded, fields)
}

// addCost records what an agent run cost (summed over a stage's turns);
// report() sends it with the result. Runners that report nothing add 0 and
// stay unreported.
func (w *Worker) addCost(run *Run, res AgentResult) {
	if res.CostUSD <= 0 {
		return
	}
	if w.costs == nil {
		w.costs = map[string]float64{}
	}
	w.costs[run.ID] += res.CostUSD
}

// SetEnvFileKeys records which variables came from the env file (the CLI
// loads it before the daemon starts).
func (w *Worker) SetEnvFileKeys(keys []string) { w.envFileKeys = keys }

func (w *Worker) saveSession(run *Run, phase, sessionID string) {
	if sessionID == "" {
		return
	}
	if w.sessions == nil {
		w.sessions = map[string]string{}
	}
	w.sessions[run.ID+":"+phase] = sessionID
	w.persist()
}

// persist writes sessions AND accounts in one format (review H7: a flat
// sessions-only write followed by a nested reload nil'd both maps and
// crashed the worker on its next save — a crash loop under systemd).
func (w *Worker) persist() {
	if w.sessions == nil {
		w.sessions = map[string]string{}
	}
	if w.accounts == nil {
		w.accounts = map[string]string{}
	}
	b, err := json.Marshal(map[string]map[string]string{"sessions": w.sessions, "accounts": w.accounts})
	if err != nil {
		return
	}
	_ = os.MkdirAll(w.cfg.StateDir, 0o700)
	_ = os.WriteFile(filepath.Join(w.cfg.StateDir, "sessions.json"), b, 0o600)
}

func (w *Worker) rememberAccount(run *Run, a *Account) {
	if a == nil {
		return
	}
	if w.accounts == nil {
		w.accounts = map[string]string{}
	}
	if w.accounts[run.ID] == "" {
		w.accounts[run.ID] = a.Name
		w.persist()
	}
}

func (w *Worker) loadSessions() {
	// Nil-safe for BOTH formats: the nested {sessions, accounts} shape and
	// the legacy flat map (which carried sessions only).
	b, err := os.ReadFile(filepath.Join(w.cfg.StateDir, "sessions.json"))
	if err != nil {
		w.sessions, w.accounts = map[string]string{}, map[string]string{}
		return
	}
	var state struct {
		Sessions map[string]string `json:"sessions"`
		Accounts map[string]string `json:"accounts"`
	}
	if json.Unmarshal(b, &state) == nil && state.Sessions != nil {
		w.sessions = state.Sessions
		w.accounts = state.Accounts
	} else {
		var flat map[string]string
		if json.Unmarshal(b, &flat) == nil && flat != nil {
			w.sessions = flat
		}
	}
	if w.sessions == nil {
		w.sessions = map[string]string{}
	}
	if w.accounts == nil {
		w.accounts = map[string]string{}
	}
}

// classify converts an agent outcome into the worker's error protocol:
// nil = handled (reported), quotaError = pause, error = failed report.
func (w *Worker) handleAgentOutcome(ctx context.Context, run *Run, res AgentResult, account *Account, phase string) error {
	w.saveSession(run, phase, res.SessionID)
	switch res.Outcome {
	case OutcomeOK:
		return nil
	case OutcomeQuota:
		if account != nil {
			account.MarkExhausted(res.ResetAt, 30*time.Minute, time.Now())
		}
		return &quotaError{detail: res.Detail, resetAt: res.ResetAt, account: account}
	case OutcomeTransient:
		return &transientError{detail: res.Detail}
	case OutcomeAuth:
		return fmt.Errorf("auth (fix the account login): %s", res.Detail)
	default:
		return fmt.Errorf("%s", res.Detail)
	}
}

func renderPrompt(name string, data any) (string, error) {
	var b strings.Builder
	// ParseFS names templates by BASENAME (plan.md), not the glob path —
	// looking up "prompts/plan.md" found nothing and no stage could render
	// its prompt (the first live worker run found it).
	if err := prompts.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Output string `json:"output,omitempty"`
	// Stopped: the check didn't finish — check_timeout or quiet_timeout
	// stopped it, or the timeout came before it ran. A hang is the
	// worker's problem, not the implementer's: never a fix round.
	Stopped bool `json:"stopped,omitempty"`
}
