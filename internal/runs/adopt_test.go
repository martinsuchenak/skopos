package runs

import (
	"context"
	"strings"
	"testing"
)

// A review that finds commits after the implemented one hands back: the
// round in flight ends, an implement run adopts the commits (waiting for
// the worker whose clone holds them when they are local), and after the
// same commit was taken past twice the item waits for a person.
func TestReviewFindsNewCommits(t *testing.T) {
	var moved []string
	s := testService(t)
	s.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		moved = append(moved, to+":"+reason)
		return nil
	})
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Stages: map[string]string{"review": "gemini", "qa": "gemini"}}})
	ctx := rootCtx()

	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	impl, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if _, err := s.Report(ctx, impl.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, HeadSHA: "aaa"}, impl.LeaseToken); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= maxAdoptRounds+1; round++ {
		rv, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-review", Runners: []string{"gemini"}, Workspaces: []string{"ws-a"}})
		if rv == nil || rv.Stage != StageReview || rv.HeadSHA != "aaa" {
			t.Fatalf("round %d: the review is pinned to the implemented head: %+v", round, rv)
		}
		if _, err := s.Report(ctx, rv.ID, ReportInput{WorkerID: "w-review", Status: StatusFailed, Reason: "moved",
			HeadMoved: &HeadMoved{From: "aaa", To: "bbb", Local: true}}, rv.LeaseToken); err != nil {
			t.Fatal(err)
		}
		if round > maxAdoptRounds {
			break
		}
		queued, _ := s.List(ctx, StatusQueued, "item-1")
		if len(queued) != 1 || queued[0].Stage != StageImplement || queued[0].AdoptFrom != "aaa" || queued[0].PreferWorker != "w-review" {
			t.Fatalf("round %d: an adopt run waits for the clone's worker: %+v", round, queued)
		}
		for _, m := range moved {
			if strings.HasPrefix(m, "blocked") || strings.HasPrefix(m, "in_review") {
				t.Fatalf("round %d: the item moved: %v", round, moved)
			}
		}
		// The adopt run found nothing new on its worker: the same head.
		ad, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w-review", Workspaces: []string{"ws-a"}})
		if ad == nil || ad.AdoptFrom != "aaa" {
			t.Fatalf("round %d: the worker claims the adopt run: %+v", round, ad)
		}
		if _, err := s.Report(ctx, ad.ID, ReportInput{WorkerID: "w-review", Status: StatusSucceeded, HeadSHA: "aaa"}, ad.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	if last := moved[len(moved)-1]; !strings.HasPrefix(last, "blocked:") || !strings.Contains(last, "could not be checked") {
		t.Fatalf("the third time the item waits for a person: %v", moved)
	}
}

// Commits pushed to origin: any worker may adopt them, and the new head is
// what review and QA test next.
func TestAdoptedHeadIsReviewedNext(t *testing.T) {
	s := testService(t)
	s.SetItemTransition(func(context.Context, string, string, string, string) error { return nil })
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {Stages: map[string]string{"qa": "gemini"}}})
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	impl, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	_, _ = s.Report(ctx, impl.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, HeadSHA: "aaa"}, impl.LeaseToken)
	qa, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{"gemini"}, Workspaces: []string{"ws-a"}})
	_, _ = s.Report(ctx, qa.ID, ReportInput{WorkerID: "w2", Status: StatusFailed, HeadMoved: &HeadMoved{From: "aaa", To: "ccc"}}, qa.LeaseToken)
	queued, _ := s.List(ctx, StatusQueued, "item-1")
	if len(queued) != 1 || queued[0].AdoptFrom != "aaa" || queued[0].PreferWorker == "w2" {
		t.Fatalf("pushed commits: an adopt run for any worker: %+v", queued)
	}
	// A panel sibling that found the same commits leaves that run alone.
	s.adoptHead(ctx, qa, HeadMoved{From: "aaa", To: "ccc"})
	again, _ := s.List(ctx, StatusQueued, "item-1")
	if len(again) != 1 || again[0].ID != queued[0].ID {
		t.Fatalf("a second report of the same commits keeps the adopt run: %+v", again)
	}
	// The implementing worker is preferred, as for any implement run again.
	ad, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	if ad == nil || ad.AdoptFrom != "aaa" {
		t.Fatalf("the adopt run: %+v", ad)
	}
	_, _ = s.Report(ctx, ad.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, HeadSHA: "ccc"}, ad.LeaseToken)
	if head, _ := s.ImplementedHead(ctx, "item-1"); head != "ccc" {
		t.Fatalf("the adopted head is the item's head: %q", head)
	}
	queued, _ = s.List(ctx, StatusQueued, "item-1")
	if len(queued) != 1 || queued[0].Stage != StageQA || queued[0].HeadSHA != "ccc" {
		t.Fatalf("QA runs again on the adopted head: %+v", queued)
	}
}
