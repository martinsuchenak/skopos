package inbox

import (
	"context"
	"errors"
	"testing"
)

// A claim is released by the key that made it or by an approver — not by
// any key that can write the inbox (strix vuln-0006: release, claim,
// convert took another agent's item).
func TestClaimReleaseNeedsTheClaimingKeyOrAnApprover(t *testing.T) {
	svc := testService(t)
	item := mustCreate(t, svc, context.Background(), "claimed work")

	if _, err := svc.Claim(agentCtx(), item.ID, "victim-agent"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Another key, no approver: refused, and the claim stays.
	if _, err := svc.Claim(scopedCtx(), item.ID, ""); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("another key's release: want ErrClaimConflict, got %v", err)
	}
	if got, _ := svc.GetItem(context.Background(), item.ID); got.Status != StatusInProgress || got.ClaimedByAgentID != "victim-agent" {
		t.Fatalf("the claim moved: %+v", got)
	}
	// The claiming key releases its own claim.
	if got, err := svc.Claim(agentCtx(), item.ID, ""); err != nil || got.Status != StatusOpen {
		t.Fatalf("own release: %+v %v", got, err)
	}
	// An approver releases anyone's (the dashboard's Release).
	if _, err := svc.Claim(agentCtx(), item.ID, "victim-agent"); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Claim(approverCtx(), item.ID, ""); err != nil || got.Status != StatusOpen {
		t.Fatalf("approver release: %+v %v", got, err)
	}
}
