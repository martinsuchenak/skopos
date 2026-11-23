package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

const (
	// defaultPreflightInterval: how often a repo's preflight runs unless
	// preflight_interval says otherwise.
	defaultPreflightInterval = 5 * time.Minute
	maxPreflightInterval     = wsconfig.MaxPreflightInterval
)

// preflightTimeout bounds one repo's preflight commands together.
var preflightTimeout = 2 * time.Minute

// preflightResult is a repo's last preflight: when it ran and, when it
// failed, why ("" = healthy).
type preflightResult struct {
	at     time.Time
	reason string
}

// PreflightEvery is the repo's preflight_interval, defaulted.
func (r *Repo) PreflightEvery() time.Duration {
	if r.PreflightInterval > 0 {
		return r.PreflightInterval
	}
	return defaultPreflightInterval
}

// checkHealth runs every repo preflight that is due and returns the
// workspaces whose last preflight failed (workspace → reason), for the
// claim: skopos hands this worker none of their runs, so another worker
// takes them, instead of this one failing them on a broken environment.
func (w *Worker) checkHealth(ctx context.Context) map[string]string {
	var out map[string]string
	now := time.Now()
	// Whether codex's sandbox can start here (codexsandbox.go): a failure
	// holds back the codex runner's runs only.
	if w.checkCodexSandbox(ctx) {
		w.resetFacts()
	}
	for _, ws := range w.cfg.Workspaces {
		// The server's config for the workspace cannot be used on this
		// host (managed.go): none of its runs until that is fixed.
		if why := w.configProblem(ws); why != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[ws] = why
			continue
		}
		// The clone the worker makes itself is not there yet (clone.go):
		// nothing of the workspace can run, and nothing else is checked.
		if why := w.cloneProblem(ws); why != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[ws] = why
			continue
		}
		// A branch-checkout clone holding someone's files: every run
		// would stop on it (readyClone), so another worker takes them.
		if why := w.cloneDirt(ctx, ws); why != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[ws] = why
			continue
		}
		// What the workspace's runs need on this host: the sandbox, the
		// commands its stages run, the push token (stagecheck.go). A
		// failure holds back only the stages it concerns.
		if w.checkStages(ctx, ws) {
			w.resetFacts()
		}
		repo := w.repo(ws)
		if repo == nil || len(repo.Preflight) == 0 {
			continue
		}
		last, seen := w.health[ws]
		if !seen || now.Sub(last.at) >= repo.PreflightEvery() {
			reason := w.preflight(ctx, repo)
			if ctx.Err() != nil {
				return out // shutting down: not a verdict on the environment
			}
			switch {
			case reason != "" && (!seen || last.reason == ""):
				w.log.Warn("preflight failed: not taking this repo's work until it passes", "workspace", ws, "reason", reason)
			case reason == "" && seen && last.reason != "":
				w.log.Info("preflight passes again", "workspace", ws)
			}
			last = preflightResult{at: now, reason: reason}
			w.health[ws] = last
		}
		if last.reason != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[ws] = last.reason
		}
	}
	return out
}

// preflight runs a repo's preflight commands in its clone, contained and
// bounded like setup, and says why it failed ("" = all passed).
func (w *Worker) preflight(ctx context.Context, repo *Repo) string {
	pctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	env := scrubEnv(repo.SetupEnv, w.cfg.TokenEnvs(), w.envFileKeys)
	for _, c := range repo.Preflight {
		out, err := runShell(pctx, repo.Path, env, c, 0)
		if err != nil {
			if pctx.Err() != nil && ctx.Err() == nil {
				err = fmt.Errorf("timed out after %s", preflightTimeout)
			}
			return fmt.Sprintf("preflight %q failed: %v: %s", c, err, tail(string(out), 300))
		}
	}
	return ""
}

// cloneDirtEvery: how often an idle worker looks at a branch-checkout
// clone (git status on a large checkout is not free; the claim polls
// every few seconds).
const cloneDirtEvery = time.Minute

// cloneDirt says why a branch-checkout clone cannot take a run now ("" =
// it can), as readyClone would refuse it: files on the default branch that
// no run of this worker left. A clone another process holds (a run, the
// index pass) is not looked at: the last answer stands. A clone a branch
// review's checks left is the next run's to clean up, not a problem.
func (w *Worker) cloneDirt(ctx context.Context, ws string) string {
	repo := w.repo(ws)
	if repo == nil || repo.Checkout != checkoutBranch || !branchCheckoutSupported {
		return ""
	}
	last, seen := w.dirt[ws]
	if seen && time.Since(last.at) < cloneDirtEvery {
		return last.reason
	}
	release, ok, err := tryLockClone(ctx, repo.Path)
	if err != nil || !ok {
		return last.reason
	}
	defer release()
	reason := ""
	if !checkMarked(ctx, repo.Path) {
		current, _ := git(ctx, repo.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
		dirty, err := git(ctx, repo.Path, "status", "--porcelain")
		if err != nil {
			if ctx.Err() != nil {
				return last.reason
			}
			reason = fmt.Sprintf("reading the clone %s: %v", repo.Path, err)
		} else {
			reason = uncleanClone(repo, current, dirty)
		}
	}
	switch {
	case reason != "" && last.reason == "":
		w.log.Warn("the clone is not clean: not taking this repo's work until it is", "workspace", ws, "reason", reason)
	case reason == "" && last.reason != "":
		w.log.Info("the clone is clean again", "workspace", ws)
	}
	if w.dirt == nil {
		w.dirt = map[string]preflightResult{}
	}
	w.dirt[ws] = preflightResult{at: time.Now(), reason: reason}
	return reason
}
