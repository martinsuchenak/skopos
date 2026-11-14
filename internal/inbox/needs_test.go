package inbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
)

// The labels an item needs of its worker are set at capture and editable
// while the item is open; once the item is in the agent workflow only an
// approver changes them, and the change is audited.
func TestNeedsEditRules(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item, err := svc.CreateItem(root, CreateInput{WorkspaceID: "ws-a", Title: "stock count", AuthorAgentID: "author", Needs: []string{" Stock", "stock", "plant-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"plant-2", "stock"}; !slices.Equal(item.Needs, want) {
		t.Fatalf("created with %v, want %v", item.Needs, want)
	}
	if _, err := svc.CreateItem(root, CreateInput{WorkspaceID: "ws-a", Title: "x", AuthorAgentID: "author", Needs: []string{"has space"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("an invalid label at capture: %v", err)
	}
	one := []string{"stock"}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Needs: &one}); err != nil {
		t.Fatalf("open item: %v", err)
	}
	got, _ := svc.GetItem(root, item.ID)
	if !slices.Equal(got.Needs, one) {
		t.Fatalf("stored: %v", got.Needs)
	}
	rows, err := svc.ListItems(root, "ws-a", "", "", "")
	if err != nil || len(rows) != 1 || !slices.Equal(rows[0].Needs, one) {
		t.Fatalf("list rows carry the needs: %+v %v", rows, err)
	}
	bad := []string{"-x"}
	if err := svc.UpdateItem(root, item.ID, UpdateInput{Needs: &bad}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid label: %v", err)
	}

	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	none := []string{}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Needs: &none}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("an agent key lifting a workflow item's needs must be refused: %v", err)
	}
	// Re-sending the current list is not an edit.
	same := []string{"STOCK"}
	if err := svc.UpdateItem(agentCtx(), item.ID, UpdateInput{Needs: &same}); err != nil {
		t.Fatalf("unchanged needs: %v", err)
	}
	if err := svc.UpdateItem(approverCtx(), item.ID, UpdateInput{Needs: &none, Via: audit.ViaDashboard}); err != nil {
		t.Fatalf("approver: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); len(got.Needs) != 0 {
		t.Fatalf("cleared: %v", got.Needs)
	}
	entries, err := svc.Timeline(root, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "inbox.edit" && strings.Contains(e.Notes, "needs") {
			n++
			if e.Via != audit.ViaDashboard {
				t.Errorf("via: %+v", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("the change is audited once: %d in %+v", n, entries)
	}
}
