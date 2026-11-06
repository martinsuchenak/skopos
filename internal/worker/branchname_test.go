package worker

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchNames(t *testing.T) {
	item := &Item{ID: "01a1ae4e-1b41-78a5-9fbb-73e2bd7e5358", Title: "Some feature!", Ref: "WR203345"}
	cases := []struct {
		name   string
		repo   Repo
		branch string
		want   string
		err    string
	}{
		{"default template", Repo{BranchPrefix: "agent/"}, "", "agent/9fbb-73e2bd7e5358-some-feature", ""},
		{"ref template", Repo{BranchPrefix: "WR", BranchTemplate: "{ref}-{slug}"}, "", "WR203345-some-feature", ""},
		{"own branch wins", Repo{BranchPrefix: "WR", BranchTemplate: "{ref}-{slug}"}, "WR203345-api", "WR203345-api", ""},
		{"no prefix", Repo{BranchPrefix: "agent/"}, "feature/x", "", "does not start with"},
		{"protected", Repo{BranchPrefix: "", Protected: []string{"PROD"}}, "PROD", "", "protected"},
		{"unsafe", Repo{BranchPrefix: ""}, "a..b", "", "not a usable branch name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := *item
			it.Branch = c.branch
			got, err := branchFor(&it, &c.repo)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("got %q, %v; want error %q", got, err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
	// The default template is the name items always had.
	if got, _ := branchFor(item, &Repo{BranchPrefix: "agent/"}); got != normalizeBranch(item.ID, item.Title, "agent/") {
		t.Fatalf("default template drifted from normalizeBranch: %q", got)
	}
	noRef := *item
	noRef.Ref = ""
	if _, err := branchFor(&noRef, &Repo{BranchPrefix: "WR", BranchTemplate: "{ref}-{slug}"}); err == nil || !strings.Contains(err.Error(), "reference") {
		t.Fatalf("template with {ref} and no ref: %v", err)
	}
	if got := branchStem(item, &Repo{BranchPrefix: "WR", BranchTemplate: "{ref}-{slug}"}); got != "WR203345-" {
		t.Fatalf("stem: %q", got)
	}
	for _, bad := range []string{"{prefix}{slug}", "{ref}-{title}"} {
		if err := validateBranchTemplate(bad); err == nil {
			t.Errorf("template %q accepted", bad)
		}
	}
	for _, ok := range []string{"{ref}-{slug}", "{prefix}{tail}-{slug}", "{prefix}{ref}"} {
		if err := validateBranchTemplate(ok); err != nil {
			t.Errorf("template %q: %v", ok, err)
		}
	}
	for _, ok := range []string{"WR203345-x", "agent/abc-def", "feature/a.b_c"} {
		if !validBranch(ok) {
			t.Errorf("validBranch(%q) = false", ok)
		}
	}
	for _, bad := range []string{"-x", "a..b", "a//b", "a/", "x.lock", "a/.b", "a b", "a~1"} {
		if validBranch(bad) {
			t.Errorf("validBranch(%q) = true", bad)
		}
	}
}

// originClone is a bare origin with main, and a clone of it.
type originClone struct {
	t                *testing.T
	origin, clone, w string // w: a second clone that pushes as "someone else"
}

func newOriginClone(t *testing.T) *originClone {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	o := &originClone{t: t, origin: filepath.Join(root, "origin.git"), clone: filepath.Join(root, "clone"), w: filepath.Join(root, "dev")}
	o.run(root, "init", "-q", "--bare", "-b", "main", o.origin)
	o.run(root, "clone", "-q", o.origin, o.w)
	o.commit("README.md", "hello", "base")
	o.run(o.w, "push", "-q", "origin", "main")
	o.run(root, "clone", "-q", o.origin, o.clone)
	return o
}

func (o *originClone) run(dir string, args ...string) string {
	o.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		o.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file in the developer's clone and commits it.
func (o *originClone) commit(file, content, msg string) string {
	o.t.Helper()
	if err := os.WriteFile(filepath.Join(o.w, file), []byte(content), 0o644); err != nil {
		o.t.Fatal(err)
	}
	o.run(o.w, "add", "-A")
	o.run(o.w, "commit", "-q", "-m", msg)
	return o.run(o.w, "rev-parse", "HEAD")
}

// devBranch pushes a branch with one commit from the developer's clone.
func (o *originClone) devBranch(name string) string {
	o.t.Helper()
	o.run(o.w, "checkout", "-q", "-b", name, "main")
	head := o.commit(name+".txt", "work on "+name, "start "+name)
	o.run(o.w, "push", "-q", "origin", name)
	o.run(o.w, "checkout", "-q", "main")
	return head
}

// Discovery: an exact match is taken over; other branches of the same
// reference without an exact match are a question; none is a new branch.
func TestResolveBranch(t *testing.T) {
	o := newOriginClone(t)
	repo := &Repo{Path: o.clone, DefaultBranch: "main", BranchPrefix: "WR", BranchTemplate: "{ref}-{slug}"}
	item := &Item{ID: "01a1-0000-7000-8000-000000000001", Title: "Some feature", Ref: "WR1"}
	ctx := t.Context()

	got, err := resolveBranch(ctx, repo, item)
	if err != nil || got.Name != "WR1-some-feature" || got.Exists || got.Question != "" {
		t.Fatalf("no branches: %+v %v", got, err)
	}
	o.devBranch("WR1-api")
	o.devBranch("WR1-ui")
	o.devBranch("WR10-other") // another reference with a longer number
	got, err = resolveBranch(ctx, repo, item)
	if err != nil || got.Exists || !strings.Contains(got.Question, "`WR1-api`, `WR1-ui`") || strings.Contains(got.Question, "WR10") {
		t.Fatalf("several candidates: %+v %v", got, err)
	}
	o.devBranch("WR1-some-feature")
	got, err = resolveBranch(ctx, repo, item)
	if err != nil || !got.Exists || got.Name != "WR1-some-feature" || got.Question != "" {
		t.Fatalf("exact match: %+v %v", got, err)
	}
	// Naming the branch skips the question.
	named := *item
	named.Branch = "WR1-ui"
	got, err = resolveBranch(ctx, repo, &named)
	if err != nil || !got.Exists || got.Name != "WR1-ui" {
		t.Fatalf("named branch: %+v %v", got, err)
	}
}

// A handover continues the developer's branch; the implement start fast-
// forwards a stale local copy to origin's, and refuses a branch rewritten
// since the plan was approved.
func TestImplementStartOnExistingBranch(t *testing.T) {
	o := newOriginClone(t)
	ctx := t.Context()
	head := o.devBranch("WR2-x")

	// Planned against head: implement starts from origin's branch.
	local, ref, merge, err := implementStart(ctx, o.clone, "WR2-x", head)
	if err != nil || local || ref != head || merge != "" {
		t.Fatalf("from origin: local=%v ref=%q %v", local, ref, err)
	}
	// A local copy left by an earlier round, behind origin's: fast-forwarded.
	o.run(o.clone, "branch", "WR2-x", head)
	o.run(o.w, "checkout", "-q", "WR2-x")
	newer := o.commit("more.txt", "more", "more work")
	o.run(o.w, "push", "-q", "origin", "WR2-x")
	local, ref, merge, err = implementStart(ctx, o.clone, "WR2-x", head)
	if err != nil || !local || ref != "WR2-x" || merge != "" {
		t.Fatalf("stale local: local=%v ref=%q %v", local, ref, err)
	}
	if got := gitOut(t, o.clone, "rev-parse", "refs/heads/WR2-x"); got != newer {
		t.Fatalf("local branch not fast-forwarded: %s want %s", got, newer)
	}
	// Rewritten: the approved base is no longer in the branch's history.
	o.run(o.w, "reset", "-q", "--hard", "main")
	o.commit("other.txt", "other", "rewritten")
	o.run(o.w, "push", "-q", "--force", "origin", "WR2-x")
	o.run(o.clone, "branch", "-D", "WR2-x")
	if _, _, _, err := implementStart(ctx, o.clone, "WR2-x", head); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Fatalf("rewritten branch: %v", err)
	}
}

// A local branch and origin's that both moved on (an interrupted round left
// commits on this worker while another worker pushed its own) are pulled:
// merged when they merge, the run refused when they conflict — in both
// checkout modes.
func TestImplementStartPullsADivergedBranch(t *testing.T) {
	for _, mode := range []string{"worktree", "branch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			setup := func(t *testing.T, localFile string) (*originClone, string, string) {
				o := newOriginClone(t)
				base := o.run(o.w, "rev-parse", "HEAD")
				o.run(o.w, "checkout", "-q", "-b", "agent/x", "main")
				pushed := o.commit("shared.txt", "from the other worker\n", "other worker's round")
				o.run(o.w, "push", "-q", "origin", "agent/x")
				o.run(o.w, "checkout", "-q", "main")
				// This worker's leftover round, from the same base.
				o.run(o.clone, "checkout", "-q", "-b", "agent/x", base)
				if err := os.WriteFile(filepath.Join(o.clone, localFile), []byte("from this worker\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				o.run(o.clone, "add", "-A")
				o.run(o.clone, "commit", "-q", "-m", "leftover round")
				leftover := o.run(o.clone, "rev-parse", "HEAD")
				o.run(o.clone, "checkout", "-q", "main")
				return o, base, pushed + " " + leftover
			}
			start := func(o *originClone, base string) (string, error) {
				if mode == "branch" {
					return o.clone, checkoutImplementBranch(ctx, o.clone, "agent/x", base)
				}
				wt := filepath.Join(t.TempDir(), "wt")
				return wt, prepareImplementWorktree(ctx, o.clone, wt, "agent/x", base)
			}

			o, base, both := setup(t, "local.txt")
			dir, err := start(o, base)
			if err != nil {
				t.Fatalf("a clean merge must continue: %v", err)
			}
			for _, sha := range strings.Fields(both) {
				if !isAncestor(ctx, dir, sha, "HEAD") {
					t.Fatalf("the branch must hold both rounds; %s is missing", sha)
				}
			}
			if got := gitOut(t, dir, "status", "--porcelain"); got != "" {
				t.Fatalf("the merge must leave a clean tree: %s", got)
			}

			o, base, _ = setup(t, "shared.txt")
			before := o.run(o.clone, "rev-parse", "refs/heads/agent/x")
			_, err = start(o, base)
			if err == nil || !strings.Contains(err.Error(), "don't merge cleanly") || !strings.Contains(err.Error(), "shared.txt") {
				t.Fatalf("a conflict must refuse the run naming the files: %v", err)
			}
			if got := o.run(o.clone, "rev-parse", "refs/heads/agent/x"); got != before {
				t.Fatalf("a refused merge must leave the local branch as it was: %s want %s", got, before)
			}
			if got := gitOut(t, o.clone, "status", "--porcelain"); got != "" {
				t.Fatalf("a refused merge must leave the clone clean: %s", got)
			}
		})
	}
}

// The handover context: commits since the fork point and their diff; the
// knowledge base is the fork point, never the branch.
func TestHandoverContext(t *testing.T) {
	o := newOriginClone(t)
	ctx := t.Context()
	head := o.devBranch("WR3-x")
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	if _, err := fetchBranch(ctx, o.clone, "WR3-x"); err != nil {
		t.Fatal(err)
	}
	main := gitOut(t, o.clone, "rev-parse", "origin/main")
	h, err := handoverContext(ctx, repo, "WR3-x", head)
	if err != nil {
		t.Fatal(err)
	}
	if h.ForkPoint != main || !strings.Contains(h.Commits, "start WR3-x") || !strings.Contains(h.Diff, "+work on WR3-x") || h.Truncated {
		t.Fatalf("handover: %+v", h)
	}
	if knowledgeBase(ctx, repo, head) != main || knowledgeBase(ctx, repo, main) != main {
		t.Fatal("knowledge base must be the fork point")
	}
	if onDefaultBranch(ctx, repo, head) || !onDefaultBranch(ctx, repo, main) {
		t.Fatal("onDefaultBranch")
	}
}

// Checks on a branch under review leave the branch-checkout clone as they
// found it: on its default branch, clean, nothing stashed; setup is skipped
// when the clone is already set up for that branch at that head.
func TestCheckHeadLeavesTheCloneClean(t *testing.T) {
	o := newOriginClone(t)
	ctx := t.Context()
	head := o.devBranch("WR4-x")
	if _, err := fetchBranch(ctx, o.clone, "WR4-x"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "setup-ran")
	repo := &Repo{
		Path: o.clone, DefaultBranch: "main", Checkout: checkoutBranch,
		Setup:  []string{"sh -c 'echo x >> " + marker + "'"},
		Checks: []string{"sh -c 'echo junk > untracked.txt; echo changed > README.md; test -f WR4-x.txt'"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))
	defer srv.Close()
	w := New(&Config{StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": repo}}, NewClient(srv.URL, "k"), slog.New(slog.DiscardHandler))
	run := &Run{ID: "01a1-0000-7000-8000-0000000000aa", ItemID: "item", WorkspaceID: "ws"}
	for round := 1; round <= 2; round++ {
		checks, setup, err := w.checkHead(ctx, run, repo, "WR4-x", head, w.log)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// The check passed; the worker's own "committed" check reports the
		// file it rewrote (a formatter's change the branch is missing).
		if len(checks) != 2 || !checks[0].Passed || checks[1].Name != "committed" || checks[1].Passed {
			t.Fatalf("round %d checks: %+v", round, checks)
		}
		if round == 1 && len(setup) != 1 || round == 2 && len(setup) != 0 {
			t.Fatalf("round %d setup: %+v", round, setup)
		}
		if b := gitOut(t, o.clone, "symbolic-ref", "--short", "HEAD"); b != "main" {
			t.Fatalf("round %d: clone on %q", round, b)
		}
		if st, _ := git(ctx, o.clone, "status", "--porcelain"); st != "" {
			t.Fatalf("round %d: clone dirty: %q", round, st)
		}
		if st, _ := git(ctx, o.clone, "stash", "list"); st != "" {
			t.Fatalf("round %d: stashed: %q", round, st)
		}
	}
	if b, _ := os.ReadFile(marker); strings.Count(string(b), "x") != 1 {
		t.Fatalf("setup ran %d times, want once", strings.Count(string(b), "x"))
	}
}

// The prompts: a handover plan gets the work on the branch and the review
// of it; a branch review gets the item as its spec; a handed-over branch's
// review says whose commits are whose.
func TestHandoverPrompts(t *testing.T) {
	plan, err := renderPrompt("plan.md", map[string]any{
		"Workspace": "ws", "Base": "PROD", "Title": "t", "Content": "BRIEF",
		"Handover":   &handover{Branch: "WR1-x", ForkPoint: "f00d", Commits: "abc start", Diffstat: "1 file", Diff: "+DIFFLINE", Truncated: true},
		"Changes":    "CHANGE-REQUEST",
		"LastReview": "## review — claude-1\n\nREVIEW-VERDICT",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"on the existing branch `WR1-x`", "# Work already on the branch (handover)", "abc start", "+DIFFLINE", "cut short", "plan only the rest", "REVIEW-VERDICT", "CHANGE-REQUEST"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan prompt missing %q", want)
		}
	}
	if fresh, _ := renderPrompt("plan.md", map[string]any{"Workspace": "ws", "Base": "PROD", "Title": "t", "Content": "BRIEF"}); strings.Contains(fresh, "handover") || !strings.Contains(fresh, "in a fresh git branch") {
		t.Errorf("an ordinary plan prompt changed")
	}
	review, err := renderPrompt("review.md", map[string]any{"Base": "PROD", "Stage": "branch_review", "BranchReview": true, "Brief": "WHAT-IT-IS-FOR"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review, "Someone else wrote this branch") || !strings.Contains(review, "WHAT-IT-IS-FOR") || strings.Contains(review, "implement this plan") {
		t.Errorf("branch review prompt: %s", review)
	}
	handed, _ := renderPrompt("review.md", map[string]any{"Base": "PROD", "Stage": "review", "HandoverBase": "beef1234", "Steps": []Step{{Title: "s"}}})
	if !strings.Contains(handed, "commits up to `beef1234` were already on it") || !strings.Contains(handed, "implement this plan") {
		t.Errorf("handover review prompt: %s", handed)
	}
}

// A worker whose origin/<default> is older than the plan's base (it didn't
// plan the item) still finds the base on the default branch: knowledge is
// read at the base itself, not at an older merge-base.
func TestKnowledgeBaseWithStaleDefault(t *testing.T) {
	o := newOriginClone(t)
	ctx := t.Context()
	newer := o.commit("AGENTS.md", "new instructions", "main moves on")
	o.run(o.w, "push", "-q", "origin", "main")
	head := o.devBranch("WR5-x") // built on newer
	if _, err := fetchBranch(ctx, o.clone, "WR5-x"); err != nil {
		t.Fatal(err)
	}
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	if isAncestor(ctx, o.clone, newer, "origin/main") {
		t.Fatal("test setup: the clone's origin/main should be stale")
	}
	if got := knowledgeBase(ctx, repo, newer); got != newer {
		t.Fatalf("knowledge base %s, want the base %s", got, newer)
	}
	if got := knowledgeBase(ctx, repo, head); got != newer {
		t.Fatalf("handover knowledge base %s, want the fork point %s", got, newer)
	}
}

// A worker killed during a branch review's checks leaves the clone detached
// with the checks' files; the next run's readyClone discards them instead of
// refusing the clone.
func TestReadyCloneAfterKilledChecks(t *testing.T) {
	o := newOriginClone(t)
	ctx := t.Context()
	head := o.devBranch("WR6-x")
	if _, err := fetchBranch(ctx, o.clone, "WR6-x"); err != nil {
		t.Fatal(err)
	}
	repo := &Repo{Path: o.clone, DefaultBranch: "main", Checkout: checkoutBranch}
	w := New(&Config{StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": repo}}, nil, slog.New(slog.DiscardHandler))
	setCheckMarker(ctx, o.clone, true)
	o.run(o.clone, "checkout", "-q", "--detach", head)
	if err := os.WriteFile(filepath.Join(o.clone, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.clone, "README.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.readyClone(ctx, repo, w.log); err != nil {
		t.Fatalf("readyClone: %v", err)
	}
	if b := gitOut(t, o.clone, "symbolic-ref", "--short", "HEAD"); b != "main" {
		t.Fatalf("clone on %q", b)
	}
	if st, _ := git(ctx, o.clone, "status", "--porcelain"); st != "" || checkMarked(ctx, o.clone) {
		t.Fatalf("not cleaned: %q marker=%v", st, checkMarked(ctx, o.clone))
	}
	// Without the marker, someone's files on a detached HEAD still stop it.
	o.run(o.clone, "checkout", "-q", "--detach", head)
	if err := os.WriteFile(filepath.Join(o.clone, "mine.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.readyClone(ctx, repo, w.log); err == nil {
		t.Fatal("unmarked dirty clone must be refused")
	}
}

// Panel members share checks within their round only.
func TestGroupChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"stage":"branch_review","group_id":"g2","head_sha":"h","checks":[]},
			{"stage":"branch_review","group_id":"g1","head_sha":"h","checks":[{"name":"test","passed":false}]}
		]`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k")
	if got := c.GroupChecks(t.Context(), "item", "g2", "h"); got != nil {
		t.Fatalf("a new round must not reuse the last round's checks: %+v", got)
	}
	if got := c.GroupChecks(t.Context(), "item", "g1", "h"); len(got) != 1 || got[0].Name != "test" {
		t.Fatalf("same round: %+v", got)
	}
}

func TestDiffLines(t *testing.T) {
	o := newOriginClone(t)
	base := o.run(o.w, "rev-parse", "HEAD")
	head := o.commit("big.txt", strings.Repeat("line\n", 250), "big")
	if n := diffLines(t.Context(), o.w, base, head); n != 250 {
		t.Fatalf("diffLines = %d", n)
	}
}

// An item's existing worktree (a retry on the same worker) is brought up
// to origin's branch before the run continues: fast-forwarded when behind,
// merged when both moved on, left alone with uncommitted changes.
func TestPullWorktree(t *testing.T) {
	ctx := t.Context()
	o := newOriginClone(t)
	base := o.run(o.w, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := prepareImplementWorktree(ctx, o.clone, wt, "agent/x", base); err != nil {
		t.Fatal(err)
	}
	o.run(o.w, "checkout", "-q", "-b", "agent/x", "main")
	pushed := o.commit("other.txt", "another worker\n", "other round")
	o.run(o.w, "push", "-q", "origin", "agent/x")

	// Behind: fast-forwarded.
	if err := pullWorktree(ctx, o.clone, wt, "agent/x"); err != nil {
		t.Fatal(err)
	}
	if got := o.run(wt, "rev-parse", "HEAD"); got != pushed {
		t.Fatalf("behind origin: HEAD %s, want %s", got, pushed)
	}
	// Both moved on: merged.
	if err := os.WriteFile(filepath.Join(wt, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.run(wt, "add", "-A")
	o.run(wt, "commit", "-q", "-m", "this worker's step")
	mine := o.run(wt, "rev-parse", "HEAD")
	newer := o.commit("more.txt", "more\n", "more from the other worker")
	o.run(o.w, "push", "-q", "origin", "agent/x")
	if err := pullWorktree(ctx, o.clone, wt, "agent/x"); err != nil {
		t.Fatal(err)
	}
	if !isAncestor(ctx, wt, mine, "HEAD") || !isAncestor(ctx, wt, newer, "HEAD") {
		t.Fatal("diverged: the worktree must hold both")
	}
	// Uncommitted changes: untouched.
	head := o.run(wt, "rev-parse", "HEAD")
	o.commit("late.txt", "late\n", "late")
	o.run(o.w, "push", "-q", "origin", "agent/x")
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pullWorktree(ctx, o.clone, wt, "agent/x"); err != nil || o.run(wt, "rev-parse", "HEAD") != head {
		t.Fatalf("a dirty worktree must be left alone: %v", err)
	}
}
