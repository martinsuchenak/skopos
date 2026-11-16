package worker

import (
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
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// A planner result with risks and questions (the planner's schema): the
// questions as entries with options, one the old bare-string shape.
const fakePlanner = `printf '%s' '{"type":"result","session_id":"s1","structured_output":{"title":"Cache layer","summary":"Adds a cache.","size":"m","needs_clarification":false,"steps":[{"title":"Add the cache","description":"depends on question 1"}],"risks":["stale reads"],"questions":[{"question":"Which backend?","options":["Redis","in-process"]},"Keep the flag?"]}}'
`

// A plan with questions: the worker writes the plan (with its risks), then
// the questions as their own section in the numbered format the dashboard,
// Slack and later prompts read. They used to be dropped whenever the
// planner returned steps.
func TestPlanWritesQuestionsAndRisks(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir, _ := gitRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"clone", "-q", "--bare", dir, origin}, {"-C", dir, "remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	const itemID = "01a19700-0000-7000-8000-0000000000aa"
	var mu sync.Mutex
	content := "Add a cache."
	var reports []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/inbox/"+itemID && r.Method == http.MethodGet:
			b, _ := json.Marshal(map[string]any{"id": itemID, "workspace_id": "ws", "title": "Cache", "content": content, "status": "planning"})
			_, _ = w.Write(b)
		case r.URL.Path == "/api/inbox/"+itemID && r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			content = body["content"]
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/plans" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"plan-1"}`)
		case r.URL.Path == "/api/inbox/"+itemID+"/link-plan":
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/plans/plan-1/replan":
			_, _ = io.WriteString(w, `{"id":"rev-1"}`)
		case r.URL.Path == "/api/runs/run-1/report":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	repo := &Repo{Path: dir, DefaultBranch: "main", BranchPrefix: "agent/"}
	cfg := &Config{
		ID: "worker-test", Bin: fakeCLI(t, fakePlanner), StateDir: t.TempDir(), PlanTimeout: time.Minute,
		Repos:    map[string]*Repo{"ws": repo},
		Accounts: []*Account{{Name: "a1", Runner: "claude"}},
	}
	w := New(cfg, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := &Run{ID: "run-1", WorkspaceID: "ws", ItemID: itemID, Stage: "plan"}
	if err := w.stagePlan(t.Context(), run, w.log); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || reports[0]["status"] != "succeeded" {
		t.Fatalf("reports: %v", reports)
	}
	_, secs := splitItem(content)
	if len(secs) != 2 || secs[0].Heading != "Enrichment — skopos worker" || secs[1].Heading != "Questions — planner" {
		t.Fatalf("sections: %q", content)
	}
	if !strings.Contains(secs[0].Body, "**Risks:**\n- stale reads") {
		t.Errorf("the plan must carry its risks: %q", secs[0].Body)
	}
	if want := "1. Which backend?\n   - (a) Redis\n   - (b) in-process\n2. Keep the flag?"; secs[1].Body != want {
		t.Errorf("questions:\n%q\nwant\n%q", secs[1].Body, want)
	}
}

// The planner finds nothing to do: no plan is written, the reason goes on
// the item, and the run reports nothing_to_do with it.
func TestPlanNothingToDo(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir, _ := gitRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"clone", "-q", "--bare", dir, origin}, {"-C", dir, "remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	const itemID = "01a19700-0000-7000-8000-0000000000ab"
	var mu sync.Mutex
	content := "Update the dependencies."
	var reports []map[string]any
	planned := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/inbox/"+itemID && r.Method == http.MethodGet:
			b, _ := json.Marshal(map[string]any{"id": itemID, "workspace_id": "ws", "title": "Deps", "content": content, "status": "planning"})
			_, _ = w.Write(b)
		case r.URL.Path == "/api/inbox/"+itemID && r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			content = body["content"]
			_, _ = io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/api/plans"):
			planned = true
			http.NotFound(w, r)
		case r.URL.Path == "/api/runs/run-1/report":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	const nothing = `printf '%s' '{"type":"result","session_id":"s1","structured_output":{"title":"Deps","summary":"Every dependency is already current.","size":"s","needs_clarification":false,"nothing_to_do":true,"steps":[],"risks":[],"questions":[]}}'
`
	cfg := &Config{
		ID: "worker-test", Bin: fakeCLI(t, nothing), StateDir: t.TempDir(), PlanTimeout: time.Minute,
		Repos:    map[string]*Repo{"ws": {Path: dir, DefaultBranch: "main", BranchPrefix: "agent/"}},
		Accounts: []*Account{{Name: "a1", Runner: "claude"}},
	}
	w := New(cfg, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.stagePlan(t.Context(), &Run{ID: "run-1", WorkspaceID: "ws", ItemID: itemID, Stage: "plan"}, w.log); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if planned {
		t.Error("no plan is written when there is nothing to do")
	}
	if len(reports) != 1 || reports[0]["status"] != "succeeded" || reports[0]["nothing_to_do"] != true || reports[0]["summary"] != "Every dependency is already current." {
		t.Fatalf("reports: %v", reports)
	}
	if !strings.Contains(content, "## Enrichment — skopos worker\n\n**Nothing to do.** Every dependency is already current.") {
		t.Fatalf("the reason on the item:\n%s", content)
	}
}

// A planner the workspace gives commands (here a kit) plans in a detached
// worktree, never in the clone itself, which later runs start from.
func TestPlanWithGrantsRunsInAWorktree(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	kitHome(t)
	dir, _ := gitRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"clone", "-q", "--bare", dir, origin}, {"-C", dir, "remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	const itemID = "01a19700-0000-7000-8000-0000000000bb"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/inbox/"+itemID && r.Method == http.MethodGet:
			b, _ := json.Marshal(map[string]any{"id": itemID, "workspace_id": "ws", "title": "Cache", "content": "Add a cache.", "status": "planning"})
			_, _ = w.Write(b)
		case r.URL.Path == "/api/plans" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"plan-1"}`)
		case r.URL.Path == "/api/plans/plan-1/replan":
			_, _ = io.WriteString(w, `{"id":"rev-1"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	where := filepath.Join(t.TempDir(), "pwd")
	planner := "pwd > '" + where + "'\n" + fakePlanner
	cfg := &Config{
		ID: "worker-test", Bin: fakeCLI(t, planner), StateDir: t.TempDir(), PlanTimeout: time.Minute, ServerCommands: true,
		Repos:    map[string]*Repo{"ws": {Path: dir, DefaultBranch: "main", BranchPrefix: "agent/"}},
		Accounts: []*Account{{Name: "a1", Runner: "claude"}},
	}
	cfg.setStages("ws", map[string]*wsconfig.StageCaps{"plan": {Kits: []string{"task"}}})
	w := New(cfg, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.stagePlan(t.Context(), &Run{ID: "run-2", WorkspaceID: "ws", ItemID: itemID, Stage: "plan", Runner: "claude"}, w.log); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(where)
	if err != nil {
		t.Fatal(err)
	}
	// The worktree is gone by now (removed after the run): its path says.
	ran := strings.TrimSpace(string(got))
	if !strings.Contains(ran, filepath.Join("worktrees", filepath.Base(dir), "plan-")) {
		t.Fatalf("the planner ran in %s (the clone is %s)", ran, dir)
	}
}
