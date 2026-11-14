package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewPRClientDerivesAPI(t *testing.T) {
	cases := []struct {
		kind, remote, api, owner, repo string
	}{
		{"github", "https://github.com/globex/shop.git", "https://api.github.com", "globex", "shop"},
		{"github", "https://ghe.example.com/org/app", "https://ghe.example.com/api/v3", "org", "app"},
		{"gitea", "https://gitea.example.com/globex/shop.git", "https://gitea.example.com/api/v1", "globex", "shop"},
		{"gitea", "https://example.com/git/globex/shop.git", "https://example.com/git/api/v1", "globex", "shop"},
	}
	for _, tc := range cases {
		c, err := newPRClient(tc.kind, tc.remote, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.remote, err)
		}
		if c.apiBase != tc.api || c.owner != tc.owner || c.repo != tc.repo {
			t.Fatalf("%s: got %s %s/%s", tc.remote, c.apiBase, c.owner, c.repo)
		}
	}
	for _, bad := range []string{"git@github.com:o/r.git", "https://github.com/onlyowner", "ftp://x/o/r"} {
		if _, err := newPRClient("github", bad, ""); err == nil {
			t.Fatalf("%s should be refused", bad)
		}
	}
	if _, err := newPRClient("none", "https://github.com/o/r", ""); err == nil {
		t.Fatal("kind none has no pull requests")
	}
}

// fakeForge is a minimal pull-request API in both dialects: GitHub's
// filtered list and Gitea's base/head lookup, open, reopen and close. It
// records what it was sent.
type fakeForge struct {
	mu      sync.Mutex
	prs     []*fakePR
	auth    []string
	created []map[string]any
	patched []string // "<number>:<state>"
	queries []string
}

type fakePR struct {
	number            int
	head, base, state string
	merged            bool
}

func (p *fakePR) json() map[string]any {
	out := map[string]any{"number": p.number, "html_url": fmt.Sprintf("https://forge/o/r/pull/%d", p.number), "state": p.state,
		"merged": p.merged, "merged_at": nil, "head": map[string]any{"ref": p.head}, "base": map[string]any{"ref": p.base}}
	if p.merged {
		out["merged_at"] = "2026-11-01T00:00:00Z"
	}
	return out
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	_, rest, _ := strings.Cut(r.URL.Path, "/pulls")
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case r.Method == http.MethodGet && rest == "":
		f.queries = append(f.queries, r.URL.RawQuery)
		q := r.URL.Query()
		out := []map[string]any{}
		for k := len(f.prs) - 1; k >= 0; k-- {
			p := f.prs[k]
			if (q.Get("state") == "all" || q.Get("state") == p.state) && (q.Get("head") == "" || q.Get("head") == "o:"+p.head) {
				out = append(out, p.json())
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.Contains(rest, "/"):
		base, head, _ := strings.Cut(rest, "/")
		for k := len(f.prs) - 1; k >= 0; k-- {
			if f.prs[k].base == base && f.prs[k].head == head {
				_ = json.NewEncoder(w).Encode(f.prs[k].json())
				return
			}
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && rest == "":
		var in map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		f.created = append(f.created, in)
		p := &fakePR{number: 40 + len(f.prs) + 1, head: in["head"].(string), base: in["base"].(string), state: "open"}
		f.prs = append(f.prs, p)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p.json())
	case r.Method == http.MethodPatch && rest != "":
		var in map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		f.patched = append(f.patched, rest+":"+in["state"])
		for _, p := range f.prs {
			if fmt.Sprint(p.number) == rest {
				p.state = in["state"]
				_ = json.NewEncoder(w).Encode(p.json())
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// The first round opens the pull request and a change-request round reuses
// it. A PR closed without merging (by hand, or by the cleanup after a Reject
// that was undone) is reopened; after a merge, a new one is opened.
func TestEnsurePullRequest(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"gitea", "github"} {
		t.Run(kind, func(t *testing.T) {
			forge := &fakeForge{}
			srv := httptest.NewServer(forge)
			defer srv.Close()
			c, err := newPRClient(kind, srv.URL+"/o/r.git", "tok")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "github" {
				c.apiBase = srv.URL // not github.com: point the API at the fake
			}
			ensure := func(want string) *PullRequest {
				t.Helper()
				pr, action, err := c.Ensure(ctx, "agent/x", "main", "Fix textutil", "body", true, func(*PullRequest) bool { return true })
				if err != nil || action != want {
					t.Fatalf("want %s, got %s (%+v, %v)", want, action, pr, err)
				}
				return pr
			}
			first := ensure("opened")
			if again := ensure("reused"); again.Number != first.Number {
				t.Fatalf("reused #%d, want #%d", again.Number, first.Number)
			}
			if err := c.Close(ctx, first.Number); err != nil {
				t.Fatal(err)
			}
			if re := ensure("reopened"); re.Number != first.Number || re.State != "open" {
				t.Fatalf("reopened %+v, want #%d open", re, first.Number)
			}
			forge.prs[0].state, forge.prs[0].merged = "closed", true
			if next := ensure("opened"); next.Number == first.Number {
				t.Fatal("a merged PR is never reopened")
			}
			if len(forge.created) != 2 {
				t.Fatalf("opened %d pull requests, want 2", len(forge.created))
			}
			in := forge.created[0]
			if kind == "github" {
				if in["draft"] != true || in["title"] != "Fix textutil" || forge.auth[0] != "Bearer tok" {
					t.Fatalf("github create: %v auth %q", in, forge.auth[0])
				}
				if !strings.Contains(forge.queries[0], "head=o%3Aagent%2Fx") || !strings.Contains(forge.queries[0], "state=all") {
					t.Fatalf("github lookup must filter by head, any state: %q", forge.queries[0])
				}
			} else if in["title"] != "WIP: Fix textutil" || in["draft"] != nil || forge.auth[0] != "token tok" {
				t.Fatalf("gitea create: %v auth %q", in, forge.auth[0])
			}
		})
	}
}

// The sweep:
//   - done: removes the worktree, and the local branch only if it was pushed;
//     the pull request stays for the approver to merge;
//   - discarded: nothing for a day (Reject can be undone), then closes the
//     pull request and cleans up;
//   - 404 (deleted, or out of the key's scope): nothing for a week, then
//     removes only the worktree, never the branch or the pull request;
//   - live: untouched, and earlier marks are forgotten.
func TestSweepBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	forge := &fakeForge{}
	const (
		rejected = "01a19000-0000-7000-8000-00000000000a"
		approved = "01a19000-0000-7000-8000-00000000000b"
		live     = "01a19000-0000-7000-8000-00000000000c"
		local    = "01a19000-0000-7000-8000-00000000000d"
		gone     = "01a19000-0000-7000-8000-00000000000e"
	)
	var mu sync.Mutex
	status := map[string]string{rejected: "discarded", approved: "done", live: "in_review", local: "done"}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", forge)
	mux.HandleFunc("/api/inbox/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/api/inbox/")
		st, ok := status[id]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": st})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	clone := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", clone}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Setenv("SWEEP_TOKEN", "tok")
	repo := &Repo{Path: clone, DefaultBranch: "main"}
	repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv, repo.Push.PullRequest = "gitea", srv.URL+"/o/r.git", "SWEEP_TOKEN", "draft"
	state := t.TempDir()
	w := &Worker{cfg: &Config{StateDir: state, Repos: map[string]*Repo{"ws": repo}}, client: NewClient(srv.URL, "k"), log: slog.Default()}

	recs := map[string]branchRecord{}
	for id, pr := range map[string]int{rejected: 5, approved: 6, live: 7, local: 0, gone: 8} {
		suffix := id[len(id)-1:]
		wt := filepath.Join(state, "worktrees", "r", "agent-"+suffix)
		if err := createWorktreeAt(ctx, clone, wt, "agent/"+suffix, "main"); err != nil {
			t.Fatal(err)
		}
		rec := branchRecord{ItemID: id, WorkspaceID: "ws", Branch: "agent/" + suffix, Worktree: wt, Pushed: pr > 0, PRNumber: pr}
		w.saveBranch(rec)
		recs[id] = rec
	}
	branchExists := func(b string) bool {
		return exec.Command("git", "-C", clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Run() == nil
	}
	present := func(id string) bool {
		_, statErr := os.Stat(recs[id].Worktree)
		_, kept := w.loadBranch(id)
		return statErr == nil && kept
	}

	now := time.Now()
	w.sweepBranches(ctx, now)
	if present(approved) || branchExists("agent/b") {
		t.Fatal("done: worktree, record and the pushed local branch go at once")
	}
	if present(local) || !branchExists("agent/d") {
		t.Fatal("done: an unpushed local branch is the only copy and stays")
	}
	for _, id := range []string{rejected, live, gone} {
		if !present(id) {
			t.Fatalf("%s: nothing happens on the first sight", id)
		}
	}
	if len(forge.patched) != 0 {
		t.Fatalf("no pull request is closed yet: %v", forge.patched)
	}

	// Restored within the day: the mark is forgotten.
	mu.Lock()
	status[rejected] = "open"
	mu.Unlock()
	w.sweepBranches(ctx, now.Add(time.Hour))
	if rec, _ := w.loadBranch(rejected); rec.DiscardedSeenMS != 0 {
		t.Fatal("a restored item's discard mark must be cleared")
	}
	mu.Lock()
	status[rejected] = "discarded"
	mu.Unlock()
	w.sweepBranches(ctx, now.Add(2*time.Hour))
	w.sweepBranches(ctx, now.Add(2*time.Hour+discardGrace))
	if len(forge.patched) != 1 || forge.patched[0] != "5:closed" {
		t.Fatalf("a day after the Reject, its pull request is closed: %v", forge.patched)
	}
	if present(rejected) || branchExists("agent/a") {
		t.Fatal("rejected: worktree, record and pushed branch go after the grace")
	}

	w.sweepBranches(ctx, now.Add(missingGrace+time.Minute))
	if _, statErr := os.Stat(recs[gone].Worktree); statErr == nil {
		t.Fatal("a week of 404s removes the worktree")
	}
	if !branchExists("agent/e") || len(forge.patched) != 1 {
		t.Fatal("a 404 never deletes the branch or closes the pull request")
	}
	if !present(live) || !branchExists("agent/c") {
		t.Fatal("a live item is left alone")
	}
}

// A re-implementation continues the item's branch: the local one when the
// cleanup kept it, else the one pushed to origin, else a new branch from
// the base. The push then stays fast-forward.
func TestPrepareImplementWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := t.TempDir()
	run(origin, "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	run(seed, "init", "-q", "-b", "main")
	run(seed, "commit", "-q", "--allow-empty", "-m", "base")
	base := run(seed, "rev-parse", "HEAD")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-q", "origin", "main")
	run(seed, "checkout", "-q", "-b", "agent/pushed")
	run(seed, "commit", "-q", "--allow-empty", "-m", "round 1")
	pushedHead := run(seed, "rev-parse", "HEAD")
	run(seed, "push", "-q", "origin", "agent/pushed")

	clone := t.TempDir()
	run(clone, "clone", "-q", origin, ".")
	state := t.TempDir()

	wt := filepath.Join(state, "pushed")
	if err := prepareImplementWorktree(ctx, clone, wt, "agent/pushed", base); err != nil {
		t.Fatal(err)
	}
	if got := run(wt, "rev-parse", "HEAD"); got != pushedHead {
		t.Fatalf("pushed branch: HEAD %s, want origin's %s", got, pushedHead)
	}
	// The cleanup removed the worktree but kept the local branch.
	run(wt, "commit", "-q", "--allow-empty", "-m", "round 2, local only")
	localHead := run(wt, "rev-parse", "HEAD")
	removeWorktree(ctx, clone, wt)
	if err := prepareImplementWorktree(ctx, clone, wt, "agent/pushed", base); err != nil {
		t.Fatal(err)
	}
	if got := run(wt, "rev-parse", "HEAD"); got != localHead {
		t.Fatalf("local branch: HEAD %s, want %s", got, localHead)
	}
	fresh := filepath.Join(state, "fresh")
	if err := prepareImplementWorktree(ctx, clone, fresh, "agent/new", base); err != nil {
		t.Fatal(err)
	}
	if got := run(fresh, "rev-parse", "HEAD"); got != base {
		t.Fatalf("new branch: HEAD %s, want base %s", got, base)
	}
}

// The dashboard reads "not pushed" in a review's failure reason for runs
// from workers that don't report `pushed`; the two strings must agree.
func TestNotPushedMarkerMatchesDashboard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "main.ts"))
	if err != nil {
		t.Skip("dashboard source not present:", err)
	}
	if !strings.Contains(string(src), "' "+notPushedMarker+"'") {
		t.Fatalf("web/src/main.ts unpushed() must match the worker's %q", notPushedMarker)
	}
}
