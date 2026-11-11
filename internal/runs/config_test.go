package runs

import (
	"context"
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
