package runs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
)

type auditSpy struct{ got []audit.RecordInput }

func (a *auditSpy) Record(_ context.Context, in audit.RecordInput) error {
	a.got = append(a.got, in)
	return nil
}

// A paused worker claims nothing — queued runs go to others, its own
// interrupted run waits for it — and shows as paused; it takes work again
// when resumed or when the pause runs out.
func TestWorkerPause(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	spy := &auditSpy{}
	s.SetAuditRecorder(spy)
	ctx := rootCtx()
	claim := func(worker string) *Run {
		t.Helper()
		r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: worker, Workspaces: []string{"ws"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	state := func(worker string) (string, *WorkerPause) {
		t.Helper()
		all, err := s.ListWorkers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range all {
			if w.ID == worker {
				return w.State, w.Paused
			}
		}
		t.Fatalf("no worker %s", worker)
		return "", nil
	}

	if _, err := s.PauseWorker(ctx, "w1", PauseInput{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a worker that never polled: %v", err)
	}
	claim("w1")
	claim("w2")
	p, err := s.PauseWorker(ctx, "w1", PauseInput{For: "2h", Reason: "working on it by hand", Via: "cli"})
	if err != nil || !p.Until.Equal(now.Add(2*time.Hour)) || p.By != "system" { // rootCtx: no principal (internal)
		t.Fatalf("pause: %+v %v", p, err)
	}
	if st, pp := state("w1"); st != "paused" || pp == nil || pp.Reason != "working on it by hand" {
		t.Fatalf("listed as %q %+v", st, pp)
	}
	if s.WorkerPaused(ctx, "w1") == nil || s.WorkerPaused(ctx, "w2") != nil {
		t.Fatal("WorkerPaused")
	}

	queued := mustQueue(t, s, ctx, "ws", "item-1", StagePlan)
	if r := claim("w1"); r != nil {
		t.Fatalf("a paused worker claims nothing: %+v", r)
	}
	if r := claim("w2"); r == nil || r.ID != queued.ID {
		t.Fatalf("others take the queue: %+v", r)
	} else if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w2", Status: StatusSucceeded, Summary: "ok"}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}

	// Its own interrupted run (its session lives there) waits for it.
	_ = s.ResumeWorker(ctx, "w1", "")
	own := mustQueue(t, s, ctx, "ws", "item-2", StagePlan)
	r := claim("w1")
	if r == nil || r.ID != own.ID {
		t.Fatalf("w1 claims: %+v", r)
	}
	if _, err := s.PauseWorker(ctx, "w1", PauseInput{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := state("w1"); st != "busy" {
		t.Fatalf("a run in progress keeps it busy: %q", st)
	}
	if r := claim("w1"); r != nil { // the poll after a restart: released, not handed back
		t.Fatalf("paused: %+v", r)
	}
	if got, _ := s.Get(ctx, own.ID); got.Status != StatusInterrupted {
		t.Fatalf("the run must be released: %s", got.Status)
	}
	if r := claim("w2"); r != nil {
		t.Fatalf("w1's interrupted run is not w2's: %+v", r)
	}
	now = now.Add(24 * time.Hour) // until resumed: still paused
	if r := claim("w1"); r != nil {
		t.Fatalf("still paused: %+v", r)
	}
	if err := s.ResumeWorker(ctx, "w1", "dashboard"); err != nil {
		t.Fatal(err)
	}
	if r := claim("w1"); r == nil || r.ID != own.ID {
		t.Fatalf("resumed: w1 takes its run back: %+v", r)
	}

	// A pause with an end runs out by itself.
	if _, err := s.PauseWorker(ctx, "w2", PauseInput{For: "30m"}); err != nil {
		t.Fatal(err)
	}
	later := mustQueue(t, s, ctx, "ws", "item-3", StagePlan)
	if r := claim("w2"); r != nil {
		t.Fatalf("paused: %+v", r)
	}
	now = now.Add(31 * time.Minute)
	if st, pp := state("w2"); st == "paused" || pp != nil { // offline by now: it last polled 31m ago
		t.Fatalf("the pause ran out: %q %+v", st, pp)
	}
	if r := claim("w2"); r == nil || r.ID != later.ID {
		t.Fatalf("the pause ran out: %+v", r)
	}

	if len(spy.got) != 5 || spy.got[0].Action != "worker.pause" || spy.got[0].Via != "cli" || spy.got[0].Notes != "until 2026-11-09T11:00:00Z: working on it by hand" ||
		spy.got[3].Action != "worker.resume" || spy.got[3].EntityID != "w1" || spy.got[3].EntityType != "worker" {
		t.Fatalf("audit: %+v", spy.got)
	}

	for _, bad := range []PauseInput{{For: "soon"}, {For: "-1h"}, {Until: now.Add(-time.Minute)}, {For: "1h", Until: now.Add(time.Hour)}} {
		if _, err := s.PauseWorker(ctx, "w1", bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// A paused worker is not waited for (a retry's preferred worker) and does
// not hold the index duty.
func TestPausedWorkerIsNotWaitedFor(t *testing.T) {
	s := testService(t)
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := rootCtx()
	r, err := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Indexable: []string{"ws"}})
	if err != nil || r != nil {
		t.Fatal(r, err)
	}
	first := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	r, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws"}, Indexable: []string{"ws"}})
	if r == nil || r.ID != first.ID {
		t.Fatalf("claim: %+v", r)
	}
	if _, err := s.Report(ctx, r.ID, ReportInput{WorkerID: "w1", Status: StatusFailed, Reason: "x"}, r.LeaseToken); err != nil {
		t.Fatal(err)
	}
	again := mustQueue(t, s, ctx, "ws", "item-1", StageImplement)
	if again.PreferWorker != "w1" {
		t.Fatalf("prefers w1: %+v", again)
	}
	if _, err := s.PauseWorker(ctx, "w1", PauseInput{}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Workspaces: []string{"ws"}}); r == nil || r.ID != again.ID {
		t.Fatalf("w1 is paused: w2 takes it: %+v", r)
	}
	able, err := s.storage.IndexableWorkers(ctx, "ws", now.Add(-indexDutyWindow), now)
	if err != nil || len(able) != 0 {
		t.Fatalf("a paused worker cannot index: %+v %v", able, err)
	}
}

// Who may pause a worker: root, the key it runs with, or an approver whose
// scope covers every workspace it serves.
func TestWorkerPauseAccess(t *testing.T) {
	s := testService(t)
	worker := scopedCtx("ws-a", "ws-b") // key k1
	if r, err := s.ClaimNext(worker, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a", "ws-b"}}); err != nil || r != nil {
		t.Fatal(r, err)
	}
	key := func(id string, approver bool, ws ...string) context.Context {
		set := map[string]struct{}{}
		for _, w := range ws {
			set[w] = struct{}{}
		}
		return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: id, Name: id, Approver: approver, Workspaces: set})
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"root", rootCtx(), nil},
		{"the worker's own key", worker, nil},
		{"an approver over both workspaces", key("k2", true, "ws-a", "ws-b"), nil},
		{"an approver over one", key("k3", true, "ws-a"), auth.ErrApproverRequired},
		{"a plain key in scope", key("k4", false, "ws-a", "ws-b"), auth.ErrApproverRequired},
		{"a key that can't see it", key("k5", true, "ws-c"), ErrNotFound},
	} {
		_, err := s.PauseWorker(c.ctx, "w1", PauseInput{})
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: pause %v, want %v", c.name, err, c.want)
		}
		if err := s.ResumeWorker(c.ctx, "w1", ""); (c.want == nil) != (err == nil) {
			t.Errorf("%s: resume %v", c.name, err)
		}
	}
	p, _ := s.PauseWorker(worker, "w1", PauseInput{})
	if p.By != "key worker-key" {
		t.Fatalf("by: %q", p.By)
	}
}
