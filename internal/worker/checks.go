package worker

import (
	"context"
	"fmt"
	"strings"
)

const (
	// defaultCheckFixRounds: a failing check goes back to the implementer
	// this often before the item blocks.
	defaultCheckFixRounds = 2
	maxCheckFixRounds     = 5
	// committedCheck names the check that the tested tree is the pushed
	// one: checks run on the worktree, the push sends commits only.
	committedCheck = "committed"
	// The phases a running implement run reports past its first turn (the
	// server's runs.PhaseChecks / PhaseFixing): the dashboard's Checks step
	// shows them running.
	phaseChecks = "checks"
	phaseFixing = "fixing"
)

// FixRounds is the repo's check_fix_rounds, defaulted.
func (r *Repo) FixRounds() int {
	if r.CheckFixRounds == nil {
		return defaultCheckFixRounds
	}
	return *r.CheckFixRounds
}

// repoChecks runs the repo's checks on the worktree. With checks configured,
// uncommitted changes to tracked files fail too, looked for AFTER the checks:
// whether the implementer left them or a check rewrote a file (go mod tidy,
// a formatter), what passed would differ from the pushed branch.
func (w *Worker) repoChecks(ctx context.Context, repo *Repo, worktree string) []Check {
	if len(repo.Checks) == 0 {
		return nil
	}
	out := runChecks(ctx, worktree, repo.Checks, w.cfg.TokenEnvs(), w.envFileKeys)
	if dirty, _ := git(ctx, worktree, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(dirty) != "" {
		out = append(out, Check{Name: committedCheck, Output: "Uncommitted changes to tracked files after the checks ran. Commit them if they belong to the change; " +
			"if a check rewrote them (go mod tidy, a formatter), commit its result:\n" + tail(dirty, 2000)})
	} else {
		out = append(out, Check{Name: committedCheck, Passed: true})
	}
	return out
}

func failedChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if !c.Passed {
			out = append(out, c)
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func checkNames(checks []Check) string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// fixChecks is the CI loop: run the checks; while some fail and rounds
// remain, hand the failures to fix and run them again. fix returns stop
// when its round ended the stage itself (blocked, amendment, a quota
// pause); the caller then returns err as the stage outcome.
func fixChecks(maxRounds int, run func() []Check, fix func(failed []Check, round int) (stop bool, err error)) (checks []Check, rounds int, stop bool, err error) {
	checks = run()
	for failed := failedChecks(checks); len(failed) > 0 && rounds < maxRounds; failed = failedChecks(checks) {
		rounds++
		if stop, err = fix(failed, rounds); stop {
			return checks, rounds, true, err
		}
		checks = run()
	}
	return checks, rounds, false, nil
}
