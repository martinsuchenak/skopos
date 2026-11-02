package worker

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestCheckoutValidation(t *testing.T) {
	checkout := func(c string) Config {
		return Config{Repos: map[string]*Repo{"ws": {Path: "/x", Checkout: c}}}
	}
	checkConfigCases(t, []configCase{
		{"default", checkout(""), ""},
		{"worktree", checkout("worktree"), ""},
		{"branch", checkout("branch"), ""},
		{"unknown", checkout("clone"), `checkout "clone" is not one of worktree, branch`},
	})
	cfg := checkout("")
	if err := cfg.Default(t.TempDir()); err != nil || cfg.Repos["ws"].Checkout != checkoutWorktree {
		t.Fatalf("default checkout %q, err %v", cfg.Repos["ws"].Checkout, err)
	}
}

// The branch checkout end to end: the implementation runs in the clone on
// the item's branch (set up there), and the clone ends on the default
// branch with the work on the branch.
func TestBranchCheckoutImplement(t *testing.T) {
	t.Run("implements in the clone and puts it back", func(t *testing.T) {
		mark := filepath.Join(t.TempDir(), "setup")
		g := newGateStack(t, "fix", nil, "{ pwd; git rev-parse --abbrev-ref HEAD; } >> "+mark)
		repo := g.w.cfg.Repos["ws"]
		repo.Checkout = checkoutBranch
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		rep := g.lastReport(t)
		branch, _ := rep["branch"].(string)
		if rep["status"] != "succeeded" || branch == "" {
			t.Fatalf("report: %v", rep)
		}
		if got := gitOut(t, repo.Path, "symbolic-ref", "--short", "HEAD"); got != "main" {
			t.Fatalf("the clone must end on main, is on %s", got)
		}
		if got := gitOut(t, repo.Path, "status", "--porcelain"); got != "" {
			t.Fatalf("the clone must end clean: %s", got)
		}
		if got := gitOut(t, repo.Path, "show", branch+":state"); got != "good" {
			t.Fatalf("the branch must hold the fixed state, has %q", got)
		}
		if got := gitOut(t, repo.Path, "rev-list", "--count", g.run.BaseSHA+".."+branch); got != "2" {
			t.Fatalf("the branch starts at the approved base: %s commits past it", got)
		}
		b, _ := os.ReadFile(mark)
		clone, _ := filepath.EvalSymlinks(repo.Path)
		if lines := strings.Fields(string(b)); len(lines) != 2 || !strings.HasSuffix(lines[0], filepath.Base(clone)) || lines[1] != branch {
			t.Fatalf("setup must run in the clone on the item's branch: %q", b)
		}
		if dirExists(filepath.Join(g.w.cfg.StateDir, "worktrees")) {
			t.Fatal("a branch checkout cuts no worktree")
		}
		if rec, _ := g.w.loadBranch(g.run.ItemID); rec.Worktree != "" || rec.Branch != branch {
			t.Fatalf("record: %+v", rec)
		}
	})
	for _, other := range []bool{false, true} {
		name := "a pause stashes the uncommitted work; the resume skips setup"
		if other {
			name = "a resume after another branch's setup sets the clone up again"
		}
		t.Run(name, func(t *testing.T) {
			mark := filepath.Join(t.TempDir(), "setup-runs")
			g := newGateStack(t, "quota", nil, "echo ran >> "+mark)
			repo := g.w.cfg.Repos["ws"]
			repo.Checkout = checkoutBranch
			err := g.w.stageImplement(t.Context(), g.run, g.w.log)
			var qe *quotaError
			if !errors.As(err, &qe) {
				t.Fatalf("the first fix round must pause on quota, got %v", err)
			}
			if got := gitOut(t, repo.Path, "symbolic-ref", "--short", "HEAD"); got != "main" {
				t.Fatalf("a paused run must leave the clone on main, is on %s", got)
			}
			rec, _ := g.w.loadBranch(g.run.ItemID)
			if fileExists(filepath.Join(repo.Path, "wip.txt")) || len(stashRefs(t.Context(), repo.Path, stashLabel(rec.Branch))) != 1 {
				t.Fatal("the uncommitted work must be stashed under the branch")
			}
			if other {
				writeCloneSetup(t.Context(), repo.Path, cloneSetup{Branch: "agent/other", Head: "x"})
			}
			if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
				t.Fatalf("resume: %v (calls %s)", err, g.callLog(t))
			}
			rep := g.lastReport(t)
			if rep["status"] != "succeeded" {
				t.Fatalf("report: %v", rep)
			}
			if got := gitOut(t, repo.Path, "show", rec.Branch+":wip.txt"); got != "wip" {
				t.Fatalf("the resumed run must get the stashed work back, wip.txt = %q", got)
			}
			if refs := stashRefs(t.Context(), repo.Path, stashLabel(rec.Branch)); len(refs) != 0 {
				t.Fatalf("an applied stash is dropped: %v", refs)
			}
			want := map[bool]int{false: 1, true: 2}[other]
			if b, _ := os.ReadFile(mark); strings.Count(string(b), "ran") != want {
				t.Fatalf("setup ran %d times, want %d", strings.Count(string(b), "ran"), want)
			}
		})
	}
	for _, dirty := range []bool{false, true} {
		t.Run(map[bool]string{false: "a worktree from before the switch goes", true: "a worktree from before the switch with work in it stops the run"}[dirty], func(t *testing.T) {
			g := newGateStack(t, "fix", nil)
			repo := g.w.cfg.Repos["ws"]
			branch := normalizeBranch(g.run.ItemID, "Add state", repo.BranchPrefix)
			wt := filepath.Join(t.TempDir(), "old-worktree")
			if err := prepareImplementWorktree(t.Context(), repo.Path, wt, branch, g.run.BaseSHA); err != nil {
				t.Fatal(err)
			}
			g.w.saveBranch(branchRecord{ItemID: g.run.ItemID, WorkspaceID: "ws", Branch: branch, Worktree: wt})
			if dirty {
				if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("mine"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			repo.Checkout = checkoutBranch
			err := g.w.stageImplement(t.Context(), g.run, g.w.log)
			if dirty {
				if err == nil || !strings.Contains(err.Error(), "has uncommitted changes") || !fileExists(filepath.Join(wt, "notes.txt")) {
					t.Fatalf("want the run stopped and the work kept, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if rep := g.lastReport(t); rep["status"] != "succeeded" || dirExists(wt) {
				t.Fatalf("report %v, old worktree still there: %v", rep, dirExists(wt))
			}
			if rec, _ := g.w.loadBranch(g.run.ItemID); rec.Worktree != "" {
				t.Fatalf("record: %+v", rec)
			}
		})
	}
}

func branchCloneWorker(t *testing.T, client *Client) (*Worker, *Repo) {
	t.Helper()
	dir, _ := gitRepo(t)
	repo := &Repo{Path: dir, DefaultBranch: "main", Checkout: checkoutBranch}
	return New(&Config{StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": repo}}, client, slog.New(slog.DiscardHandler)), repo
}

// Every stage holds a branch-checkout clone for its attempt and finds it on
// the default branch: a killed run's branch is parked, its work stashed
// under the branch for the item's next run. Files on the default branch
// are someone else's: the run stops instead of hiding them.
func TestRunStageHoldsTheClone(t *testing.T) {
	t.Run("a killed run's branch is put back", func(t *testing.T) {
		w, repo := branchCloneWorker(t, nil)
		dir := repo.Path
		gitOut(t, dir, "checkout", "-q", "-b", "agent/x")
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("half done"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := w.runStage(t.Context(), &Run{ID: "r", WorkspaceID: "ws", Stage: "bogus"}, w.log)
		if err == nil || !strings.Contains(err.Error(), "unknown stage") {
			t.Fatalf("err %v", err)
		}
		if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
			t.Fatalf("on %s, want main", got)
		}
		if got := gitOut(t, dir, "status", "--porcelain"); got != "" {
			t.Fatalf("the clone must be clean: %s", got)
		}
		release, ok, err := tryLockClone(t.Context(), dir)
		if !ok || err != nil {
			t.Fatalf("the lock must be free after the attempt (ok %v, err %v)", ok, err)
		}
		// Held: another holder (another worker process) can't take it.
		if _, ok, err := tryLockClone(t.Context(), dir); ok || err != nil {
			t.Fatalf("the lock must be taken while held (ok %v, err %v)", ok, err)
		}
		release()
		if err := checkoutImplementBranch(t.Context(), dir, "agent/x", "main"); err != nil {
			t.Fatal(err)
		}
		if err := applyStash(t.Context(), dir, stashLabel("agent/x")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(b) != "half done" || !fileExists(filepath.Join(dir, "new.txt")) {
			t.Fatalf("the item's next run must get its work back: AGENTS.md %q", b)
		}
	})
	t.Run("files on the default branch stop the run", func(t *testing.T) {
		w, repo := branchCloneWorker(t, nil)
		if err := os.WriteFile(filepath.Join(repo.Path, "dump.sql"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := w.runStage(t.Context(), &Run{ID: "r", WorkspaceID: "ws", Stage: "bogus"}, w.log)
		if err == nil || !strings.Contains(err.Error(), "needs it clean") || !fileExists(filepath.Join(repo.Path, "dump.sql")) {
			t.Fatalf("want the run stopped and the file kept, got %v", err)
		}
		if refs := stashRefs(t.Context(), repo.Path, "skopos:main"); len(refs) != 0 {
			t.Fatalf("nothing is stashed: %v", refs)
		}
	})
}

// The sweep leaves a busy branch-checkout clone for the next pass, then
// drops the finished item's branch and stash.
func TestSweepBranchCheckout(t *testing.T) {
	item := "01a19700-0000-7000-8000-00000000beef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"`+item+`","status":"done"}`)
	}))
	t.Cleanup(srv.Close)
	w, repo := branchCloneWorker(t, NewClient(srv.URL, "k"))
	dir := repo.Path
	gitOut(t, dir, "checkout", "-q", "-b", "agent/x")
	if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := parkClone(t.Context(), dir, "main", stashLabel("agent/x")); err != nil {
		t.Fatal(err)
	}
	w.saveBranch(branchRecord{ItemID: item, WorkspaceID: "ws", Branch: "agent/x", Pushed: true})

	release, ok, err := tryLockClone(t.Context(), dir)
	if !ok || err != nil {
		t.Fatalf("lock: %v %v", ok, err)
	}
	w.sweepBranches(t.Context(), time.Now())
	if _, ok := w.loadBranch(item); !ok {
		t.Fatal("a busy clone is left for the next sweep")
	}
	release()
	w.sweepBranches(t.Context(), time.Now())
	if _, ok := w.loadBranch(item); ok {
		t.Fatal("the record must go once cleaned up")
	}
	if _, err := git(t.Context(), dir, "rev-parse", "--verify", "--quiet", "refs/heads/agent/x"); err == nil {
		t.Fatal("the pushed branch must be deleted")
	}
	if refs := stashRefs(t.Context(), dir, stashLabel("agent/x")); len(refs) != 0 {
		t.Fatalf("the finished item's stash must be dropped: %v", refs)
	}
}
