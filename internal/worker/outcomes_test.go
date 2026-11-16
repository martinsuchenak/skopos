package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A signed-off item's pull request is watched: merged and closed are
// reported at once, an open one only when the watch ends; a pull request
// the forge no longer shows, or an item the server no longer has, ends the
// watch without a report. Lookups are spaced an hour apart.
func TestSweepOutcomes(t *testing.T) {
	const (
		merged  = "01a19000-0000-7000-8000-0000000000a1"
		open    = "01a19000-0000-7000-8000-0000000000a2"
		closed  = "01a19000-0000-7000-8000-0000000000a3"
		gonePR  = "01a19000-0000-7000-8000-0000000000a4"
		goneItm = "01a19000-0000-7000-8000-0000000000a5"
	)
	prs := map[int]string{
		1: `{"number":1,"html_url":"https://forge/o/r/pulls/1","state":"closed","merged":true,"merged_at":"2026-11-20T10:00:00Z","merge_commit_sha":"abc123","closed_at":"2026-11-20T10:00:00Z"}`,
		2: `{"number":2,"html_url":"https://forge/o/r/pulls/2","state":"open","merged":false}`,
		3: `{"number":3,"html_url":"https://forge/o/r/pulls/3","state":"closed","merged":false,"closed_at":"2026-11-21T09:00:00Z"}`,
		5: `{"number":5,"html_url":"https://forge/o/r/pulls/5","state":"closed","merged":true,"merged_at":"2026-11-20T10:00:00Z"}`,
	}
	var mu sync.Mutex
	lookups := map[int]int{}
	reports := map[string]OutcomeReport{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/o/r/pulls/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/o/r/pulls/"), "%d", &n)
		mu.Lock()
		lookups[n]++
		mu.Unlock()
		body, ok := prs[n]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/inbox/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/inbox/"), "/outcome")
		if r.Method != http.MethodPut || id == goneItm {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var in OutcomeReport
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		reports[id] = in
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("OUTCOME_TOKEN", "tok")
	repo := &Repo{Path: t.TempDir(), DefaultBranch: "main"}
	repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv = "gitea", srv.URL+"/o/r.git", "OUTCOME_TOKEN"
	w := &Worker{cfg: &Config{ID: "worker-1", StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": repo}}, client: NewClient(srv.URL, "k"), log: slog.Default()}
	start := time.Now()
	for id, n := range map[string]int{merged: 1, open: 2, closed: 3, gonePR: 4, goneItm: 5} {
		w.watchOutcome(branchRecord{ItemID: id, WorkspaceID: "ws", PRNumber: n, PRURL: fmt.Sprintf("https://forge/o/r/pulls/%d", n)}, start)
	}
	watched := func(id string) bool {
		_, err := os.Stat(filepath.Join(w.outcomesDir(), id+".json"))
		return err == nil
	}

	ctx := context.Background()
	w.sweepOutcomes(ctx, start)
	if r := reports[merged]; r.State != "merged" || r.MergeSHA != "abc123" || r.MergedAt == nil || r.Reporter != "worker-1" || watched(merged) {
		t.Fatalf("merged: %+v, still watched %v", r, watched(merged))
	}
	if r := reports[closed]; r.State != "closed" || r.ClosedAt == nil || r.MergedAt != nil || watched(closed) {
		t.Fatalf("closed: %+v", r)
	}
	if _, ok := reports[open]; ok || !watched(open) {
		t.Fatal("open: not reported, still watched")
	}
	if _, ok := reports[gonePR]; ok || watched(gonePR) {
		t.Fatal("a pull request the forge does not show ends the watch, unreported")
	}
	if watched(goneItm) {
		t.Fatal("an item the server does not have ends the watch")
	}

	w.sweepOutcomes(ctx, start.Add(10*time.Minute))
	if lookups[2] != 1 {
		t.Fatalf("an open pull request is looked up at most hourly: %d lookups", lookups[2])
	}
	w.sweepOutcomes(ctx, start.Add(2*time.Hour))
	if lookups[2] != 2 || !watched(open) {
		t.Fatal("an hour later it is looked up again, and still watched")
	}
	w.sweepOutcomes(ctx, start.Add(outcomeWatch+time.Hour))
	if r := reports[open]; r.State != "open" || watched(open) {
		t.Fatalf("at the end of the watch an open pull request is reported as open: %+v", r)
	}
}
