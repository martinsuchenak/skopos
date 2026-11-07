//go:build unix

package worker

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIndexServer records branch commits and deletes of the code index API.
type fakeIndexServer struct {
	mu      sync.Mutex
	commits []string // "<branch>@<head>"
	files   map[string]int
	drops   []string
}

func (f *fakeIndexServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/manifest"):
			var in struct {
				Files []struct {
					Hash string `json:"hash"`
				} `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			missing := []string{}
			for _, f := range in.Files {
				missing = append(missing, f.Hash)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"missing": missing})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs"):
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commit"):
			var in struct {
				Branch string `json:"branch"`
				Head   string `json:"head_sha"`
				Source string `json:"source"`
				Files  []any  `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Source != "worker:w1" {
				t.Errorf("source %q", in.Source)
			}
			f.commits = append(f.commits, in.Branch+"@"+in.Head)
			if f.files == nil {
				f.files = map[string]int{}
			}
			f.files[in.Branch] = len(in.Files)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/branch/"):
			f.drops = append(f.drops, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	})
}

func (f *fakeIndexServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commits)
}

// An idle pass indexes the assigned default branch when origin moved (and
// only then), the queued item branch of a workspace with the switch on, and
// a retired branch's index is dropped. The clone itself is never checked
// out elsewhere: indexing uses its own worktree.
func TestIndexPass(t *testing.T) {
	o := newOriginClone(t)
	o.commit("main.go", "package main\n\nfunc Hello() string { return \"hi\" }\n", "code")
	o.run(o.w, "push", "-q", "origin", "main")
	fake := &fakeIndexServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	w := New(&Config{ID: "w1", StateDir: t.TempDir(), Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": repo}}, NewClient(srv.URL, "k"), slog.New(slog.DiscardHandler))
	if got := w.indexable(); !slices.Equal(got, []string{"ws"}) {
		t.Fatalf("indexable %v", got)
	}
	ctx := t.Context()
	head := o.run(o.w, "rev-parse", "HEAD")
	w.indexPass(ctx, []string{"ws"}, []string{"ws"})
	if got := fake.seen(); !slices.Equal(got, []string{"main@" + head}) {
		t.Fatalf("first pass commits %v, want main@%s", got, head)
	}
	if fake.files["main"] < 2 {
		t.Fatalf("main index has %d files", fake.files["main"])
	}
	// The clone's own checkout is untouched; the index worktree is apart.
	if br := o.run(o.clone, "rev-parse", "--abbrev-ref", "HEAD"); br != "main" {
		t.Fatalf("clone moved to %s", br)
	}
	if _, err := os.Stat(filepath.Join(w.indexCheckout(repo), "main.go")); err != nil {
		t.Fatalf("index worktree: %v", err)
	}

	// Not due again until indexCheckEvery; when due, an unmoved origin
	// pushes nothing.
	w.indexPass(ctx, []string{"ws"}, []string{"ws"})
	w.index.checked["ws"] = time.Time{}
	w.indexPass(ctx, []string{"ws"}, []string{"ws"})
	if got := fake.seen(); len(got) != 1 {
		t.Fatalf("unmoved origin re-indexed: %v", got)
	}

	// Origin moves: the next due pass indexes the new head.
	head2 := o.commit("more.go", "package main\n\nfunc More() {}\n", "more")
	o.run(o.w, "push", "-q", "origin", "main")
	w.index.checked["ws"] = time.Time{}
	w.indexPass(ctx, []string{"ws"}, []string{"ws"})
	if got := fake.seen(); len(got) != 2 || got[1] != "main@"+head2 {
		t.Fatalf("moved origin: %v", got)
	}

	// An implemented item branch (local in the clone) is indexed once it is
	// queued, only for a workspace with the switch on.
	o.run(o.clone, "branch", "agent/item-1", "origin/main")
	w.queueBranchIndex("ws", "agent/item-1")
	w.indexPass(ctx, nil, nil) // switch off: the job is dropped
	if got := fake.seen(); len(got) != 2 || len(w.index.state.Queue) != 0 {
		t.Fatalf("switch off: commits %v queue %v", got, w.index.state.Queue)
	}
	w.queueBranchIndex("ws", "agent/item-1")
	w.indexPass(ctx, []string{"ws"}, nil)
	if got := fake.seen(); len(got) != 3 || got[2] != "agent/item-1@"+head2 || len(w.index.state.Queue) != 0 {
		t.Fatalf("item branch: commits %v queue %v", got, w.index.state.Queue)
	}

	// At the end of an implement run the branch is indexed at once (switch
	// on), not left for an idle moment.
	head3 := o.commit("third.go", "package main\n\nfunc Third() {}\n", "third")
	o.run(o.w, "push", "-q", "origin", "main")
	o.run(o.clone, "fetch", "-q", "origin")
	o.run(o.clone, "branch", "agent/item-2", "origin/main")
	w.client.indexEnabled = []string{"ws"}
	w.indexBranchNow(ctx, repo, "ws", "agent/item-2")
	if got := fake.seen(); len(got) != 4 || got[3] != "agent/item-2@"+head3 || len(w.index.state.Queue) != 0 {
		t.Fatalf("implement-time index: commits %v queue %v", got, w.index.state.Queue)
	}
	w.client.indexEnabled = nil
	w.indexBranchNow(ctx, repo, "ws", "agent/item-2") // switch off: nothing
	if got := fake.seen(); len(got) != 4 {
		t.Fatalf("switch off must not index: %v", got)
	}

	// The state survives a restart.
	w2 := New(&Config{ID: "w1", StateDir: w.cfg.StateDir, Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": repo}}, NewClient(srv.URL, "k"), slog.New(slog.DiscardHandler))
	w2.index.mu.Lock()
	w2.loadIndexState()
	saved := w2.index.state
	w2.index.mu.Unlock()
	if saved.Default["ws"] != head2 || saved.Branches[branchKey("ws", "agent/item-1")] != head2 {
		t.Fatalf("saved state %+v", saved)
	}

	// The sweep retires the branch: its index is dropped on the server.
	w.client.indexEnabled = []string{"ws"}
	w.dropBranchIndex(ctx, "ws", "agent/item-1")
	if len(fake.drops) != 1 || !strings.HasSuffix(fake.drops[0], "/branch/agent%2Fitem-1") && !strings.HasSuffix(fake.drops[0], "/branch/agent/item-1") {
		t.Fatalf("drops %v", fake.drops)
	}
}

// A failing fetch marks the workspace: not indexable, and not retried until
// indexRetryAfter.
func TestIndexFailureBacksOff(t *testing.T) {
	o := newOriginClone(t)
	o.run(o.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	w := New(&Config{ID: "w1", StateDir: t.TempDir(), Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": repo}}, NewClient("http://127.0.0.1:1", "k"), slog.New(slog.DiscardHandler))
	w.indexPass(t.Context(), []string{"ws"}, []string{"ws"})
	if got := w.indexable(); len(got) != 0 {
		t.Fatalf("a repo whose fetch failed must not be indexable: %v", got)
	}
	w.client.indexDefault = []string{"ws"}
	w.maybeIndex(t.Context())
	if w.index.done != nil {
		t.Fatal("nothing is due while the failure backs off")
	}
}

// maybeIndex starts a pass in the background and stopIndexing stops it.
func TestStopIndexing(t *testing.T) {
	o := newOriginClone(t)
	srv := httptest.NewServer((&fakeIndexServer{}).handler(t))
	defer srv.Close()
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	w := New(&Config{ID: "w1", StateDir: t.TempDir(), Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": repo}}, NewClient(srv.URL, "k"), slog.New(slog.DiscardHandler))
	w.client.indexEnabled, w.client.indexDefault = []string{"ws"}, []string{"ws"}
	w.maybeIndex(t.Context())
	if w.index.done == nil {
		t.Fatal("a pass must start")
	}
	start := time.Now()
	w.stopIndexing()
	if time.Since(start) > 10*time.Second || w.index.done != nil {
		t.Fatal("stopIndexing must stop the pass")
	}
}

// Two clones with the same directory name get separate indexing worktrees.
func TestIndexCheckoutIsPerClone(t *testing.T) {
	w := New(&Config{StateDir: "/state"}, nil, slog.New(slog.DiscardHandler))
	a, b := w.indexCheckout(&Repo{Path: "/src/fortix/app"}), w.indexCheckout(&Repo{Path: "/src/rylex/app"})
	if a == b || !strings.HasPrefix(filepath.Base(a), "app-") {
		t.Fatalf("index checkouts %q and %q", a, b)
	}
}

// A clone another process holds is not a failure: no back-off, still
// indexable, and the next check tries again.
func TestBusyCloneIsNotAFailure(t *testing.T) {
	o := newOriginClone(t)
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	w := New(&Config{ID: "w1", StateDir: t.TempDir(), Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": repo}}, NewClient("http://127.0.0.1:1", "k"), slog.New(slog.DiscardHandler))
	release, ok, err := tryLockClone(t.Context(), repo.Path)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer release()
	w.indexPass(t.Context(), []string{"ws"}, []string{"ws"})
	if !w.index.failed["ws"].IsZero() || !slices.Equal(w.indexable(), []string{"ws"}) {
		t.Fatalf("busy clone recorded as a failure: failed %v indexable %v", w.index.failed, w.indexable())
	}
}

// A pass that does not stop in time keeps its slot: no second pass starts
// and implement-time indexing queues the branch instead.
func TestStuckPassBlocksASecondOne(t *testing.T) {
	old := indexStopWait
	indexStopWait = 50 * time.Millisecond
	defer func() { indexStopWait = old }()
	w := New(&Config{ID: "w1", StateDir: t.TempDir(), Workspaces: []string{"ws"}, Repos: map[string]*Repo{"ws": {Path: t.TempDir(), DefaultBranch: "main"}}}, NewClient("http://127.0.0.1:1", "k"), slog.New(slog.DiscardHandler))
	stuck := make(chan struct{})
	w.index.mu.Lock()
	w.loadIndexState()
	w.index.done, w.index.cancel = stuck, func() {}
	w.index.mu.Unlock()
	w.stopIndexing()
	if !w.indexRunning() {
		t.Fatal("a pass that did not stop must still count as running")
	}
	w.client.indexEnabled, w.client.indexDefault = []string{"ws"}, []string{"ws"}
	w.maybeIndex(t.Context())
	if w.index.done != stuck {
		t.Fatal("no second pass while the first runs")
	}
	w.indexBranchNow(t.Context(), w.cfg.Repos["ws"], "ws", "agent/x")
	if len(w.index.state.Queue) != 1 {
		t.Fatalf("implement-time indexing must queue while a pass runs: %v", w.index.state.Queue)
	}
	close(stuck)
	if w.indexRunning() {
		t.Fatal("once it exits it no longer counts")
	}
}

// The default-branch worker drops indexes of branches gone from origin and
// built long ago; branches on origin, the default branch and recent builds
// stay; an unreachable origin drops nothing.
func TestPruneStaleIndexes(t *testing.T) {
	o := newOriginClone(t)
	o.devBranch("feature-live")
	old := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	var mu sync.Mutex
	var drops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"branch": "main", "head_sha": "a", "built_at": old},
				{"branch": "feature-live", "head_sha": "b", "built_at": old},
				{"branch": "feature/gone", "head_sha": "c", "built_at": old},
				{"branch": "feature/new-local", "head_sha": "d", "built_at": recent},
			})
		case r.Method == http.MethodDelete:
			mu.Lock()
			drops = append(drops, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	repo := &Repo{Path: o.clone, DefaultBranch: "main"}
	w := New(&Config{ID: "w1", StateDir: t.TempDir()}, NewClient(srv.URL, "k"), slog.New(slog.DiscardHandler))
	w.index.mu.Lock()
	w.loadIndexState()
	w.index.mu.Unlock()
	w.pruneStaleIndexes(t.Context(), repo, "ws")
	if len(drops) != 1 || !strings.Contains(drops[0], "feature%2Fgone") && !strings.HasSuffix(drops[0], "feature/gone") {
		t.Fatalf("drops %v", drops)
	}
	// Not again until indexReconcileEvery.
	w.pruneStaleIndexes(t.Context(), repo, "ws")
	if len(drops) != 1 {
		t.Fatalf("pruned twice: %v", drops)
	}
	// Origin unreachable: nothing is dropped.
	o.run(o.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	w.index.pruned["ws"] = time.Time{}
	w.pruneStaleIndexes(t.Context(), repo, "ws")
	if len(drops) != 1 {
		t.Fatalf("an unreachable origin must drop nothing: %v", drops)
	}
}
