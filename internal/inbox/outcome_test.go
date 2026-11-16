package inbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// The worker reports what became of an item's branch: stored as the
// newest report, audited when the state changes, never from a run's token.
func TestRecordOutcome(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "ship it")

	if got, err := svc.GetOutcome(root, item.ID); err != nil || got != nil {
		t.Fatalf("nothing reported yet: %+v, %v", got, err)
	}
	run := auth.WithPrincipal(root, &auth.Principal{KeyID: "run:r1", Workspaces: map[string]struct{}{"ws-a": {}}, Run: &auth.RunIdentity{ID: "r1", WorkspaceID: "ws-a"}})
	if _, err := svc.RecordOutcome(run, item.ID, OutcomeInput{State: OutcomeMerged}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("a run's token may not report: %v", err)
	}
	if _, err := svc.RecordOutcome(root, item.ID, OutcomeInput{State: "reverted"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an unknown state is refused: %v", err)
	}
	other := auth.WithPrincipal(root, &auth.Principal{KeyID: "k-other", Workspaces: map[string]struct{}{"ws-b": {}}})
	if _, err := svc.RecordOutcome(other, item.ID, OutcomeInput{State: OutcomeOpen}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a key without the workspace: %v", err)
	}

	worker := auth.WithPrincipal(root, &auth.Principal{KeyID: "k-worker", Workspaces: map[string]struct{}{"ws-a": {}}})
	if _, err := svc.RecordOutcome(worker, item.ID, OutcomeInput{State: OutcomeOpen, PRURL: "https://forge/o/r/pulls/7", Reporter: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	merged := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, err := svc.RecordOutcome(worker, item.ID, OutcomeInput{State: OutcomeMerged, PRURL: "https://forge/o/r/pulls/7", MergeSHA: "abc123", MergedAt: &merged, Reporter: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordOutcome(worker, item.ID, OutcomeInput{State: OutcomeMerged, PRURL: "https://forge/o/r/pulls/7", MergeSHA: "abc123", MergedAt: &merged, Reporter: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetOutcome(root, item.ID)
	if err != nil || got == nil || got.State != OutcomeMerged || got.MergeSHA != "abc123" || got.MergedAt == nil || !got.MergedAt.Equal(merged) || got.ReportedBy != "worker-1" {
		t.Fatalf("the newest report: %+v, %v", got, err)
	}
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "inbox.outcome" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("audited once per change of state (open, merged): %d", n)
	}
}
