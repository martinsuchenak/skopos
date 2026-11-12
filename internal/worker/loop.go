package worker

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
)

//go:embed prompts/*.md
var promptFS embed.FS

var prompts = template.Must(template.New("").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).ParseFS(promptFS, "prompts/*.md"))

const planSchema = `{"type":"object","properties":{"title":{"type":"string"},"summary":{"type":"string"},"size":{"type":"string","enum":["s","m","l"]},"needs_clarification":{"type":"boolean"},"steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}},"risks":{"type":"array","items":{"type":"string"}},"questions":{"type":"array","items":{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}},"required":["question","options"]}},"skills":{"type":"array","items":{"type":"string"}}},"required":["summary","size","needs_clarification","steps","risks","questions"]}`

const implSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["done","amendment","blocked"]},"steps_done":{"type":"array","items":{"type":"integer"}},"summary":{"type":"string"},"reason":{"type":"string"},"questions":{"type":"array","items":{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}},"required":["question","options"]}},"amended_steps":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}},"required":["title","description"]}}},"required":["status","steps_done","summary"]}`

// guardSettings: deny rules that apply to every agent run (git push/remote/
// config, forge CLIs, network fetch tools, destructive commands). Deny
// beats any allow. The last four close options of allowed commands that
// reach past the allowlist (found live on Claude Code 2.1.318, whose own
// path checks cover cat/head/grep/find but not these): `git log|show|diff
// --output=<file>` writes any file — with --format, a core.fsmonitor line
// into .git/config, which runs a command at the next `git status`;
// `--pathspec-from-file=<file>` echoes a file outside the worktree in its
// error; `rg --pre <cmd>` runs a command (a script the agent wrote), and
// `rg --hostname-bin` one more. git rejects abbreviated long options here
// (`--outpu=` is an error), so the full names are enough.
var guardSettings = `{"permissions":{"deny":["Bash(git push:*)","Bash(git remote:*)","Bash(git config:*)","Bash(git branch:*)","Bash(git checkout:*)","Bash(git switch:*)","Bash(git reset --hard:*)","Bash(git rebase:*)","Bash(git worktree:*)","Bash(gh:*)","Bash(tea:*)","Bash(curl:*)","Bash(wget:*)","Bash(ssh:*)","Bash(scp:*)","Bash(rm -rf:*)","Bash(sudo:*)","Bash(git *--output*)","Bash(git *--pathspec-from-file*)","Bash(rg *--pre*)","Bash(rg *--hostname-bin*)"]}}`

var basePlanTools = []string{"Read", "Glob", "Grep"}
var planTools = basePlanTools

// baseImplTools: commits only as `git commit -m …` (the form the prompts
// give) — with -m, git refuses -F/--file/-c/-C, so a commit cannot take its
// message from a file outside the worktree (`git commit -F <file>` printed
// the worker's env file back, found live).
var baseImplTools = []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep", "TodoWrite",
	"Bash(git add:*)", "Bash(git commit -m:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
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
	// grants: the capabilities each run's stage profile granted (a hash),
	// reported apart from the executor.
	grants map[string]string
	// costs: what each run's agent turns cost, sent with its report.
	costs map[string]float64
	// lastSweep: when finished items' worktrees were last cleaned up.
	lastSweep time.Time
	// phases: what each running implement run is doing past its first turn
	// (run id → phase), sent with every heartbeat.
	phases sync.Map
	// health: each repo's last preflight (workspace → result).
	health map[string]preflightResult
	// Workspace config from the server (managed.go): what the worker holds
	// per managed workspace, why one cannot be used here, and the claim
	// answer's digest it was last refreshed for. cfgMu guards the two maps:
	// a background index pass reads repos while a run's config is applied.
	cfgMu          sync.RWMutex
	managed        map[string]*managedWS
	configProblems map[string]string
	configDigest   string
	// configStale: a run was executed under another revision than the one
	// idle work held, so the next idle poll refetches the newest.
	configStale bool
	// upgradeTried: when the last self-upgrade attempt failed (retried
	// after upgradeRetry); exePath and upgradeHTTP are swapped in tests.
	upgradeTried time.Time
	exePath      func() (string, error)
	upgradeHTTP  *http.Client
	// agentMCP: the MCP config file runs get when the worker has an agent
	// key (agentmcp.go); "" = runs get only the operator's mcp_config.
	// agentMCPRefresh rewrites it before each run (srv+ server URL).
	agentMCP        string
	agentMCPRefresh func() error
	// tokenRun: the run being executed came with an agent token (its id;
	// "" = none). runMCP: that run's own MCP config, written from the token
	// and removed when the run ends ("" when it could not be written — the
	// run then gets no skopos tools, never the shared agent key).
	// runSecrets: the token, redacted from the run's log. runTokensSeen:
	// the server issues run tokens (logged once).
	// facts: what the worker reports about its runners (facts.go), derived
	// once.
	facts    []policy.Facts
	factsSet bool
	factsMu  sync.Mutex
	// fleetLoaded: the server's runner definitions were fetched at least
	// once (defs.go).
	fleetLoaded bool
	// wrapperChecked: when each server-defined wrapper's check last ran
	// here (stagecaps.go). execProblems: what the worker last said it
	// cannot run under the server's config, to log only changes.
	wrapperChecked map[string]time.Time
	execProblems   []string
	tokenRun       string
	runMCP         string
	// runOwned: the server holds the MCP servers of the run being executed
	// (its claim answer carried them); runSkopos: the run's MCP file holds
	// the skopos entry; runMCPNames / runMCPNotes: the servers it got from
	// the server, and the ones left out with why (agentmcp.go).
	runOwned, runSkopos bool
	runMCPNames         []string
	runMCPNotes         []string
	runSecrets          []string
	runTokensSeen       bool
	// index: idle-time code indexing (indexer.go).
	index indexer
	// logOutput: the log output (--log-output-url) run logs also go to;
	// nil = none. extraSecrets: values run logs redact besides the ones
	// the config names (runlog.go).
	logOutput    io.Writer
	extraSecrets []string
	// paused: the pause the last claim answer named ("" = none), logged
	// when it changes.
	paused string
}

// SetLogOutput sends every run's log lines to the log output too.
func (w *Worker) SetLogOutput(out io.Writer) { w.logOutput = out }

func New(cfg *Config, client *Client, log *slog.Logger) *Worker {
	w := &Worker{cfg: cfg, client: client, log: log, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}, costs: map[string]float64{}, health: map[string]preflightResult{}}
	w.loadSessions()
	w.ApplyMCPServers()
	if client != nil {
		client.SetServerCommands(cfg.ServerCommands)
	}
	return w
}

// Loop runs until the context is cancelled: claim, execute, report; pause on
// quota (affinity keeps the run); idle polls at cfg.PollEvery.
func (w *Worker) Loop(ctx context.Context) error {
	w.sweepRunMCP()
	w.loadServerConfig(ctx)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Between runs, never during one: a worker that always finds work
		// would otherwise keep the runner definitions it started with.
		w.maybeRefreshFleet(ctx)
		// A wrapper the workspaces' config names has to start on this
		// host: checked before the claim says what the worker can run.
		w.checkWrappers(ctx, false)
		run, err := w.client.Claim(ctx, w.cfg.ID, w.cfg.AvailableRunners(time.Now()), w.cfg.Workspaces, w.checkHealth(ctx), w.indexable(), w.runnerFacts())
		if err != nil {
			w.log.Error("claim failed", "err", err)
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				return ctx.Err()
			}
			continue
		}
		if run == nil {
			if w.maybeUpgrade(ctx) {
				w.stopIndexing()
				return ErrRestartForUpgrade
			}
			// Paused (someone is working on this machine): no idle work
			// either — indexing and cleanup touch the clones.
			if w.notePause(w.client.Paused()) {
				w.stopIndexing()
			} else {
				w.maybeRefreshConfig(ctx)
				w.maybeSweep(ctx)
				w.maybeIndex(ctx)
			}
			if !sleepCtx(ctx, w.cfg.PollEvery) {
				w.stopIndexing()
				return ctx.Err()
			}
			continue
		}
		// A run comes first: indexing (idle work) stops before it starts.
		w.stopIndexing()
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

// notePause logs a change of pause and reports whether the worker is
// paused: until is "" (not paused), "resume" (until resumed) or the end.
func (w *Worker) notePause(until string) bool {
	if until != w.paused {
		switch until {
		case "":
			w.log.Info("resumed: taking work again")
		case "resume":
			w.log.Info("paused: taking no work until resumed")
		default:
			w.log.Info("paused: taking no work until " + until)
		}
		w.paused = until
	}
	return until != ""
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

// heartbeatEvery: how often a running run renews its lease (and learns
// whether skopos cancelled it). A variable for tests.
var heartbeatEvery = 30 * time.Second

// execute runs one claimed stage to a terminal report or pause.
func (w *Worker) execute(ctx context.Context, run *Run) {
	log := w.log.With("run", run.ID, "item", run.ItemID, "stage", run.Stage)
	// The run's own MCP config, when the claim came with an agent token —
	// before the run log opens, so its redactor knows the token.
	forgetMCP, err := w.prepareRunMCP(run)
	defer forgetMCP()
	// The runners' temporary directories of this run go with it.
	defer func() {
		if w.cfg.StateDir != "" && plainRunID(run.ID) {
			_ = os.RemoveAll(filepath.Join(w.cfg.runDir(run.ID), "tmp"))
		}
	}()
	if err != nil && run.MCP != nil {
		// The server holds the workspace's MCP servers and this run's file
		// could not be written: given back, not run without them.
		log.Error("not running the run", "reason", "the run's MCP config could not be written", "err", err)
		_ = w.report(ctx, run, false, map[string]any{"reason": "mcp: the run's MCP config could not be written on the worker: " + err.Error(), "retryable": true})
		return
	}
	if err != nil {
		log.Warn("the run's MCP config could not be written: its agents get no skopos tools (the shared agent key is not used in its place)", "err", err)
	}
	// The server holds the workspace's MCP servers and could not resolve
	// them for this run: given back, not run without them or with this
	// file's.
	if run.MCP != nil && run.MCP.Unreadable {
		log.Error("not running the run", "reason", "the server could not read the run's MCP servers")
		_ = w.report(ctx, run, false, map[string]any{"reason": "mcp: the server could not read the run's MCP servers", "retryable": true})
		return
	}
	// The run log streams from here to the end (runlog.go); its last lines
	// go after the report — skopos takes them under the claim's lease.
	rl := w.openRunLog(run)
	defer rl.close()
	ctx = withRunLog(ctx, rl)
	rl.notef("%s run, attempt %d, on %s", run.Stage, run.Attempt, w.cfg.ID)
	if run.MCP != nil {
		// Which MCP servers the run has, and which it should have had.
		rl.notef("MCP servers from the server: %s", cmp.Or(strings.Join(w.runMCPNames, ", "), "none"))
		for _, n := range w.runMCPNotes {
			log.Warn("mcp: " + n)
			rl.notef("%s", n)
		}
	}
	if w.agentMCPRefresh != nil {
		if err := w.agentMCPRefresh(); err != nil {
			log.Warn("agent mcp config not refreshed; runs keep the last one", "err", err)
		}
	}
	// Heartbeat while executing. A run skopos cancelled (the item was
	// rejected or marked done) is stopped: its context ends, which kills the
	// agent and everything it started; nothing more is pushed or reported.
	ctx, stopRun := context.WithCancelCause(ctx)
	defer stopRun(nil)
	hbCtx, hbStop := context.WithCancel(ctx)
	defer w.phases.Delete(run.ID)
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				phase, _ := w.phases.Load(run.ID)
				p, _ := phase.(string)
				err := w.client.Heartbeat(hbCtx, run.ID, w.cfg.ID, p)
				if c, ok := errors.AsType[*RunCancelledError](err); ok {
					log.Info("skopos cancelled the run; stopping it", "reason", c.Reason)
					rl.notef("stopping: skopos cancelled the run (%s)", c.Reason)
					stopRun(c)
					return
				}
				if err != nil {
					log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()
	defer hbStop()

	// The server named the runner and checked it against the workspace's
	// policy with what this worker reported. A run it could not have
	// checked — no runner, or one this worker does not have — is not run
	// on whatever account happens to be free.
	if why := w.refuseRunner(run); why != "" {
		log.Error("not running the run", "reason", why)
		rl.notef("not run: %s", why)
		_ = w.report(ctx, run, false, map[string]any{"reason": why})
		return
	}
	// The run executes under the config its claim named, and no other: a
	// revision that cannot be fetched or used here gives the run back.
	if err := w.holdConfig(ctx, run); err != nil {
		log.Error("not running the run", "reason", err)
		rl.notef("not run: %s", err)
		_ = w.report(ctx, run, false, map[string]any{"reason": "config: " + err.Error(), "retryable": true})
		return
	}
	// The config the run executes under says the server holds the
	// workspace's MCP servers, and the claim carried none: the run would
	// fall back to this file's mcp_config, which that config rules out.
	if m := w.managedFor(run.WorkspaceID); m != nil && m.mcpOwned && run.MCP == nil {
		why := "the workspace's config names its MCP servers, and the claim carried none"
		log.Error("not running the run", "reason", why)
		rl.notef("not run: %s", why)
		_ = w.report(ctx, run, false, map[string]any{"reason": "mcp: " + why, "retryable": true})
		return
	}
	// What contains the run depends on that config (its stage blocks and
	// the wrappers they name), so it is compared with what the claim
	// matched only now. A difference gives the run back: the next claim
	// reports what this worker would really run it in.
	w.checkWrappers(ctx, false)
	if why := w.refuseExecutor(run); why != "" {
		log.Error("not running the run", "reason", why)
		rl.notef("not run: %s", why)
		_ = w.report(ctx, run, false, map[string]any{"reason": why, "retryable": true})
		return
	}
	if run.ConfigRev != "" {
		note := "config revision " + run.ConfigRev
		if m := w.managedFor(run.WorkspaceID); m != nil && len(m.local) > 0 {
			// The revision does not describe these: say so on the run.
			note += " (" + strings.Join(m.local, ", ") + " from this worker's file: server_commands is off)"
		}
		rl.notef("%s", note)
	}

	// Transient failures retry in-place (round 7: a loop, not recursion —
	// one heartbeat goroutine for the whole stage, no nested stacks).
	const maxTransient = 3
	for attempt := 0; ; attempt++ {
		err := w.runStage(ctx, run, log)
		if c, ok := errors.AsType[*RunCancelledError](context.Cause(ctx)); ok {
			// Whatever the stage returned, it was cut off: skopos already
			// has the run's outcome, so nothing is reported.
			log.Info("run stopped", "reason", c.Reason)
			rl.notef("stopped: %s", c.Reason)
			return
		}
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
			rl.line("worker", fmt.Sprintf("transient failure (%s), retrying in %s", tr.detail, time.Duration(attempt+1)*30*time.Second), true)
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
	if repo := w.repo(run.WorkspaceID); repo != nil && repo.Checkout == checkoutBranch {
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
		rl := runLogFrom(ctx)
		rl.line("worker", "paused until "+resumeAt.Format(time.RFC3339)+": "+reason, true)
		rl.flush()
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
			if reset.IsZero() || a.quota().exhaustedUntil.Before(reset) {
				reset = a.quota().exhaustedUntil
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
		// The account this run used before — as long as it still is the
		// runner the server named. The claim checked run.Runner against
		// the workspace's policy; a run that comes back for another runner
		// (the policy or the preference changed while it was paused, a
		// takeover of its own run) must not carry on with the old account
		// and its session. One login may feed several defined runners: the
		// one for this run's runner is the run's account.
		was := ""
		for _, a := range w.cfg.Accounts {
			if a.Name != name {
				continue
			}
			if run.Runner == "" || accountRunner(a) == run.Runner {
				return a
			}
			was = accountRunner(a)
		}
		if was != "" {
			log.Info("the run now names another runner; dropping its earlier account and session", "was", was, "now", run.Runner)
		}
		w.forgetRun(run.ID)
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
			if now.After(cand.quota().exhaustedUntil) && w.cfg.canRunStage(accountRunner(cand), run.Stage) {
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
	runner := account.Runner
	if runner == "" {
		runner = "claude"
	}
	prof, problem := w.cfg.stageProfileFor(run.WorkspaceID, phase, runner)
	// What contains this run and what it was given, for the report: the
	// server compares the executor with the one the claim matched on.
	w.executors[run.ID] = w.cfg.ExecutorFor(run.WorkspaceID, run.Stage, runner)
	w.grants[run.ID] = prof.Grants()
	mcpConfig, withSkopos := w.mcpConfigFor(run, runner)
	if withSkopos {
		tools = append(slices.Clone(tools), skoposTools(phase)...)
	}
	ar := AgentRun{
		Bin: w.cfg.RunnerBin(runner), Dir: dir, Prompt: prompt, Runner: runner, Model: account.Model, Effort: account.Effort,
		SessionID: sess, Resume: sess != "",
		Schema: schema, AllowedTools: tools,
		Settings:  settingsJSON(w.mcpDeny(run.WorkspaceID), ""),
		Timeout:   timeout,
		LogFile:   filepath.Join(w.cfg.StateDir, "runs", run.ID, phase+".log"),
		Env:       account.Env,
		TokenEnvs: w.cfg.TokenEnvs(),
		MCPConfig: mcpConfig,

		FileEnvKeys:   w.envFileKeys,
		TempDir:       w.cfg.runTempDir(run.ID),
		HostContained: w.cfg.hostContainer(prof) != "",
	}
	if problem != "" {
		// The stage's wrapper cannot be used here: never run it bare.
		ar.StartErr = problem
		return ar
	}
	if prof == nil {
		return ar
	}
	// The stage profile (operator-supplied, validated fail-closed at load):
	// capabilities extend the run, the wrapper contains it. runAgent
	// re-checks containment per runner (sandbox settings only contain
	// runners that honor them).
	ar.Granted = prof.grants()
	ar.Sandboxed = prof.sandboxed()
	ar.AllowedTools = append(append([]string{}, tools...), prof.Tools...)
	ar.Settings = settingsJSON(w.mcpDeny(run.WorkspaceID), prof.Settings)
	if len(prof.Env) > 0 {
		env := make(map[string]string, len(account.Env)+len(prof.Env))
		for k, v := range account.Env {
			env[k] = v
		}
		for k, v := range prof.Env {
			// What the server gives a stage never replaces a variable of
			// the account: that is the login and the endpoint the runner's
			// facts were derived from.
			if _, own := account.Env[k]; own && prof.server {
				continue
			}
			env[k] = v
		}
		ar.Env = env
	}
	if len(prof.Wrapper) > 0 {
		wrapper, err := w.stageWrapper(prof, dir, phase, run, account)
		if err != nil {
			ar.StartErr = err.Error()
		}
		ar.Wrapper = wrapper
		if strings.Contains(strings.Join(prof.Wrapper, " "), "{cli}") {
			ar.WrapBin = cliPath(ar.Bin)
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
		if g := w.grants[run.ID]; g != "" {
			fields["grants"] = g
		}
	}
	delete(w.executors, run.ID)
	delete(w.grants, run.ID)
	if c, ok := w.costs[run.ID]; ok {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["cost_usd"] = c
		delete(w.costs, run.ID)
	}
	if rl := runLogFrom(ctx); rl != nil {
		if succeeded {
			rl.notef("%s succeeded", run.Stage)
		} else {
			rl.line("worker", fmt.Sprintf("%s failed: %v", run.Stage, fields["reason"]), true)
		}
		// Out before the report: a retried run's lease is gone after it.
		rl.flush()
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
func (w *Worker) SetEnvFileKeys(keys []string) {
	w.envFileKeys = keys
	w.cfg.SetEnvFileKeys(keys) // the runner facts ignore them too (facts.go)
}

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

// forgetRun drops the account and the agent sessions a run used before: it
// starts again on whatever account fits the runner it now names.
func (w *Worker) forgetRun(runID string) {
	delete(w.accounts, runID)
	for key := range w.sessions {
		if strings.HasPrefix(key, runID+":") {
			delete(w.sessions, key)
		}
	}
	w.persist()
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

// runnerFacts is what the worker reports about its runners with every
// claim (facts.go), derived once: the config does not change while it runs.
func (w *Worker) runnerFacts() []policy.Facts {
	w.factsMu.Lock()
	defer w.factsMu.Unlock()
	if !w.factsSet {
		w.facts, w.factsSet = w.cfg.RunnerFacts(), true
		if w.client != nil {
			w.client.SetExecutors(w.cfg.ExecutorFacts())
		}
		// What the worker cannot run under the server's config, said once
		// per change (it reports those cases as executor none:…).
		problems := w.cfg.ExecutorProblems()
		for _, p := range problems {
			if !slices.Contains(w.execProblems, p) {
				w.log.Warn("stages: " + p)
			}
		}
		w.execProblems = problems
	}
	return w.facts
}

// refuseExecutor says why a claimed run must not be executed ("" = run
// it): the server matched the workspace's policy on the executor this
// worker reported for the run, and recorded it on the run. When the worker
// would now run it in something else — its configuration changed since the
// claim, or the server read the report differently — the policy check does
// not cover what would run, so nothing is started.
func (w *Worker) refuseExecutor(run *Run) string {
	if run.Runner != "" {
		// Whatever the claim matched: a case this worker reports it cannot
		// run is not run.
		if here := w.cfg.ExecutorFor(run.WorkspaceID, run.Stage, run.Runner); policy.Unavailable(here) {
			why := policy.UnavailableReason(here)
			if _, problem := w.cfg.stageProfileFor(run.WorkspaceID, stagePhase(run.Stage), run.Runner); problem != "" {
				why = problem
			}
			return fmt.Sprintf("this worker cannot run %s on %s here: %s", run.Stage, run.Runner, why)
		}
	}
	if len(run.Facts) == 0 || run.Runner == "" {
		return ""
	}
	var claimed struct {
		Executor string `json:"executor"`
	}
	if json.Unmarshal(run.Facts, &claimed) != nil || claimed.Executor == "" {
		return ""
	}
	if here := w.cfg.ExecutorFor(run.WorkspaceID, run.Stage, run.Runner); here != claimed.Executor {
		return fmt.Sprintf("the server matched this run on executor %s; this worker would run it in %s", claimed.Executor, here)
	}
	return ""
}

// refuseRunner says why a claimed run must not be executed ("" = run it):
// from a server that reads runner facts, every run names a runner, and one
// of those this worker reported. Without that the policy check behind the
// claim did not cover what would run here. A server from before runner
// facts hands out unpinned runs as it always did.
func (w *Worker) refuseRunner(run *Run) string {
	if w.client == nil || !w.client.FactsUsed() {
		return ""
	}
	if run.Runner == "" {
		return "the server handed out a run without a runner although it reads this worker's runner facts"
	}
	for _, f := range w.runnerFacts() {
		if f.Name == run.Runner {
			return ""
		}
	}
	return fmt.Sprintf("the server handed out a run for runner %q, which this worker does not have", run.Runner)
}
