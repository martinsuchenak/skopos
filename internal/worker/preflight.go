package worker

import (
	"context"
	"fmt"
	"time"
)

const (
	// defaultPreflightInterval: how often a repo's preflight runs unless
	// preflight_interval says otherwise.
	defaultPreflightInterval = 5 * time.Minute
	maxPreflightInterval     = 24 * time.Hour
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
// takes them, instead of this one failing them on a broken environment
// (tmp/self-healing-workers — design.md §2).
func (w *Worker) checkHealth(ctx context.Context) map[string]string {
	var out map[string]string
	now := time.Now()
	for _, ws := range w.cfg.Workspaces {
		repo := w.cfg.Repos[ws]
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
