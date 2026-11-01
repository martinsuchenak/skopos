package worker

import (
	"encoding/json"
	"errors"
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

// fakeImplementer is a claude CLI stand-in. The implement turn commits
// state=bad (the check wants good); a fix round acts on $FIX:
//   - fix:     commits state=good, done
//   - nothing: commits nothing, done (the checks keep failing)
//   - blocked: returns blocked
//   - quota:   the first fix call dies on a usage limit, later ones fix
//
// Every call appends its kind (impl / fix) to $CALLS.
const fakeImplementer = `prompt="$2"
c() { git -c user.name=t -c user.email=t@t "$@"; }
done_json() { printf '{"type":"result","session_id":"s1","structured_output":{"status":"done","steps_done":[1],"summary":"%s"}}' "$1"; }
case "$prompt" in
*"fix round"*)
  echo fix >> "$CALLS"
  case "$FIX" in
  fix) echo good > state; git add -A; c commit -qm fix; done_json "fixed the state" ;;
  nothing) done_json "tried" ;;
  blocked) printf '{"type":"result","session_id":"s1","structured_output":{"status":"blocked","steps_done":[1],"summary":"x","reason":"the check needs a database"}}' ;;
  quota)
    if [ ! -f "$CALLS.quota" ]; then touch "$CALLS.quota"; echo "You've hit your usage limit" >&2; exit 1; fi
    echo good > state; git add -A; c commit -qm fix; done_json "fixed after the pause" ;;
  esac ;;
*)
  echo impl >> "$CALLS"
  echo bad > state; git add -A; c commit -qm "agent: step 1"; done_json "implemented" ;;
esac
`

type gateStack struct {
	w       *Worker
	run     *Run
	calls   string
	mu      sync.Mutex
	reports []map[string]any
	phases  []string // the phases heartbeats carried, in order
}

func (g *gateStack) lastReport(t *testing.T) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.reports) == 0 {
		t.Fatal("no report sent")
	}
	return g.reports[len(g.reports)-1]
}

func (g *gateStack) callLog(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(g.calls)
	return strings.Join(strings.Fields(string(b)), ",")
}

// newGateStack: a clone with origin/main at its base, a fake skopos API
// (item, plan, reports) and a worker whose claude is fakeImplementer.
func newGateStack(t *testing.T, fix string, rounds *int, setup ...string) *gateStack {
	t.Helper()
	dir, base := gitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/main", base).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v %s", err, out)
	}
	g := &gateStack{calls: filepath.Join(t.TempDir(), "calls")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/inbox/01a19700-0000-7000-8000-00000000c0de":
			_, _ = io.WriteString(w, `{"id":"01a19700-0000-7000-8000-00000000c0de","workspace_id":"ws","title":"Add state","content":"c","plan_id":"plan-1","status":"implementing"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/plans/plan-1":
			_, _ = io.WriteString(w, `{"id":"plan-1","items":[{"title":"Write the state file","description":"d"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/heartbeat":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.mu.Lock()
			g.phases = append(g.phases, body["phase"])
			g.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/report":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.mu.Lock()
			g.reports = append(g.reports, body)
			g.mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	repo := &Repo{Path: dir, DefaultBranch: "main", BranchPrefix: "agent/", Checks: []string{`test "$(cat state)" = good`}, CheckFixRounds: rounds, Setup: setup}
	repo.Push.Kind, repo.Push.PullRequest = "none", "none"
	cfg := &Config{
		ID: "worker-test", Bin: fakeCLI(t, fakeImplementer), StateDir: t.TempDir(), ImplTimeout: time.Minute,
		Repos:    map[string]*Repo{"ws": repo},
		Accounts: []*Account{{Name: "a1", Runner: "claude", Env: map[string]string{"FIX": fix, "CALLS": g.calls}}},
	}
	g.w = New(cfg, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.run = &Run{ID: "run-1", WorkspaceID: "ws", ItemID: "01a19700-0000-7000-8000-00000000c0de", Stage: "implement", BaseSHA: base}
	return g
}

func checkNamed(t *testing.T, report map[string]any, name string) map[string]any {
	t.Helper()
	list, _ := report["checks"].([]any)
	for _, c := range list {
		if m, _ := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("report has no check %q: %v", name, report["checks"])
	return nil
}

// The gate end to end, through stageImplement with a fake implementer.
func TestImplementChecksGate(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	const check = `test "$(cat state)" = good`
	zero := 0
	t.Run("a fix round fixes it", func(t *testing.T) {
		g := newGateStack(t, "fix", nil, "echo prepared > setup.log")
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		rep := g.lastReport(t)
		if rep["status"] != "succeeded" || !strings.Contains(rep["summary"].(string), "fixed in 1 fix round") {
			t.Fatalf("report: %v", rep)
		}
		if setup, _ := rep["setup"].([]any); len(setup) != 1 || setup[0].(map[string]any)["passed"] != true {
			t.Fatalf("the report must carry the setup that ran: %v", rep["setup"])
		}
		if checkNamed(t, rep, check)["passed"] != true || checkNamed(t, rep, committedCheck)["passed"] != true {
			t.Fatalf("checks: %v", rep["checks"])
		}
		if rep["fix_rounds"] != float64(1) {
			t.Fatalf("the report must carry the fix rounds used: %v", rep["fix_rounds"])
		}
		// The dashboard's Checks step: checks running, the fix, checks again.
		if got := strings.Join(g.phases, ","); got != "checks,fixing,checks" {
			t.Fatalf("phases %s", got)
		}
		if got := g.callLog(t); got != "impl,fix" {
			t.Fatalf("calls %s", got)
		}
	})
	t.Run("still failing blocks with the checks and head", func(t *testing.T) {
		g := newGateStack(t, "nothing", nil)
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		rep := g.lastReport(t)
		if rep["status"] != "failed" || rep["reason"] != "checks failed: "+check+" (after 2 fix rounds)" || rep["head_sha"] == "" {
			t.Fatalf("report: %v", rep)
		}
		if checkNamed(t, rep, check)["passed"] == true {
			t.Fatal("the failing check must be reported as failed")
		}
		if got := g.callLog(t); got != "impl,fix,fix" {
			t.Fatalf("calls %s", got)
		}
	})
	t.Run("no fix rounds blocks at once", func(t *testing.T) {
		g := newGateStack(t, "fix", &zero)
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		if rep := g.lastReport(t); rep["status"] != "failed" || rep["reason"] != "checks failed: "+check {
			t.Fatalf("report: %v", rep)
		}
		if got := g.callLog(t); got != "impl" {
			t.Fatalf("calls %s", got)
		}
	})
	t.Run("a blocked fix round is recorded like a failed gate", func(t *testing.T) {
		g := newGateStack(t, "blocked", nil)
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		rep := g.lastReport(t)
		if rep["status"] != "failed" || !strings.Contains(rep["reason"].(string), "couldn't fix them: the check needs a database") || rep["head_sha"] == "" || rep["checks"] == nil {
			t.Fatalf("report: %v", rep)
		}
	})
	t.Run("a failing setup is reported with its output", func(t *testing.T) {
		g := newGateStack(t, "fix", nil, "echo ok", "echo missing toolchain; false", "echo never")
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatal(err)
		}
		rep := g.lastReport(t)
		setup, _ := rep["setup"].([]any)
		reason, _ := rep["reason"].(string)
		if rep["status"] != "failed" || !strings.HasPrefix(reason, `setup "echo missing toolchain; false" failed`) || !strings.Contains(reason, "missing toolchain") || len(setup) != 2 {
			t.Fatalf("report: %v", rep)
		}
		if last := setup[1].(map[string]any); last["passed"] == true || !strings.Contains(last["output"].(string), "missing toolchain") {
			t.Fatalf("the failed setup command must carry its output: %v", last)
		}
		if got := g.callLog(t); got != "" {
			t.Fatalf("no agent runs after a failed setup, calls %s", got)
		}
	})
	t.Run("a worktree whose setup never finished is set up again", func(t *testing.T) {
		mark := filepath.Join(t.TempDir(), "setup-runs")
		g := newGateStack(t, "fix", nil, "echo ran >> "+mark)
		runs := func() int {
			b, _ := os.ReadFile(mark)
			return strings.Count(string(b), "ran")
		}
		for i, pending := range []bool{false, false, true} {
			if pending {
				// What a worker that died mid-setup leaves behind.
				rec, _ := g.w.loadBranch(g.run.ItemID)
				rec.SetupPending = true
				g.w.saveBranch(rec)
			}
			if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
				t.Fatal(err)
			}
			if want := map[int]int{0: 1, 1: 1, 2: 2}[i]; runs() != want {
				t.Fatalf("pass %d: setup ran %d times, want %d", i, runs(), want)
			}
		}
		if rec, _ := g.w.loadBranch(g.run.ItemID); rec.SetupPending {
			t.Fatal("a finished setup must clear the pending mark")
		}
	})
	t.Run("a quota pause mid-fix resumes at the checks", func(t *testing.T) {
		g := newGateStack(t, "quota", nil, "echo prepared")
		err := g.w.stageImplement(t.Context(), g.run, g.w.log)
		var qe *quotaError
		if !errors.As(err, &qe) {
			t.Fatalf("the first fix round must pause on quota, got %v", err)
		}
		if err := g.w.stageImplement(t.Context(), g.run, g.w.log); err != nil {
			t.Fatalf("resume: %v (calls %s)", err, g.callLog(t))
		}
		rep := g.lastReport(t)
		if rep["status"] != "succeeded" || !strings.Contains(rep["summary"].(string), "implemented") {
			t.Fatalf("report: %v", rep)
		}
		if setup, _ := rep["setup"].([]any); len(setup) != 1 {
			t.Fatalf("the resumed run must still report the setup it ran: %v", rep["setup"])
		}
		// One implement turn: the resume went straight back to fixing.
		if got := g.callLog(t); got != "impl,fix,fix" {
			t.Fatalf("calls %s", got)
		}
	})
}
