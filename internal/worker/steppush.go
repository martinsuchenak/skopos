package worker

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// stepPushEvery: how often the step pusher looks for new commits.
var stepPushEvery = 30 * time.Second

// pushSteps pushes the branch whenever its head moves while the
// implementer works (its step commits, the fix rounds' commits), through
// the one push path: validated name and prefix, protected patterns, the
// env-only token, never force. An implement round cut off by a restart, a
// kill or a lost lease then leaves its work on origin, and the next round —
// here or on another worker — continues from there instead of redoing it
// (tmp/self-healing-workers — design.md §3). A failed step push is only
// logged: the final push decides the run. The returned stop waits for a
// push in flight and is safe to call more than once.
func (w *Worker) pushSteps(ctx context.Context, repo *Repo, worktree, branch string, log *slog.Logger) (stop func()) {
	token := os.Getenv(repo.Push.TokenEnv)
	if !repo.PushSteps() || token == "" {
		return func() {}
	}
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pushed := headSHA(pctx, worktree) // already where the round starts
		failedAt := ""
		t := time.NewTicker(stepPushEvery)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
			}
			head := headSHA(pctx, worktree)
			if head == "" || head == pushed || head == failedAt {
				continue
			}
			if err := pushBranch(pctx, worktree, repo.Push.RemoteURL, token, repo.Push.Kind, branch, repo.BranchPrefix, repo.Protected); err != nil {
				if pctx.Err() == nil {
					log.Warn("step push failed; the final push will try again", "head", shortSHA(head), "err", err)
				}
				failedAt = head // once per head, not every tick
				continue
			}
			log.Info("pushed step commits", "branch", branch, "head", shortSHA(head))
			pushed = head
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
