package mcp

import "testing"

func TestPlanUpdateItemToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 4 {
		t.Fatalf("expected at least 4 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
