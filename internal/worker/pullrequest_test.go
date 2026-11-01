package worker

import (
	"context"
	"encoding/json"
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
)

func TestNewPRClientDerivesAPI(t *testing.T) {
	cases := []struct {
		kind, remote, api, owner, repo string
	}{
		{"github", "https://github.com/martinsuchenak/skopos-sandbox.git", "https://api.github.com", "martinsuchenak", "skopos-sandbox"},
		{"github", "https://ghe.example.com/org/app", "https://ghe.example.com/api/v3", "org", "app"},
		{"gitea", "https://gitea.example.com/fortix/freedom3.git", "https://gitea.example.com/api/v1", "fortix", "freedom3"},
		{"gitea", "https://example.com/git/fortix/freedom3.git", "https://example.com/git/api/v1", "fortix", "freedom3"},
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

// fakeForge is a minimal pull-request API: it lists, opens and closes pull
// requests and records what it was sent.
type fakeForge struct {
	mu      sync.Mutex
	prs     []map[string]any
	auth    []string
	created []map[string]any
	closed  []string
	queries []string
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
		f.queries = append(f.queries, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(f.prs)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
		var in map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &in)
		f.created = append(f.created, in)
		pr := map[string]any{"number": 40 + len(f.created), "html_url": "https://forge/o/r/pull/41", "state": "open",
			"head": map[string]any{"ref": in["head"]}, "base": map[string]any{"ref": in["base"]}}
		f.prs = append(f.prs, pr)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pr)
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/pulls/"):
		f.closed = append(f.closed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

// The first round opens the pull request; a change-request round pushes
// to the same branch and reuses it.
func TestEnsurePullRequestOpensThenReuses(t *testing.T) {
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
			pr, created, err := c.Ensure(ctx, "agent/x", "main", "Fix textutil", "body", true)
			if err != nil || !created || pr.Number != 41 || pr.URL == "" {
				t.Fatalf("first round: %+v created=%v err=%v", pr, created, err)
			}
			pr2, created2, err := c.Ensure(ctx, "agent/x", "main", "Fix textutil", "body", true)
			if err != nil || created2 || pr2.Number != 41 {
				t.Fatalf("second round must reuse: %+v created=%v err=%v", pr2, created2, err)
			}
			if len(forge.created) != 1 {
				t.Fatalf("opened %d pull requests, want 1", len(forge.created))
			}
			in := forge.created[0]
			if kind == "github" {
				if in["draft"] != true || in["title"] != "Fix textutil" || forge.auth[0] != "Bearer tok" {
					t.Fatalf("github create: %v auth %q", in, forge.auth[0])
				}
				if !strings.Contains(forge.queries[0], "head=o%3Aagent%2Fx") {
					t.Fatalf("github lookup must filter by head: %q", forge.queries[0])
				}
			} else if in["title"] != "WIP: Fix textutil" || in["draft"] != nil || forge.auth[0] != "token tok" {
				t.Fatalf("gitea create: %v auth %q", in, forge.auth[0])
			}
			if err := c.Close(ctx, 41); err != nil || len(forge.closed) != 1 || forge.closed[0] != "41" {
				t.Fatalf("close: %v %v", err, forge.closed)
			}
		})
	}
}

// The sweep closes a rejected item's pull request, leaves an approved
// one's for the approver to merge, removes finished worktrees, deletes the
// local branch only when it was pushed, and leaves running items alone.
func TestSweepBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	forge := &fakeForge{}
	status := map[string]string{
		"01a19000-0000-7000-8000-00000000000a": "discarded",
		"01a19000-0000-7000-8000-00000000000b": "done",
		"01a19000-0000-7000-8000-00000000000c": "in_review",
		"01a19000-0000-7000-8000-00000000000d": "done",
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", forge)
	mux.HandleFunc("/api/inbox/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/inbox/")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": status[id]})
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
	for id, pr := range map[string]int{"01a19000-0000-7000-8000-00000000000a": 5, "01a19000-0000-7000-8000-00000000000b": 6, "01a19000-0000-7000-8000-00000000000c": 7, "01a19000-0000-7000-8000-00000000000d": 0} {
		branch := "agent/" + id[len(id)-1:]
		wt := filepath.Join(state, "worktrees", "r", "agent-"+id[len(id)-1:])
		if err := createWorktreeAt(ctx, clone, wt, branch, "main"); err != nil {
			t.Fatal(err)
		}
		rec := branchRecord{ItemID: id, WorkspaceID: "ws", Branch: branch, Worktree: wt, Pushed: pr > 0, PRNumber: pr}
		w.saveBranch(rec)
		recs[id] = rec
	}
	w.sweepBranches(ctx)

	if len(forge.closed) != 1 || forge.closed[0] != "5" {
		t.Fatalf("only the rejected item's pull request is closed: %v", forge.closed)
	}
	branchExists := func(b string) bool {
		return exec.Command("git", "-C", clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Run() == nil
	}
	for id, rec := range recs {
		_, statErr := os.Stat(rec.Worktree)
		_, kept := w.loadBranch(id)
		switch status[id] {
		case "in_review":
			if statErr != nil || !kept || !branchExists(rec.Branch) {
				t.Fatalf("%s: a running item is left alone", id)
			}
		default:
			if statErr == nil || kept {
				t.Fatalf("%s (%s): worktree and record must be gone", id, status[id])
			}
			if branchExists(rec.Branch) == rec.Pushed {
				t.Fatalf("%s: local branch kept=%v, pushed=%v (only an unpushed branch stays)", id, branchExists(rec.Branch), rec.Pushed)
			}
		}
	}
}
