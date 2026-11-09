package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// An integration creates and starts an item in one call, links it back to
// its job, and finds it again by the job's reference.
func TestCreateAndStart(t *testing.T) {
	svc, _, _ := fullStack(t)
	q := &stageQueuer{}
	svc.SetRunQueuer(q)
	in := func(start string) CreateInput {
		return CreateInput{WorkspaceID: "ws-a", Title: "job", Content: "do it", AuthorAgentID: "fortix",
			Ref: "WR110375", ExternalURL: "https://fortix.example/wr/110375", Start: start}
	}

	// Refused before the item exists.
	if _, err := svc.CreateItem(agentCtx(), in(StartQueue)); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("non-approver start: %v", err)
	}
	if _, err := svc.CreateItem(approverCtx(), in("later")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown start: %v", err)
	}
	noRef := in(StartReview)
	noRef.Ref = ""
	if _, err := svc.CreateItem(approverCtx(), noRef); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("review without branch or ref: %v", err)
	}
	for _, bad := range []string{"http://fortix.example/x", "javascript:alert(1)", "https://", "https://u:p@host/x", "https://x/" + strings.Repeat("a", maxExternalURL)} {
		b := in("")
		b.ExternalURL = bad
		if _, err := svc.CreateItem(approverCtx(), b); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("external_url %q: %v", bad, err)
		}
	}
	if items, _ := svc.ListItemsFilter(context.Background(), ListFilter{Ref: "WR110375"}); len(items) != 0 {
		t.Fatalf("refused creates left %d items", len(items))
	}

	queued, err := svc.CreateItem(approverCtx(), in(StartQueue))
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != StatusQueued || queued.StartError != "" || queued.ExternalURL != "https://fortix.example/wr/110375" {
		t.Fatalf("queued: %+v", queued)
	}
	if q.last() == "" {
		t.Fatal("no run queued")
	}
	review, err := svc.CreateItem(approverCtx(), in(StartReview))
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != StatusQueued || review.Flow != FlowReview {
		t.Fatalf("review: %+v", review)
	}
	open, err := svc.CreateItem(agentCtx(), in(""))
	if err != nil || open.Status != StatusOpen {
		t.Fatalf("no start: %+v %v", open, err)
	}

	// Found by ref, in any status, within the caller's scope.
	items, err := svc.ListItemsFilter(agentCtx(), ListFilter{Ref: "WR110375"})
	if err != nil || len(items) != 3 {
		t.Fatalf("by ref: %d %v", len(items), err)
	}
	if items, _ := svc.ListItemsFilter(agentCtx(), ListFilter{Ref: "WR1"}); len(items) != 0 {
		t.Fatalf("ref is an exact match: %d", len(items))
	}
	if items, _ := svc.ListItemsFilter(agentCtx(), ListFilter{Ref: "WR110375", Status: "queued"}); len(items) != 2 {
		t.Fatalf("by ref and status: %d", len(items))
	}

	// The link is editable, and clearable.
	other := "https://fortix.example/wr/1"
	if err := svc.UpdateItem(agentCtx(), open.ID, UpdateInput{ExternalURL: &other}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetItem(context.Background(), open.ID)
	if got.ExternalURL != other {
		t.Fatalf("external_url = %q", got.ExternalURL)
	}
	clear := ""
	if err := svc.UpdateItem(agentCtx(), open.ID, UpdateInput{ExternalURL: &clear}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetItem(context.Background(), open.ID); got.ExternalURL != "" {
		t.Fatalf("cleared external_url = %q", got.ExternalURL)
	}
}

// A start that fails after the create still returns the item, open, with
// the reason.
func TestCreateStartFailureKeepsItem(t *testing.T) {
	svc, _, _ := fullStack(t)
	svc.SetRunQueuer(&failingQueuer{})
	item, err := svc.CreateItem(approverCtx(), CreateInput{WorkspaceID: "ws-a", Title: "job", AuthorAgentID: "fortix", Start: StartQueue})
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != StatusOpen || item.StartError == "" {
		t.Fatalf("item: %+v", item)
	}
}

type failingQueuer struct{ stubQueuer }

func (*failingQueuer) QueueStage(context.Context, string, string, string, string, string, string) (string, error) {
	return "", errors.New("queue unavailable")
}
