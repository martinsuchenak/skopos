package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// The claim names the config a run executes under, for that attempt: the
// revision in force when the run was claimed, a new one when the config
// changed before the next claim, and nothing for a workspace the server
// does not manage.
func TestClaimRecordsTheConfigRevision(t *testing.T) {
	s := testService(t)
	revs := map[string]string{"ws": "rev-1"}
	s.SetConfigSource(func(_ context.Context, ws string) string { return revs[ws] },
		func(_ context.Context, workspaces []string) string {
			return strings.Join(workspaces, "+") + "=" + revs["ws"]
		})
	id := mustQueue(t, s, rootCtx(), "ws", "item-1", StagePlan).ID
	other := mustQueue(t, s, rootCtx(), "free", "item-2", StagePlan).ID
	// The run comes back to its worker: paused, due at once.
	back := func(lease string) {
		t.Helper()
		if err := s.Pause(rootCtx(), id, "w1", "quota", time.Now().Add(-time.Minute), lease); err != nil {
			t.Fatal(err)
		}
	}

	reads := false // a worker that reads config from the server, commands off
	run, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"}, ServerCommands: &reads})
	if err != nil || run == nil || run.ID != id {
		t.Fatalf("claim: %+v %v", run, err)
	}
	if run.ConfigRev != "rev-1" {
		t.Fatalf("the claim answer names the revision: %q", run.ConfigRev)
	}
	if stored, _ := s.Get(rootCtx(), id); stored.ConfigRev != "rev-1" {
		t.Fatalf("the run records it: %q", stored.ConfigRev)
	}
	// The config changes while the run is out; it comes back interrupted
	// and is claimed again: the new attempt names the new revision.
	revs["ws"] = "rev-2"
	back(run.LeaseToken)
	again, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"}, ServerCommands: &reads})
	if err != nil || again == nil || again.ID != id || again.ConfigRev != "rev-2" {
		t.Fatalf("the next attempt runs under the config in force at its claim: %+v %v", again, err)
	}
	// A workspace the server does not manage: no revision, and an earlier
	// attempt's is not kept.
	revs["ws"] = ""
	back(again.LeaseToken)
	third, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"}, ServerCommands: &reads})
	if err != nil || third == nil || third.ConfigRev != "" {
		t.Fatalf("no longer managed: %+v %v", third, err)
	}
	if stored, _ := s.Get(rootCtx(), id); stored.ConfigRev != "" {
		t.Fatalf("the stored revision is cleared with it: %q", stored.ConfigRev)
	}
	free, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w2", Workspaces: []string{"free"}, Runners: []string{"claude"}})
	if err != nil || free == nil || free.ID != other || free.ConfigRev != "" {
		t.Fatalf("an unmanaged workspace: %+v %v", free, err)
	}
}

// Claim answers carry the runner definitions' revision and one value for
// the offered workspaces' config, so a worker knows when to refetch.
func TestClaimAnswerHeaders(t *testing.T) {
	s := testService(t)
	s.SetConfigSource(func(context.Context, string) string { return "r" }, func(_ context.Context, ws []string) string { return "digest-" + strings.Join(ws, ",") })
	s.SetFleetRev(func(context.Context) string { return "fleet-1" })
	h := NewHandler(s)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/claim", strings.NewReader(`{"worker_id":"w1","workspaces":["ws"],"runners":["claude"]}`))
	req.Header.Set("Content-Type", "application/json")
	h.Claim(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Skopos-Fleet-Rev"); got != "fleet-1" {
		t.Fatalf("Skopos-Fleet-Rev: %q", got)
	}
	if got := rec.Header().Get("Skopos-Config-Rev"); got != "digest-ws" {
		t.Fatalf("Skopos-Config-Rev: %q", got)
	}
}

// Whether a worker takes commands from the server is part of what it says
// about itself: stored, listed, and a change is an audit event. Applying
// new runner definitions is its own event.
func TestServerCommandsAndFleetRevAreRecorded(t *testing.T) {
	s := testService(t)
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	claim := func(commands *bool, fleet string) {
		t.Helper()
		if _, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Runners: []string{"claude"},
			RunnerFacts: []policy.Facts{fClaude}, ServerCommands: commands, FleetRev: fleet}); err != nil {
			t.Fatal(err)
		}
	}
	events := func(action string) []string {
		var out []string
		for _, e := range spy.got {
			if e.Action == action {
				out = append(out, e.Notes)
			}
		}
		return out
	}
	off, on := false, true
	claim(&off, "")
	claim(&off, "")
	if got := events("worker.facts"); len(got) != 1 || !strings.Contains(got[0], "server_commands off") {
		t.Fatalf("the first report, once: %v", got)
	}
	claim(&on, "fleet-1")
	facts := events("worker.facts")
	if len(facts) != 2 || !strings.Contains(facts[1], "was: ") || !strings.Contains(facts[1], "server_commands off") || !strings.Contains(facts[1], "now: ") || !strings.HasSuffix(facts[1], "server_commands on") {
		t.Fatalf("turning it on is an event with old and new: %v", facts)
	}
	if got := events("worker.config_applied"); len(got) != 1 || got[0] != "runner definitions fleet-1 (was none)" {
		t.Fatalf("applying runner definitions: %v", got)
	}
	// The definitions alone change: no facts event.
	claim(&on, "fleet-2")
	if len(events("worker.facts")) != 2 || len(events("worker.config_applied")) != 2 {
		t.Fatalf("a new fleet revision is not a change in the worker's facts: %v %v", events("worker.facts"), events("worker.config_applied"))
	}
	workers, err := s.ListWorkers(rootCtx())
	if err != nil || len(workers) != 1 || workers[0].ServerCommands == nil || !*workers[0].ServerCommands || workers[0].FleetRev != "fleet-2" {
		t.Fatalf("listed: %+v %v", workers, err)
	}
	// A worker from before workspace config says nothing about it.
	if _, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w-old", Workspaces: []string{"ws"}, Runners: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	workers, _ = s.ListWorkers(rootCtx())
	for _, w := range workers {
		if w.ID == "w-old" && w.ServerCommands != nil {
			t.Fatalf("an older worker reports nothing: %+v", w)
		}
	}
}

// A worker from before workspace config reads none of it: it gets no run
// of a managed workspace (it would run it from its own file while the run
// recorded the server's revision), and shows as unhealthy for it with the
// reason. Its other workspaces are unaffected.
func TestOldWorkerGetsNoManagedWorkspace(t *testing.T) {
	s := testService(t)
	s.SetConfigSource(func(_ context.Context, ws string) string {
		if ws == "managed" {
			return "rev-1"
		}
		return ""
	}, func(context.Context, []string) string { return "d" })
	managed := mustQueue(t, s, rootCtx(), "managed", "item-1", StagePlan).ID
	free := mustQueue(t, s, rootCtx(), "free", "item-2", StagePlan).ID

	old := ClaimInput{WorkerID: "w-old", Workspaces: []string{"managed", "free"}, Runners: []string{"claude"}}
	run, err := s.ClaimNext(rootCtx(), old)
	if err != nil || run == nil || run.ID != free {
		t.Fatalf("an old worker still takes its unmanaged workspace's run: %+v %v", run, err)
	}
	if run, err := s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w-old-2", Workspaces: []string{"managed"}, Runners: []string{"claude"}}); err != nil || run != nil {
		t.Fatalf("and none of the managed one: %+v %v", run, err)
	}
	workers, _ := s.ListWorkers(rootCtx())
	found := false
	for _, w := range workers {
		if w.ID == "w-old-2" {
			found = true
			if !strings.Contains(w.Unhealthy["managed"], "upgrade it") {
				t.Fatalf("shown as unhealthy with the reason: %v", w.Unhealthy)
			}
		}
	}
	if !found {
		t.Fatal("the worker is registered")
	}
	on := true
	run, err = s.ClaimNext(rootCtx(), ClaimInput{WorkerID: "w-new", Workspaces: []string{"managed"}, Runners: []string{"claude"}, ServerCommands: &on})
	if err != nil || run == nil || run.ID != managed || run.ConfigRev != "rev-1" {
		t.Fatalf("a worker that reads server config takes it: %+v %v", run, err)
	}
}

// A claimed run's MCP servers travel in the claim answer, for the run's own
// workspace: resolved for the claiming worker (whether it takes commands),
// never stored, never in a later read of the run. A worker that cannot
// take them is kept away from the workspaces that need it, and servers
// that cannot be resolved are said so instead of left out silently.
func TestClaimCarriesTheRunsMCPServers(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	const secret = "tok-very-secret-value"
	var asked []string
	fail := false
	s.SetMCPSource(func(_ context.Context, ws string, commands bool) (*RunMCP, error) {
		asked = append(asked, fmt.Sprintf("%s commands=%v", ws, commands))
		if fail {
			return nil, errors.New("database is locked")
		}
		if ws != "held" {
			return nil, nil
		}
		return &RunMCP{Servers: map[string]json.RawMessage{"router": json.RawMessage(`{"type":"http","url":"https://x/mcp","headers":{"Authorization":"Bearer ` + secret + `"}}`)},
			Secrets: []string{secret}}, nil
	}, func(_ context.Context, ws string) bool { return ws == "held" })
	on := true
	claim := func(worker, ws string, features ...string) *Run {
		t.Helper()
		run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{ws}, Runners: []string{"claude"},
			RunnerFacts: []policy.Facts{fClaude}, ServerCommands: &on, Features: features})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	queued := mustQueue(t, s, ctx, "held", "item-1", StagePlan)
	// A worker whose release cannot take the servers from the claim gets
	// none of the workspace's runs, and shows why.
	if run := claim("w-old", "held"); run != nil {
		t.Fatalf("a worker without the feature got the run: %+v", run)
	}
	if got := s.storage.WorkerUnhealthy(ctx, "w-old")["held"]; !strings.Contains(got, "MCP servers") {
		t.Fatalf("unhealthy reason: %q", got)
	}
	run := claim("w-new", "held", FeatureRunMCP)
	if run == nil || run.ID != queued.ID || run.MCP == nil || len(run.MCP.Servers) != 1 || run.MCP.Secrets[0] != secret {
		t.Fatalf("the claim answer: %+v", run)
	}
	if asked[len(asked)-1] != "held commands=true" {
		t.Fatalf("resolved for the run's workspace and this worker: %v", asked)
	}
	// Only the claim answer: reading the run later shows none of it, and
	// nothing was stored.
	got, err := s.Get(ctx, run.ID)
	if err != nil || got.MCP != nil {
		t.Fatalf("a later read of the run: %+v %v", got.MCP, err)
	}
	buf, _ := json.Marshal(got)
	if strings.Contains(string(buf), secret) {
		t.Fatalf("the run as read back holds the secret: %s", buf)
	}
	var stored int
	if err := s.storage.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE CAST(facts AS TEXT) LIKE ? OR result_summary LIKE ? OR fail_reason LIKE ?`,
		"%"+secret+"%", "%"+secret+"%", "%"+secret+"%").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the secret reached the runs table: %d %v", stored, err)
	}
	// A workspace the server holds no MCP servers for: nothing in the
	// answer, and a worker without the feature is welcome.
	mustQueue(t, s, ctx, "free", "item-2", StagePlan)
	if run := claim("w-old", "free"); run == nil || run.MCP != nil {
		t.Fatalf("a workspace with the worker's own MCP config: %+v", run)
	}
	// Servers that cannot be resolved: the worker is told, so it gives the
	// run back instead of running without them.
	fail = true
	mustQueue(t, s, ctx, "held", "item-3", StagePlan)
	if run := claim("w-other", "held", FeatureRunMCP); run == nil || run.MCP == nil || !run.MCP.Unreadable || len(run.MCP.Servers) != 0 {
		t.Fatalf("unresolvable servers: %+v", run)
	}
}

// A worker id is the caller's word. A second key polling under another
// worker's id — a worker config copied to a second host — is never handed
// that worker's interrupted run of a workspace outside its own scope, nor
// the run's agent token, nor the workspace's MCP credentials.
func TestOwnRunIsNotHandedToAnotherKeysScope(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	const secret = "tok-of-the-held-workspace"
	s.SetMCPSource(func(_ context.Context, ws string, _ bool) (*RunMCP, error) {
		if ws != "held" {
			return nil, nil
		}
		return &RunMCP{Servers: map[string]json.RawMessage{"x": json.RawMessage(`{"type":"http","url":"https://x"}`)}, Secrets: []string{secret}}, nil
	}, func(_ context.Context, ws string) bool { return ws == "held" })
	key := func(id string, ws ...string) context.Context {
		set := map[string]struct{}{}
		for _, w := range ws {
			set[w] = struct{}{}
		}
		return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: id, Name: id, Workspaces: set})
	}
	k1, k2 := key("k1", "held"), key("k2", "other")
	on := true
	claim := func(ctx context.Context, ws string, features ...string) *Run {
		t.Helper()
		run, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{ws}, Runners: []string{"claude"},
			RunnerFacts: []policy.Facts{fClaude}, ServerCommands: &on, Features: features})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	queued := mustQueue(t, s, rootCtx(), "held", "item-1", StagePlan)
	first := claim(k1, "held", FeatureRunMCP)
	if first == nil || first.MCP == nil {
		t.Fatalf("the owner's claim: %+v", first)
	}
	if err := s.Pause(k1, first.ID, "w1", "quota", now.Add(-time.Second), first.LeaseToken); err != nil {
		t.Fatal(err)
	}
	// Another key, the same worker id, another scope.
	if run := claim(k2, "other", FeatureRunMCP); run != nil {
		t.Fatalf("a key without the workspace was handed its run: workspace %s, MCP %v, agent token %v", run.WorkspaceID, run.MCP != nil, run.AgentToken != "")
	}
	if got, _ := s.Get(rootCtx(), queued.ID); got.Status != StatusPaused || got.WorkerID != "w1" {
		t.Fatalf("the run was touched: %+v", got)
	}
	// The same worker and key, on a release that cannot take MCP servers
	// from a claim, offering another workspace: its own run stays put too.
	k1other := key("k1", "held", "other")
	if run := claim(k1other, "other"); run != nil {
		t.Fatalf("a worker that cannot take the servers got the run back: %+v", run)
	}
	// The owner, able, gets it back with its servers.
	back := claim(k1other, "other", FeatureRunMCP)
	if back == nil || back.ID != queued.ID || back.MCP == nil || back.MCP.Secrets[0] != secret {
		t.Fatalf("the owner's own run, from a claim that offers another workspace: %+v", back)
	}
}
