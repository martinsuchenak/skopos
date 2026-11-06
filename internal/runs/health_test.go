package runs

import (
	"context"
	"testing"
	"time"
)

// A workspace whose preflight fails on the worker gets none of its runs —
// not even the worker's own interrupted one — while the healthy workspaces
// still do; the registry shows why, and the notifier hears each change
// once.
func TestUnhealthyWorkspaceIsNotClaimed(t *testing.T) {
	s := testService(t)
	s.SetLease(time.Hour)
	var heard []WorkerHealth
	s.SetWorkerHealthNotifier(func(_ context.Context, h WorkerHealth) { heard = append(heard, h) })
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-a", StageImplement)
	mustQueue(t, s, ctx, "ws-b", "item-b", StageImplement)
	sick := map[string]string{"ws-a": "preflight \"redis-cli ping\" failed"}
	claim := func(unhealthy map[string]string) *Run {
		t.Helper()
		var u map[string]string
		if unhealthy != nil {
			u = map[string]string{}
			for k, v := range unhealthy {
				u[k] = v
			}
		}
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a", "ws-b"}, Unhealthy: u})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := claim(sick)
	if r == nil || r.ItemID != "item-b" {
		t.Fatalf("only the healthy workspace's run: %+v", r)
	}
	if len(heard) != 1 || heard[0].Workspace != "ws-a" || heard[0].Healthy || heard[0].Reason == "" {
		t.Fatalf("turning unhealthy is heard once: %+v", heard)
	}
	// Its run in the healthy workspace gets interrupted (a restart): the
	// worker still gets it back; nothing from ws-a.
	if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if r := claim(sick); r != nil {
		t.Fatalf("nothing from the unhealthy workspace: %+v", r)
	}
	if len(heard) != 1 {
		t.Fatalf("still unhealthy is not news: %+v", heard)
	}
	ws, _ := s.ListWorkers(ctx)
	if len(ws) != 1 || ws[0].Unhealthy["ws-a"] == "" {
		t.Fatalf("the registry shows the failing workspace: %+v", ws)
	}
	// Recovered: ws-a's run is claimable again, and that is heard.
	if r := claim(nil); r == nil || r.ItemID != "item-a" {
		t.Fatalf("a recovered workspace's run: %+v", r)
	}
	if len(heard) != 2 || heard[1].Workspace != "ws-a" || !heard[1].Healthy {
		t.Fatalf("recovery is heard: %+v", heard)
	}
	// A workspace the worker doesn't offer can't be reported unhealthy.
	r2, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws-b"}, Unhealthy: map[string]string{"ws-z": "x"}})
	_ = r2
	if ws, _ := s.ListWorkers(ctx); len(ws) != 2 {
		t.Fatalf("workers: %+v", ws)
	} else {
		for _, w := range ws {
			if w.ID == "w2" && len(w.Unhealthy) != 0 {
				t.Fatalf("health for a workspace not offered is dropped: %+v", w)
			}
		}
	}
}

// The worker's own interrupted run in an unhealthy workspace stays put
// (it ages into a takeover for a healthy worker).
func TestUnhealthyWorkspaceKeepsAffinityRun(t *testing.T) {
	s := testService(t)
	s.SetLease(time.Hour)
	ctx := rootCtx()
	mustQueue(t, s, ctx, "ws-a", "item-a", StageImplement)
	r, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if r == nil {
		t.Fatal("claim")
	}
	// The worker restarts (orphan release) and its preflight now fails.
	got, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}, Unhealthy: map[string]string{"ws-a": "down"}})
	if err != nil || got != nil {
		t.Fatalf("the interrupted run must not come back while unhealthy: %+v %v", got, err)
	}
	if got, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}}); got == nil || got.ID != r.ID {
		t.Fatalf("healthy again: the run comes back to its owner: %+v", got)
	}
}
